package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

const maxStreamRetryDelay = 8 * time.Second

var rateLimitRetryDelayPattern = regexp.MustCompile(`(?i)try again in\s+([0-9]*\.?[0-9]+)s`)

// openAIProvider talks to the OpenAI Responses API. The conversation lives on OpenAI's
// servers; goat only keeps the latest response ID and chains on it.
type openAIProvider struct {
	client *openai.Client
	create OpenAICreateFunc
	stream OpenAIStreamFunc
}

// OpenAICreateFunc makes one Responses API call without streaming.
type OpenAICreateFunc func(ctx context.Context, params responses.ResponseNewParams) (*responses.Response, error)

// OpenAIStreamFunc makes one streamed Responses API call, passing visible text to onText.
type OpenAIStreamFunc func(ctx context.Context, params responses.ResponseNewParams, onText func(string)) (*responses.Response, bool, error)

func NewOpenAI() Provider {
	client := openai.NewClient()
	return &openAIProvider{client: &client}
}

// NewOpenAIWith replaces the API calls, so tests can script responses. A call whose
// function is nil fails rather than reaching the network.
func NewOpenAIWith(create OpenAICreateFunc, stream OpenAIStreamFunc) Provider {
	if create == nil {
		create = func(context.Context, responses.ResponseNewParams) (*responses.Response, error) {
			return nil, errors.New("no scripted OpenAI response")
		}
	}
	if stream == nil {
		stream = func(context.Context, responses.ResponseNewParams, func(string)) (*responses.Response, bool, error) {
			return nil, false, errors.New("no scripted OpenAI stream")
		}
	}
	return &openAIProvider{create: create, stream: stream}
}

func (p *openAIProvider) Name() string {
	return OpenAI
}

func (p *openAIProvider) createResponse(ctx context.Context, params responses.ResponseNewParams) (*responses.Response, error) {
	if p.create != nil {
		return p.create(ctx, params)
	}
	return p.client.Responses.New(ctx, params)
}

func (p *openAIProvider) createStreamedResponse(ctx context.Context, params responses.ResponseNewParams, onText func(string)) (*responses.Response, bool, error) {
	if p.stream != nil {
		return p.stream(ctx, params, onText)
	}
	return streamResponse(ctx, p.client, params, onText)
}

func streamResponse(ctx context.Context, c *openai.Client, params responses.ResponseNewParams, onText func(string)) (*responses.Response, bool, error) {
	stream := c.Responses.NewStreaming(ctx, params)
	defer stream.Close()

	var (
		finalResponse *responses.Response
		printedText   bool
	)

	for stream.Next() {
		switch event := stream.Current().AsAny().(type) {
		case responses.ResponseTextDeltaEvent:
			onText(event.Delta)
			printedText = true
		case responses.ResponseRefusalDeltaEvent:
			onText(event.Delta)
			printedText = true
		case responses.ResponseCompletedEvent:
			response := event.Response
			finalResponse = &response
		case responses.ResponseIncompleteEvent:
			response := event.Response
			finalResponse = &response
		case responses.ResponseFailedEvent:
			return nil, printedText, &openAIStreamError{Code: string(event.Response.Error.Code), Message: event.Response.Error.Message}
		case responses.ResponseErrorEvent:
			return nil, printedText, &openAIStreamError{Code: event.Code, Message: event.Message}
		}
	}

	if err := stream.Err(); err != nil {
		return nil, printedText, err
	}
	if finalResponse == nil {
		return nil, printedText, fmt.Errorf("stream completed without a final response")
	}

	return finalResponse, printedText, nil
}

// openAIReasoningEffort maps goat's effort to the Responses API, which has no "max".
func openAIReasoningEffort(level Effort) shared.ReasoningEffort {
	if level == EffortMax {
		return shared.ReasoningEffortXhigh
	}
	return shared.ReasoningEffort(level)
}

// openAISupportsReasoning is false for model families that reject reasoning settings.
func openAISupportsReasoning(model string) bool {
	lower := strings.ToLower(model)
	return !strings.HasPrefix(lower, "gpt-4") && !strings.HasPrefix(lower, "chatgpt-")
}

