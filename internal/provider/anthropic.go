package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

const (
	// anthropicHeaderTimeout is the Anthropic SDK's own bound on waiting for a response
	// to start, kept now that goat gives the SDK its client.
	anthropicHeaderTimeout = 10 * time.Minute

	anthropicMaxTokens       = 64000
	anthropicLegacyMaxTokens = 32000
)

// errIncompleteStream means the stream ended before message_stop. The partial message
// is never stored, since a cut-off thinking block would make every later request fail.
var errIncompleteStream = errors.New("the response stream ended before the message was complete")

// anthropicProvider talks to the Anthropic Messages API. The API is stateless, so the
// transcript is kept in Conversation.Messages as wire-format JSON. It is only ever
// appended to: thinking blocks are only valid in the exact conversation that produced
// them.
type anthropicProvider struct {
	// name is the provider this adapter serves; empty means Anthropic itself.
	name          string
	client        anthropic.Client
	customBaseURL bool
	fallbacks     bool
	// cacheTTL is the lifetime of the prompt cache entries goat writes: empty for the
	// API's default of 5 minutes, or "1h".
	cacheTTL anthropic.BetaCacheControlEphemeralTTL
	// deepseek is set when the adapter talks to DeepSeek's Anthropic-compatible API.
	deepseek *deepSeekSettings
	// entry is set for an Anthropic-compatible entry from config.json.
	entry bool

	mu           sync.Mutex
	capabilities map[string]anthropicModelCapabilities
}

type anthropicModelCapabilities struct {
	adaptiveThinking bool
	efforts          map[Effort]bool
	maxTokens        int64
}

// NewAnthropicWith talks to the endpoint the client options name instead of the one in
// the environment, as tests do. official makes it behave as it does against Anthropic's
// own API, with fallbacks and automatic caching on;
// otherwise the transcript's cache breakpoint is placed by hand.
func NewAnthropicWith(official bool, opts ...option.RequestOption) Provider {
	return &anthropicProvider{
		client:        anthropic.NewClient(opts...),
		customBaseURL: !official,
		fallbacks:     true,
		capabilities:  map[string]anthropicModelCapabilities{},
	}
}

func NewAnthropic() Provider {
	// goat names the endpoint and credential itself, so the client sends the key only
	// where ResolveEndpoint has checked. The SDK's own environment defaults are left out:
	// with the key in auth.json rather than the environment, they would go on to add an
	// `ant auth login` profile's workspace, ANTHROPIC_CUSTOM_HEADERS and a warning. The
	// API key comes before the token, as in the SDK.
	endpoint := endpointOrOfficial(Anthropic)
	opts := []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithHTTPClient(keyClient(anthropicHeaderTimeout)), option.WithBaseURL(endpoint.URL)}
	if _, credential, authToken := Credential(Anthropic); credential != "" && authToken {
		opts = append(opts, option.WithAuthToken(credential))
	} else if credential != "" {
		opts = append(opts, option.WithAPIKey(credential))
	}
	return &anthropicProvider{
		client:        anthropic.NewClient(opts...),
		customBaseURL: endpoint.Custom,
		fallbacks:     !envFlagOff("GOAT_ANTHROPIC_FALLBACKS"),
		cacheTTL:      anthropicCacheTTL(os.Getenv("GOAT_ANTHROPIC_CACHE_TTL")),
		capabilities:  map[string]anthropicModelCapabilities{},
	}
}

// newAnthropicEntry talks to an Anthropic-compatible entry. Like DeepSeek it ignores
// the ANTHROPIC_* environment, so Anthropic's key never goes there. The key goes in both
// x-api-key and a bearer Authorization header: compatible servers differ in which one
// they read, and both go to the same server.
func newAnthropicEntry(name, baseURL, key string) *anthropicProvider {
	opts := []option.RequestOption{
		option.WithoutEnvironmentDefaults(),
		option.WithHTTPClient(keyClient(anthropicHeaderTimeout)),
		option.WithBaseURL(baseURL),
	}
	if key != "" {
		opts = append(opts, option.WithAPIKey(key), option.WithAuthToken(key))
	}
	return &anthropicProvider{
		name:          name,
		client:        anthropic.NewClient(opts...),
		customBaseURL: true,
		fallbacks:     false,
		entry:         true,
		capabilities:  map[string]anthropicModelCapabilities{},
	}
}

