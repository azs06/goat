package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	openaioption "github.com/openai/openai-go/v3/option"
)

// OpenCodeGo is OpenCode's Go subscription (opencode.ai/docs/go): one key, OPENCODE_API_KEY,
// for open models that it serves through three APIs, chosen per model. pi calls the
// provider opencode-go too, so a model is written the same way in both.
const OpenCodeGo = "opencode-go"

// APIOpenCode marks the opencode-go preset, which picks the API per model. It cannot be
// written in config.json.
const APIOpenCode = "opencode"

// isOpenCode reports the opencode-go preset; config.json cannot name APIOpenCode.
func isOpenCode(name string) bool {
	return entries[name].API == APIOpenCode
}

// apiResponses is OpenAI's Responses API, which the opencode-go router does not use.
const apiResponses = "responses"

// openCodeSessionHeader routes a conversation's requests together on OpenCode's side;
// pi sends it on every request.
const openCodeSessionHeader = "x-opencode-session"

// openCodeAPI is the API OpenCode Go serves model through, by family, following the
// endpoint table in OpenCode's docs (2026-10): Claude, MiniMax and Qwen take the Messages
// API, GPT, Grok and Muse the Responses API, and the rest Chat Completions. pi's catalog
// puts minimax-m2.7 on Chat Completions; the docs, which goat follows, on Messages.
func openCodeAPI(model string) string {
	lower := strings.ToLower(model)
	for _, prefix := range []string{"claude-", "minimax-", "qwen"} {
		if strings.HasPrefix(lower, prefix) {
			return APIAnthropic
		}
	}
	for _, prefix := range []string{"gpt-", "grok-", "muse-"} {
		if strings.HasPrefix(lower, prefix) {
			return apiResponses
		}
	}
	return APIOpenAI
}

// openCodeModels are the Go models the picker offers, in the order of OpenCode's docs
// (2026-10), without the Responses API ones goat cannot use there yet. They are not the
// entry's Models: a bare deepseek-v4-pro must still mean DeepSeek's own.
var openCodeModels = []string{
	"kimi-k3", "kimi-k2.7-code", "kimi-k2.6",
	"glm-5.3", "glm-5.3-flash", "glm-5.2",
	"deepseek-v4-pro", "deepseek-v4.1-flash", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp",
	"minimax-m3", "minimax-m2.7",
	"qwen3.8-max", "qwen3.8-flash", "qwen3.7-plus",
	"claude-haiku-5-5",
	"mimo-v2.6-pro", "mimo-v2.6-flash", "mimo-v2.5-pro", "mimo-v2.5",
	"longcat-2.0", "hy4-preview", "hy3", "space-bunny",
	"longcat-2.5-preview-free", "step-5-preview-free",
}

// openCodeWindows are the Go models' context windows from pi's catalog (2026-10), where
// they differ from 1M tokens, which the other models listed above have.
var openCodeWindows = map[string]int64{
	"kimi-k3":         1_048_576,
	"kimi-k2.7-code":  262_144,
	"minimax-m2.7":    204_800,
	"hy3":             256_000,
	"hy4-preview":     1_024_000,
	"mimo-v2.6-pro":   1_048_576,
	"mimo-v2.6-flash": 1_048_576,
	"mimo-v2.5-pro":   1_048_576,
}

// openCodeLimits are the published limits of a model OpenCode Go serves, or zero Limits
// for one goat does not know.
func openCodeLimits(model string) Limits {
	if strings.HasPrefix(model, "claude-") {
		return KnownLimits(model)
	}
	if window, ok := openCodeWindows[model]; ok {
		return Limits{ContextWindow: window}
	}
	for _, known := range openCodeModels {
		if known == model {
			return Limits{ContextWindow: 1_000_000}
		}
	}
	return Limits{}
}

// openCodeProvider sends each request to the adapter for the model's API: Chat
// Completions at the base URL, or the Messages API beside it.
type openCodeProvider struct {
	name     string
	chat     *chatProvider
	messages *anthropicProvider
}

// newOpenCode creates the router. baseURL ends in /v1, where Chat Completions and the
// model list live; the Anthropic SDK adds v1/messages itself. The session header is one
// per run, which is one conversation unless -c or /resume continues an older one; it only
// keeps requests together.
func newOpenCode(name, baseURL, key string) *openCodeProvider {
	session := newOpenCodeSession()
	return &openCodeProvider{
		name:     name,
		chat:     newChatProvider(name, baseURL, key, openaioption.WithHeader(openCodeSessionHeader, session)),
		messages: newAnthropicEntry(name, strings.TrimSuffix(baseURL, "/v1"), key, anthropicoption.WithHeader(openCodeSessionHeader, session)),
	}
}

func newOpenCodeSession() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

func (p *openCodeProvider) Name() string {
	return p.name
}

// adapter returns the adapter for model, or an error for one on the Responses API.
func (p *openCodeProvider) adapter(model string) (Provider, error) {
	switch openCodeAPI(model) {
	case APIAnthropic:
		return p.messages, nil
	case apiResponses:
		return nil, openCodeResponsesError(model)
	default:
		return p.chat, nil
	}
}

func openCodeResponsesError(model string) error {
	return fmt.Errorf("goat cannot use %s through OpenCode Go yet: OpenCode serves it through OpenAI's Responses API, which goat only speaks to OpenAI itself", model)
}

func (p *openCodeProvider) Send(ctx context.Context, req Request, onText func(string)) (*Response, bool, error) {
	adapter, err := p.adapter(req.Model)
	if err != nil {
		return nil, false, err
	}
	return adapter.Send(ctx, req, onText)
}

func (p *openCodeProvider) Complete(ctx context.Context, model, system, prompt string) (string, error) {
	adapter, err := p.adapter(model)
	if err != nil {
		return "", err
	}
	return adapter.Complete(ctx, model, system, prompt)
}

func (p *openCodeProvider) Seed(ctx context.Context, model, summary string) (Conversation, error) {
	adapter, err := p.adapter(model)
	if err != nil {
		return Conversation{}, err
	}
	return adapter.Seed(ctx, model, summary)
}

// RetryDelay asks the adapter whose SDK made the error.
func (p *openCodeProvider) RetryDelay(err error, attempt int) (time.Duration, bool) {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return p.messages.RetryDelay(err, attempt)
	}
	return p.chat.RetryDelay(err, attempt)
}

// ListModels returns the models OpenCode lists, without the Responses API ones.
func (p *openCodeProvider) ListModels(ctx context.Context) ([]string, error) {
	ids, err := p.chat.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	return filterModels(ids, func(id string) bool { return openCodeAPI(id) != apiResponses }), nil
}

// CheckModel refuses a model the provider is known not to serve, before goat switches to
// it: a DeepSeek model DeepSeek does not list, or an OpenCode Go model on the Responses
// API.
func CheckModel(ctx context.Context, p Provider, model string) error {
	if _, ok := p.(*openCodeProvider); ok && openCodeAPI(model) == apiResponses {
		return openCodeResponsesError(model)
	}
	return CheckDeepSeekModel(ctx, p, model)
}