func newPlainResponseParams(model string, effort Effort, input responses.ResponseNewParamsInputUnion, previousResponseID, instructions string) responses.ResponseNewParams {
	params := responses.ResponseNewParams{
		Model: model,
		Input: input,
	}
	if openAISupportsReasoning(model) {
		params.Reasoning = shared.ReasoningParam{Effort: openAIReasoningEffort(effort)}
	}
	if strings.TrimSpace(instructions) != "" {
		params.Instructions = openai.String(instructions)
	}
	if strings.TrimSpace(previousResponseID) != "" {
		params.PreviousResponseID = openai.String(previousResponseID)
	}

	return params
}

func openAITools(specs []ToolSpec) []responses.ToolUnionParam {
	tools := make([]responses.ToolUnionParam, 0, len(specs))
	for _, spec := range specs {
		tools = append(tools, responses.ToolUnionParam{
			OfFunction: &responses.FunctionToolParam{
				Name:        spec.Name,
				Description: openai.String(spec.Description),
				Parameters:  spec.Parameters,
			},
		})
	}
	return tools
}

func openAIInput(req Request) responses.ResponseNewParamsInputUnion {
	if len(req.ToolResults) == 0 {
		return responses.ResponseNewParamsInputUnion{OfString: openai.String(req.UserText)}
	}

	items := make([]responses.ResponseInputItemUnionParam, 0, len(req.ToolResults)+1)
	for _, result := range req.ToolResults {
		items = append(items, responses.ResponseInputItemUnionParam{
			OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
				CallID: result.CallID,
				Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
					OfString: openai.String(result.Output),
				},
			},
		})
	}
	if req.UserText != "" {
		items = append(items, responses.ResponseInputItemParamOfMessage(req.UserText, responses.EasyInputMessageRoleUser))
	}
	return responses.ResponseNewParamsInputUnion{OfInputItemList: items}
}

func (p *openAIProvider) requestParams(req Request) responses.ResponseNewParams {
	params := newPlainResponseParams(req.Model, req.Effort, openAIInput(req), req.Conversation.ResponseID, req.System)
	params.Tools = openAITools(req.Tools)
	return params
}

func (p *openAIProvider) Send(ctx context.Context, req Request, onText func(string)) (*Response, bool, error) {
	resp, printed, err := p.createStreamedResponse(ctx, p.requestParams(req), onText)
	if err != nil {
		return nil, printed, err
	}
	return convertOpenAIResponse(resp, req.Conversation), printed, nil
}

func convertOpenAIResponse(resp *responses.Response, previous Conversation) *Response {
	out := &Response{
		ID:           resp.ID,
		Conversation: Conversation{Provider: OpenAI, ResponseID: resp.ID},
		Text:         resp.OutputText(),
		Usage: Usage{
			InputTokens:     resp.Usage.InputTokens - resp.Usage.InputTokensDetails.CachedTokens,
			CacheReadTokens: resp.Usage.InputTokensDetails.CachedTokens,
			OutputTokens:    resp.Usage.OutputTokens,
		},
	}

	var refusals []string
	for _, item := range resp.Output {
		switch item.Type {
		case "function_call":
			call := item.AsFunctionCall()
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: call.CallID, Name: call.Name, Arguments: call.Arguments})
		case "message":
			for _, part := range item.AsMessage().Content {
				if part.Type == "refusal" && strings.TrimSpace(part.Refusal) != "" {
					refusals = append(refusals, part.Refusal)
				}
			}
		}
	}

	switch {
	case resp.Status == responses.ResponseStatusIncomplete && resp.IncompleteDetails.Reason == "max_output_tokens":
		out.Stop = StopMaxTokens
	case resp.Status == responses.ResponseStatusIncomplete && resp.IncompleteDetails.Reason == "content_filter":
		out.Stop = StopRefusal
		out.RefusalDetail = "content filter"
	case len(refusals) > 0:
		out.Stop = StopRefusal
		out.RefusalDetail = strings.Join(refusals, " ")
	case resp.Status == responses.ResponseStatusIncomplete:
		out.Stop = StopOther
	case len(out.ToolCalls) > 0:
		out.Stop = StopToolUse
	default:
		out.Stop = StopEndTurn
	}

	if out.Stop == StopRefusal {
		out.Conversation = previous
	}
	return out
}

func (p *openAIProvider) Complete(ctx context.Context, model, system, prompt string) (string, error) {
	resp, err := p.createResponse(ctx, newPlainResponseParams(
		model,
		EffortLow,
		responses.ResponseNewParamsInputUnion{OfString: openai.String(prompt)},
		"",
		system,
	))
	if err != nil {
		return "", err
	}
	return resp.OutputText(), nil
}