// entryModelCapabilities are assumed for a model an entry serves that is not a Claude
// model: no thinking or effort settings, which other servers may reject, and a modest
// output limit.
var entryModelCapabilities = anthropicModelCapabilities{maxTokens: anthropicLegacyMaxTokens}

// anthropicCacheTTL reads GOAT_ANTHROPIC_CACHE_TTL. The default 5-minute entries cost
// 1.25x to write and stay warm as long as requests keep coming; "1h" entries cost 2x
// to write but survive the user stepping away between prompts.
func anthropicCacheTTL(raw string) anthropic.BetaCacheControlEphemeralTTL {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1h", "60m":
		return anthropic.BetaCacheControlEphemeralTTLTTL1h
	case "", "5m":
		return ""
	default:
		Warnf("GOAT_ANTHROPIC_CACHE_TTL=%q is not 5m or 1h; using 5m", raw)
		return ""
	}
}

func (p *anthropicProvider) cacheControl() anthropic.BetaCacheControlEphemeralParam {
	control := anthropic.NewBetaCacheControlEphemeralParam()
	control.TTL = p.cacheTTL
	return control
}

func envFlagOff(name string) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if value == "off" || value == "no" {
		return true
	}
	enabled, err := strconv.ParseBool(value)
	return err == nil && !enabled
}

func (p *anthropicProvider) Name() string {
	if p.name != "" {
		return p.name
	}
	return Anthropic
}

// anthropicLegacyModel reports models that predate adaptive thinking and the effort
// setting. Prefixes also match dated IDs such as claude-haiku-4-5-20251001.
func anthropicLegacyModel(model string) bool {
	for _, prefix := range []string{"claude-3", "claude-haiku-4-5", "claude-sonnet-4-5", "claude-opus-4-5", "claude-opus-4-1", "claude-opus-4-2025", "claude-sonnet-4-2025"} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return model == "claude-opus-4-0" || model == "claude-sonnet-4-0"
}

// guessAnthropicCapabilities is used when the Models API cannot be reached.
func guessAnthropicCapabilities(model string) anthropicModelCapabilities {
	if anthropicLegacyModel(model) {
		return anthropicModelCapabilities{maxTokens: anthropicLegacyMaxTokens}
	}
	efforts := map[Effort]bool{EffortLow: true, EffortMedium: true, EffortHigh: true, EffortXhigh: true, EffortMax: true}
	if strings.HasPrefix(model, "claude-opus-4-6") || strings.HasPrefix(model, "claude-sonnet-4-6") {
		delete(efforts, EffortXhigh)
	}
	return anthropicModelCapabilities{adaptiveThinking: true, efforts: efforts, maxTokens: anthropicMaxTokens}
}

// modelCapabilities asks the Models API what a model supports, once per model.
func (p *anthropicProvider) modelCapabilities(ctx context.Context, model string) anthropicModelCapabilities {
	p.mu.Lock()
	caps, ok := p.capabilities[model]
	p.mu.Unlock()
	if ok {
		return caps
	}

	if p.entry && !strings.HasPrefix(strings.ToLower(model), "claude-") {
		caps = entryModelCapabilities
	} else if p.deepseek != nil {
		var listed bool
		caps, listed = p.deepSeekCapabilities(ctx, model)
		if !listed {
			// Try the model list again next time rather than remembering a guess.
			return caps
		}
	} else if info, err := p.client.Models.Get(ctx, model, anthropic.ModelGetParams{}); err == nil {
		caps = anthropicModelCapabilities{
			adaptiveThinking: info.Capabilities.Thinking.Types.Adaptive.Supported,
			efforts: map[Effort]bool{
				EffortLow:    info.Capabilities.Effort.Low.Supported,
				EffortMedium: info.Capabilities.Effort.Medium.Supported,
				EffortHigh:   info.Capabilities.Effort.High.Supported,
				EffortXhigh:  info.Capabilities.Effort.Xhigh.Supported,
				EffortMax:    info.Capabilities.Effort.Max.Supported,
			},
			maxTokens: min(int64(anthropicMaxTokens), info.MaxTokens),
		}
		if caps.maxTokens <= 0 {
			caps.maxTokens = anthropicMaxTokens
		}
	} else {
		caps = guessAnthropicCapabilities(model)
		// Do not remember a guess made because the request was cancelled.
		if ctx.Err() != nil {
			return caps
		}
	}

	p.mu.Lock()
	p.capabilities[model] = caps
	p.mu.Unlock()
	return caps
}

