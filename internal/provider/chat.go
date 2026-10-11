package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
)

// chatProvider talks to an OpenAI-compatible Chat Completions API, the one servers such
// as Ollama, vLLM, LM Studio, LiteLLM and OpenRouter share. Unlike OpenAI's own
// Responses API it keeps no state on the server, so the transcript is kept in
// Conversation.Messages as wire-format JSON, append-only like the Anthropic adapter's.
// Effort is not sent: reasoning_effort is OpenAI's own field, and servers that do not
// know it may reject the request.
type chatProvider struct {
	name        string
	completions openai.ChatCompletionService
	models      openai.ModelService
}

// newChatProvider creates the adapter. It does not use openai.NewClient, which adds
// OPENAI_API_KEY, OPENAI_ORG_ID and OPENAI_PROJECT_ID from the environment: those belong
// to OpenAI and must not reach another server.
func newChatProvider(name, baseURL, apiKey string) *chatProvider {
	opts := []option.RequestOption{option.WithHTTPClient(keyClient(0)), option.WithBaseURL(baseURL)}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	return &chatProvider{
		name:        name,
		completions: openai.NewChatCompletionService(opts...),
		models:      openai.NewModelService(opts...),
	}
}

func (p *chatProvider) Name() string {
	return p.name
}

// chatMessage is one message of the stored transcript, in the API's wire format.
type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// unansweredChatCalls returns the tool calls of the transcript's last message when it is
// an assistant message.
func unansweredChatCalls(messages []json.RawMessage) []storedContentBlock {
	if len(messages) == 0 {
		return nil
	}
	var last chatMessage
	if err := json.Unmarshal(messages[len(messages)-1], &last); err != nil || last.Role != "assistant" {
		return nil
	}
	calls := make([]storedContentBlock, 0, len(last.ToolCalls))
	for _, call := range last.ToolCalls {
		calls = append(calls, storedContentBlock{Type: "tool_use", ID: call.ID})
	}
	return calls
}

func chatTools(specs []ToolSpec) []openai.ChatCompletionToolUnionParam {
	tools := make([]openai.ChatCompletionToolUnionParam, 0, len(specs))
	for _, spec := range specs {
		tools = append(tools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        spec.Name,
			Description: openai.String(spec.Description),
			Parameters:  shared.FunctionParameters(spec.Parameters),
		}))
	}
	return tools
}

func chatRequestMessages(system string, transcript []json.RawMessage) []openai.ChatCompletionMessageParamUnion {
	messages := make([]openai.ChatCompletionMessageParamUnion, 0, len(transcript)+1)
	if strings.TrimSpace(system) != "" {
		messages = append(messages, openai.SystemMessage(system))
	}
	for _, message := range transcript {
		messages = append(messages, param.Override[openai.ChatCompletionMessageParamUnion](message))
	}
	return messages
}

// streamedChat is one complete streamed response.
type streamedChat struct {
	id     string
	text   strings.Builder
	calls  []chatToolCall
	finish string
	usage  openai.CompletionUsage
}

// addToolCallDelta adds a piece of a streamed tool call. Calls are told apart by index;
// a new ID at an index that already has one starts another call, for servers that give
// every call index 0. The name arrives whole in the call's first piece.
func (s *streamedChat) addToolCallDelta(delta openai.ChatCompletionChunkChoiceDeltaToolCall) {
	index := max(int(delta.Index), 0)
	for len(s.calls) <= index {
		s.calls = append(s.calls, chatToolCall{Type: "function"})
	}
	call := &s.calls[index]
	if delta.ID != "" && call.ID != "" && delta.ID != call.ID {
		s.calls = append(s.calls, chatToolCall{Type: "function"})
		call = &s.calls[len(s.calls)-1]
	}
	if delta.ID != "" {
		call.ID = delta.ID
	}
	if call.Function.Name == "" {
		call.Function.Name = delta.Function.Name
	}
	call.Function.Arguments += delta.Function.Arguments
}

func (p *chatProvider) stream(ctx context.Context, params openai.ChatCompletionNewParams, onText func(string)) (*streamedChat, bool, error) {
	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
	stream := p.completions.NewStreaming(ctx, params)
	defer stream.Close()
	if err := stream.Err(); err != nil {
		return nil, false, err
	}

	result := &streamedChat{}
	printed := false
	for stream.Next() {
		chunk := stream.Current()
		if result.id == "" {
			result.id = chunk.ID
		}
		if chunk.JSON.Usage.Valid() {
			result.usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			if text := choice.Delta.Content; text != "" {
				result.text.WriteString(text)
				if onText != nil {
					onText(text)
					printed = true
				}
			}
			for _, call := range choice.Delta.ToolCalls {
				result.addToolCallDelta(call)
			}
			if choice.FinishReason != "" {
				result.finish = choice.FinishReason
			}
		}
	}
	if err := stream.Err(); err != nil {
		var apiErr *openai.Error
		if !errors.As(err, &apiErr) && ctx.Err() == nil {
			return nil, printed, fmt.Errorf("%w: %v", errIncompleteStream, err)
		}
		return nil, printed, err
	}
	if result.finish == "" {
		return nil, printed, errIncompleteStream
	}
	return result, printed, nil
}