func (p *openAIProvider) Seed(ctx context.Context, model, summary string) (Conversation, error) {
	seedResp, err := p.createResponse(ctx, newPlainResponseParams(
		model,
		EffortLow,
		responses.ResponseNewParamsInputUnion{OfString: openai.String(SeedPrompt(summary))},
		"",
		"You are loading a compacted session summary into a fresh conversation. Reply with exactly: Context loaded.",
	))
	if err != nil {
		return Conversation{}, err
	}
	if strings.TrimSpace(seedResp.ID) == "" {
		return Conversation{}, fmt.Errorf("compaction seed response did not return an id")
	}
	return Conversation{Provider: OpenAI, ResponseID: seedResp.ID}, nil
}

func (p *openAIProvider) ListModels(ctx context.Context) ([]string, error) {
	var models []string
	pager := p.client.Models.ListAutoPaging(ctx)
	for pager.Next() {
		if id := pager.Current().ID; isOpenAIChatModel(id) {
			models = append(models, id)
		}
	}
	return models, pager.Err()
}

// openAIStreamError is an error the API reported inside an open stream.
type openAIStreamError struct {
	Code    string
	Message string
}

func (e *openAIStreamError) Error() string {
	return fmt.Sprintf("response error: %s: %s", e.Code, e.Message)
}

type responseStreamErrorPayload struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func retryDelayFromHeaders(headers http.Header) (time.Duration, bool) {
	if retryAfterMS := strings.TrimSpace(headers.Get("Retry-After-Ms")); retryAfterMS != "" {
		ms, err := strconv.ParseFloat(retryAfterMS, 64)
		if err == nil && ms >= 0 {
			return time.Duration(ms * float64(time.Millisecond)), true
		}
	}

	if retryAfter := strings.TrimSpace(headers.Get("Retry-After")); retryAfter != "" {
		seconds, err := strconv.ParseFloat(retryAfter, 64)
		if err == nil && seconds >= 0 {
			return time.Duration(seconds * float64(time.Second)), true
		}

		retryAt, err := http.ParseTime(retryAfter)
		if err == nil {
			delay := time.Until(retryAt)
			if delay < 0 {
				return 0, true
			}
			return delay, true
		}
	}

	return 0, false
}

func retryDelayFromMessage(message string) (time.Duration, bool) {
	matches := rateLimitRetryDelayPattern.FindStringSubmatch(message)
	if len(matches) != 2 {
		return 0, false
	}

	seconds, err := strconv.ParseFloat(matches[1], 64)
	if err != nil || seconds < 0 {
		return 0, false
	}

	return time.Duration(seconds * float64(time.Second)), true
}

func fallbackRetryDelay(attempt int) time.Duration {
	delay := time.Second << attempt
	if delay > maxStreamRetryDelay {
		return maxStreamRetryDelay
	}
	return delay
}

func (p *openAIProvider) RetryDelay(err error, attempt int) (time.Duration, bool) {
	return streamRetryDelay(err, attempt)
}

func streamRetryDelay(err error, attempt int) (time.Duration, bool) {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests {
		if apiErr.Response != nil {
			if delay, ok := retryDelayFromHeaders(apiErr.Response.Header); ok {
				return delay, true
			}
		}
		if delay, ok := retryDelayFromMessage(apiErr.Message); ok {
			return delay, true
		}
		return fallbackRetryDelay(attempt), true
	}

	var eventErr *openAIStreamError
	if errors.As(err, &eventErr) && eventErr.Code == "rate_limit_exceeded" {
		if delay, ok := retryDelayFromMessage(eventErr.Message); ok {
			return delay, true
		}
		return fallbackRetryDelay(attempt), true
	}

	var streamErr *ssestream.StreamError
	if errors.As(err, &streamErr) {
		var payload responseStreamErrorPayload
		if json.Unmarshal(streamErr.Event.Data, &payload) == nil && payload.Error.Code == "rate_limit_exceeded" {
			if delay, ok := retryDelayFromMessage(payload.Error.Message); ok {
				return delay, true
			}
			return fallbackRetryDelay(attempt), true
		}
	}

	return 0, false
}