// anthropicEffort picks the requested effort, or the closest lower level the model
// supports. ok is false when the model takes no effort setting at all.
func anthropicEffort(level Effort, caps anthropicModelCapabilities) (anthropic.BetaOutputConfigEffort, bool) {
	order := []Effort{EffortLow, EffortMedium, EffortHigh, EffortXhigh, EffortMax}
	if level == EffortNone || level == EffortMinimal {
		level = EffortLow
	}
	start := len(order) - 1
	for i, candidate := range order {
		if candidate == level {
			start = i
		}
	}
	for i := start; i >= 0; i-- {
		if caps.efforts[order[i]] {
			return anthropic.BetaOutputConfigEffort(order[i]), true
		}
	}
	for _, candidate := range order {
		if caps.efforts[candidate] {
			return anthropic.BetaOutputConfigEffort(candidate), true
		}
	}
	return "", false
}

// anthropicFallbackModels accept fallbacks:"default", which re-runs a request that a
// safety classifier declines on the model Anthropic recommends for that category.
var anthropicFallbackModels = map[string]bool{"claude-fable-5": true, "claude-fable-5-1": true, "claude-opus-5": true, "claude-opus-5-5": true, "claude-sonnet-5-5": true}

func (p *anthropicProvider) useFallbacks(model string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fallbacks && !p.customBaseURL && anthropicFallbackModels[model]
}

func (p *anthropicProvider) disableFallbacks() {
	p.mu.Lock()
	p.fallbacks = false
	p.mu.Unlock()
}

// baseParams builds a request. cached marks it as part of a conversation whose prefix
// later requests repeat: tools and system get their own cache breakpoint, which still
// hits after an /effort change invalidates the cached messages, and on Anthropic's own
// API automatic caching moves a second breakpoint to the end of the growing transcript
// (Send places it by hand elsewhere). One-off requests are not cached, since writing a
// cache entry costs more than plain input and nothing would read it back.
func (p *anthropicProvider) baseParams(ctx context.Context, model, system string, effort Effort, cached bool) anthropic.BetaMessageNewParams {
	caps := p.modelCapabilities(ctx, model)
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: caps.maxTokens,
	}
	if strings.TrimSpace(system) != "" {
		params.System = []anthropic.BetaTextBlockParam{{Text: system}}
		if cached {
			params.System[0].CacheControl = p.cacheControl()
		}
	}
	if cached && !p.customBaseURL {
		params.CacheControl = p.cacheControl()
	}
	if p.deepseek != nil {
		applyDeepSeekReasoning(&params, effort)
		params.MaxTokens = deepSeekMaxTokens(effort, caps)
		return params
	}
	if caps.adaptiveThinking {
		params.Thinking = anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{}}
	}
	if level, ok := anthropicEffort(effort, caps); ok {
		params.OutputConfig = anthropic.BetaOutputConfigParam{Effort: level}
	}
	if p.useFallbacks(model) {
		params.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
		params.Betas = append(params.Betas, anthropic.AnthropicBetaServerSideFallback2026_07_01)
	}
	return params
}

func anthropicTools(specs []ToolSpec) []anthropic.BetaToolUnionParam {
	tools := make([]anthropic.BetaToolUnionParam, 0, len(specs))
	for _, spec := range specs {
		required, _ := spec.Parameters["required"].([]string)
		tools = append(tools, anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
			Name:        spec.Name,
			Description: anthropic.String(spec.Description),
			InputSchema: anthropic.BetaToolInputSchemaParam{
				Properties: spec.Parameters["properties"],
				Required:   required,
			},
		}})
	}
	return tools
}

type storedContentBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type storedMessage struct {
	Role    string               `json:"role"`
	Content []storedContentBlock `json:"content"`
}

// unansweredToolUses returns the tool_use IDs of the transcript's last message when it
// is an assistant message, in order.
func unansweredToolUses(messages []json.RawMessage) []storedContentBlock {
	if len(messages) == 0 {
		return nil
	}
	var last storedMessage
	if err := json.Unmarshal(messages[len(messages)-1], &last); err != nil || last.Role != "assistant" {
		return nil
	}
	var uses []storedContentBlock
	for _, block := range last.Content {
		if block.Type == "tool_use" {
			uses = append(uses, block)
		}
	}
	return uses
}