func chatUsage(usage openai.CompletionUsage) Usage {
	cached := usage.PromptTokensDetails.CachedTokens
	return Usage{
		InputTokens:     usage.PromptTokens - cached,
		CacheReadTokens: cached,
		OutputTokens:    usage.CompletionTokens,
	}
}

func (p *chatProvider) Send(ctx context.Context, req Request, onText func(string)) (*Response, bool, error) {
	history := req.Conversation.Messages
	messages := append([]json.RawMessage(nil), history...)
	for _, result := range toolResultsFor(unansweredChatCalls(history), req.ToolResults) {
		message, err := json.Marshal(chatMessage{Role: "tool", ToolCallID: result.CallID, Content: result.Output})
		if err != nil {
			return nil, false, err
		}
		messages = append(messages, message)
	}
	if req.UserText != "" {
		message, err := json.Marshal(chatMessage{Role: "user", Content: req.UserText})
		if err != nil {
			return nil, false, err
		}
		messages = append(messages, message)
	}

	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(req.Model),
		Messages: chatRequestMessages(req.System, messages),
	}
	if len(req.Tools) > 0 {
		params.Tools = chatTools(req.Tools)
	}
	streamed, printed, err := p.stream(ctx, params, onText)
	if err != nil {
		return nil, printed, err
	}

	out := &Response{ID: streamed.id, Text: streamed.text.String(), Usage: chatUsage(streamed.usage)}
	switch {
	case streamed.finish == "content_filter":
		out.Stop, out.RefusalDetail = StopRefusal, "content filter"
	case len(streamed.calls) > 0:
		// Some servers end a response that calls tools with "stop".
		out.Stop = StopToolUse
	case streamed.finish == "length":
		out.Stop = StopMaxTokens
	case streamed.finish == "stop":
		out.Stop = StopEndTurn
	default:
		out.Stop = StopOther
	}
	if out.Stop == StopRefusal || (out.Text == "" && len(streamed.calls) == 0) {
		// Nothing from this request is kept; the caller still holds the tool results.
		out.Conversation = req.Conversation
		if out.Stop != StopRefusal {
			return nil, printed, fmt.Errorf("the model returned an empty response")
		}
		return out, printed, nil
	}

	for i := range streamed.calls {
		call := &streamed.calls[i]
		if call.ID == "" {
			// Some servers leave out the ID; the tool results need one to answer.
			call.ID = fmt.Sprintf("call_%d_%d", len(messages), i)
		}
		if strings.TrimSpace(call.Function.Arguments) == "" {
			call.Function.Arguments = "{}"
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	assistant, err := json.Marshal(chatMessage{Role: "assistant", Content: out.Text, ToolCalls: streamed.calls})
	if err != nil {
		return nil, printed, err
	}
	out.Conversation = Conversation{Provider: p.name, Messages: append(messages, assistant)}
	return out, printed, nil
}

func (p *chatProvider) Complete(ctx context.Context, model, system, prompt string) (string, error) {
	user, err := json.Marshal(chatMessage{Role: "user", Content: prompt})
	if err != nil {
		return "", err
	}
	streamed, _, err := p.stream(ctx, openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(model),
		Messages: chatRequestMessages(system, []json.RawMessage{user}),
	}, nil)
	if err != nil {
		return "", err
	}
	if streamed.finish == "content_filter" {
		return "", fmt.Errorf("the model declined the request (content filter)")
	}
	return streamed.text.String(), nil
}

// Seed needs no API call: the summary becomes the start of a new transcript.
func (p *chatProvider) Seed(ctx context.Context, model, summary string) (Conversation, error) {
	user, err := json.Marshal(chatMessage{Role: "user", Content: SeedPrompt(summary)})
	if err != nil {
		return Conversation{}, err
	}
	assistant, err := json.Marshal(chatMessage{Role: "assistant", Content: "Context loaded."})
	if err != nil {
		return Conversation{}, err
	}
	return Conversation{Provider: p.name, Messages: []json.RawMessage{user, assistant}}, nil
}

// RetryDelay retries a stream that broke off, and rate limits; the SDK already retries
// other failures that arrive before the stream starts.
func (p *chatProvider) RetryDelay(err error, attempt int) (time.Duration, bool) {
	if errors.Is(err, errIncompleteStream) {
		return fallbackRetryDelay(attempt), true
	}
	return streamRetryDelay(err, attempt)
}

func (p *chatProvider) ListModels(ctx context.Context) ([]string, error) {
	var models []string
	pager := p.models.ListAutoPaging(ctx)
	for pager.Next() {
		models = append(models, pager.Current().ID)
	}
	return models, pager.Err()
}