// toolResultsFor answers exactly the pending tool uses: results for other calls are
// dropped (the API rejects them) and unanswered calls get an error result (the API
// rejects a tool_use without one).
func toolResultsFor(uses []storedContentBlock, results []ToolResult) []ToolResult {
	byID := map[string]ToolResult{}
	for _, result := range results {
		byID[result.CallID] = result
	}
	answered := make([]ToolResult, 0, len(uses))
	for _, use := range uses {
		if result, ok := byID[use.ID]; ok {
			answered = append(answered, result)
			continue
		}
		answered = append(answered, ToolResult{CallID: use.ID, Output: "This tool call was not run because the session changed before its result was delivered.", IsError: true})
	}
	return answered
}

// newAnthropicUserMessage builds the next user message. When cacheControl is set it
// marks the message's last block as a cache breakpoint.
func newAnthropicUserMessage(results []ToolResult, text string, cacheControl *anthropic.BetaCacheControlEphemeralParam) (json.RawMessage, error) {
	var blocks []anthropic.BetaContentBlockParamUnion
	for _, result := range results {
		blocks = append(blocks, anthropic.NewBetaToolResultBlock(result.CallID, result.Output, result.IsError))
	}
	if text != "" {
		blocks = append(blocks, anthropic.NewBetaTextBlock(text))
	}
	if len(blocks) == 0 {
		return nil, nil
	}
	if cacheControl != nil {
		if control := blocks[len(blocks)-1].GetCacheControl(); control != nil {
			*control = *cacheControl
		}
	}
	return json.Marshal(anthropic.NewBetaUserMessage(blocks...))
}

func replayMessages(raw []json.RawMessage) []anthropic.BetaMessageParam {
	messages := make([]anthropic.BetaMessageParam, len(raw))
	for i, message := range raw {
		messages[i] = param.Override[anthropic.BetaMessageParam](message)
	}
	return messages
}

// streamedMessage is one complete streamed response.
type streamedMessage struct {
	message anthropic.BetaMessage
	// toolInputs holds each tool_use block's input exactly as streamed, by block index.
	toolInputs map[int64]*strings.Builder
}

func (p *anthropicProvider) stream(ctx context.Context, params anthropic.BetaMessageNewParams, onText func(string)) (*streamedMessage, bool, error) {
	stream := p.client.Beta.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	// Errors present before reading come from the request itself, which the SDK has
	// already retried; they are passed on as they are.
	if err := stream.Err(); err != nil {
		return nil, false, err
	}

	result := &streamedMessage{toolInputs: map[int64]*strings.Builder{}}
	printed := false
	sawStop := false
	for stream.Next() {
		event := stream.Current()
		if err := result.message.Accumulate(event); err != nil {
			return nil, printed, err
		}
		switch variant := event.AsAny().(type) {
		case anthropic.BetaRawContentBlockDeltaEvent:
			switch delta := variant.Delta.AsAny().(type) {
			case anthropic.BetaTextDelta:
				if onText != nil && delta.Text != "" {
					onText(delta.Text)
					printed = true
				}
			case anthropic.BetaInputJSONDelta:
				builder, ok := result.toolInputs[variant.Index]
				if !ok {
					builder = &strings.Builder{}
					result.toolInputs[variant.Index] = builder
				}
				builder.WriteString(delta.PartialJSON)
			}
		case anthropic.BetaRawMessageStopEvent:
			sawStop = true
		}
	}
	if err := stream.Err(); err != nil {
		// A connection that drops while the response is being read is not retried by the
		// SDK; treat it like any other unfinished stream.
		var apiErr *anthropic.Error
		if !errors.As(err, &apiErr) && ctx.Err() == nil {
			return nil, printed, fmt.Errorf("%w: %v", errIncompleteStream, err)
		}
		return nil, printed, err
	}
	if !sawStop || result.message.StopReason == "" {
		return nil, printed, errIncompleteStream
	}
	if reason := string(result.message.StopReason); reason == "insufficient_system_resource" || reason == "aborted" {
		// DeepSeek ends a response this way when it runs out of capacity.
		return nil, printed, fmt.Errorf("%w: the server stopped the response (%s)", errIncompleteStream, reason)
	}
	return result, printed, nil
}

// streamWithFallbackRetry retries once without fallbacks if the API rejects them, and
// stops sending them for the rest of the session.
func (p *anthropicProvider) streamWithFallbackRetry(ctx context.Context, params anthropic.BetaMessageNewParams, onText func(string)) (*streamedMessage, bool, error) {
	result, printed, err := p.stream(ctx, params, onText)
	if err == nil || printed || len(params.Betas) == 0 || !isFallbacksRejection(err) {
		return result, printed, err
	}

	p.disableFallbacks()
	params.Fallbacks = anthropic.BetaFallbacksParamUnion{}
	params.Betas = removeBeta(params.Betas, anthropic.AnthropicBetaServerSideFallback2026_07_01)
	return p.stream(ctx, params, onText)
}

func isFallbacksRejection(err error) bool {
	var apiErr *anthropic.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(apiErr.Error()), "fallback")
}

func removeBeta(betas []anthropic.AnthropicBeta, remove anthropic.AnthropicBeta) []anthropic.AnthropicBeta {
	var kept []anthropic.AnthropicBeta
	for _, beta := range betas {
		if beta != remove {
			kept = append(kept, beta)
		}
	}
	return kept
}

// servedBlocks splits a response at its last fallback block. When a classifier
// declined part way through and another model took over, only the text of the declined
// part may be sent back; its thinking and tool calls were never seen by the model that
// finished the response, and its tool calls must never run.
func servedBlocks(message anthropic.BetaMessage) (replay []anthropic.BetaContentBlockParamUnion, served []anthropic.BetaContentBlockUnion) {
	lastFallback := -1
	for i, block := range message.Content {
		if block.Type == "fallback" {
			lastFallback = i
		}
	}
	for i, block := range message.Content {
		if i < lastFallback && block.Type != "text" {
			continue
		}
		replay = append(replay, block.ToParam())
		if i > lastFallback {
			served = append(served, block)
		}
	}
	return replay, served
}

func anthropicStop(message anthropic.BetaMessage) (StopReason, string) {
	switch message.StopReason {
	case anthropic.BetaStopReasonEndTurn, anthropic.BetaStopReasonStopSequence:
		return StopEndTurn, ""
	case anthropic.BetaStopReasonToolUse:
		return StopToolUse, ""
	case anthropic.BetaStopReasonMaxTokens:
		return StopMaxTokens, ""
	case anthropic.BetaStopReasonModelContextWindowExceeded:
		return StopContextExceeded, ""
	case anthropic.BetaStopReasonRefusal:
		detail := strings.TrimSpace(strings.TrimSpace(string(message.StopDetails.Category)) + " " + strings.TrimSpace(message.StopDetails.Explanation))
		return StopRefusal, detail
	default:
		return StopOther, string(message.StopReason)
	}
}

func anthropicUsage(usage anthropic.BetaUsage) Usage {
	return Usage{
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		CacheReadTokens:  usage.CacheReadInputTokens,
		CacheWriteTokens: usage.CacheCreationInputTokens,
	}
}

func (p *anthropicProvider) Send(ctx context.Context, req Request, onText func(string)) (*Response, bool, error) {
	history := req.Conversation.Messages
	results := toolResultsFor(unansweredToolUses(history), req.ToolResults)
	userMessage, err := newAnthropicUserMessage(results, req.UserText, nil)
	if err != nil {
		return nil, false, err
	}

	messages := append([]json.RawMessage(nil), history...)
	if userMessage != nil {
		messages = append(messages, userMessage)
	}

	params := p.baseParams(ctx, req.Model, req.System, req.Effort, true)
	params.Tools = anthropicTools(req.Tools)
	params.Messages = replayMessages(messages)
	if p.customBaseURL && p.deepseek == nil && userMessage != nil {
		// Without automatic caching the transcript breakpoint goes on the new message by
		// hand. Only the request carries it: a marker kept in the stored transcript would
		// stay behind on every later request and use up the four allowed.
		control := p.cacheControl()
		marked, err := newAnthropicUserMessage(results, req.UserText, &control)
		if err != nil {
			return nil, false, err
		}
		params.Messages[len(params.Messages)-1] = param.Override[anthropic.BetaMessageParam](marked)
	}

	streamed, printed, err := p.streamWithFallbackRetry(ctx, params, onText)
	if err != nil {
		return nil, printed, err
	}

	message := streamed.message
	stop, detail := anthropicStop(message)
	out := &Response{
		ID:            message.ID,
		Usage:         anthropicUsage(message.Usage),
		Stop:          stop,
		RefusalDetail: detail,
	}
	if stop == StopRefusal || len(message.Content) == 0 {
		// Nothing from this request is kept; the caller still holds the tool results.
		out.Conversation = req.Conversation
		if stop != StopRefusal {
			return nil, printed, fmt.Errorf("the model returned an empty response")
		}
		return out, printed, nil
	}

	replay, served := servedBlocks(message)
	for i, block := range served {
		switch block.Type {
		case "text":
			out.Text += block.AsText().Text
		case "tool_use":
			use := block.AsToolUse()
			arguments := use.JSON.Input.Raw()
			index := int64(len(message.Content) - len(served) + i)
			if streamedInput, ok := streamed.toolInputs[index]; ok && streamedInput.Len() > 0 {
				arguments = streamedInput.String()
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: use.ID, Name: use.Name, Arguments: arguments})
		}
	}

	assistant, err := json.Marshal(anthropic.BetaMessageParam{Role: anthropic.BetaMessageParamRoleAssistant, Content: replay})
	if err != nil {
		return nil, printed, err
	}
	out.Conversation = Conversation{Provider: p.Name(), Messages: append(messages, assistant)}
	return out, printed, nil
}

func (p *anthropicProvider) Complete(ctx context.Context, model, system, prompt string) (string, error) {
	params := p.baseParams(ctx, model, system, EffortLow, false)
	params.Messages = []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(prompt))}

	streamed, _, err := p.streamWithFallbackRetry(ctx, params, nil)
	if err != nil {
		return "", err
	}
	if stop, detail := anthropicStop(streamed.message); stop == StopRefusal {
		return "", fmt.Errorf("the model declined the request (%s)", detail)
	}

	var text strings.Builder
	_, served := servedBlocks(streamed.message)
	for _, block := range served {
		if block.Type == "text" {
			text.WriteString(block.AsText().Text)
		}
	}
	return text.String(), nil
}

// Seed needs no API call: the summary becomes the start of a new transcript.
func (p *anthropicProvider) Seed(ctx context.Context, model, summary string) (Conversation, error) {
	user, err := json.Marshal(anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(SeedPrompt(summary))))
	if err != nil {
		return Conversation{}, err
	}
	assistant, err := json.Marshal(anthropic.BetaMessageParam{
		Role:    anthropic.BetaMessageParamRoleAssistant,
		Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock("Context loaded.")},
	})
	if err != nil {
		return Conversation{}, err
	}
	return Conversation{Provider: p.Name(), Messages: []json.RawMessage{user, assistant}}, nil
}

// RetryDelay only covers failures inside an open stream. The SDK already retries
// rate limits and server errors that arrive before the stream starts.
func (p *anthropicProvider) RetryDelay(err error, attempt int) (time.Duration, bool) {
	if errors.Is(err, errIncompleteStream) {
		return fallbackRetryDelay(attempt), true
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusOK {
		switch apiErr.Type() {
		case anthropic.ErrorTypeOverloadedError, anthropic.ErrorTypeRateLimitError, anthropic.ErrorTypeAPIError:
			return fallbackRetryDelay(attempt), true
		}
	}
	return 0, false
}

func (p *anthropicProvider) ListModels(ctx context.Context) ([]string, error) {
	if p.deepseek != nil {
		return p.deepSeekModelIDs(ctx)
	}
	var models []string
	pager := p.client.Models.ListAutoPaging(ctx, anthropic.ModelListParams{})
	for pager.Next() {
		models = append(models, pager.Current().ID)
	}
	return models, pager.Err()
}

// IsThinkingBindingError reports the API rejecting replayed thinking blocks that no
// longer match their conversation, for example after goat's system prompt changed.
func IsThinkingBindingError(err error) bool {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		return false
	}
	message := strings.ToLower(apiErr.Error())
	// DeepSeek rejects a transcript whose earlier reasoning it cannot match.
	return strings.Contains(message, "different conversation") || strings.Contains(message, "reasoning_content")
}
