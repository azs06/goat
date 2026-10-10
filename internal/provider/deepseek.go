package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Warnf reports a warning from a provider. It writes a plain "[warn]" line to stderr;
// the CLI replaces it with its own, which clears the spinner and styles the line.
var Warnf = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[warn] "+format+"\n", args...)
}

// DeepSeek is reached through its Anthropic-compatible API, the endpoint it documents
// for agents. It needs the reasoning of every earlier turn sent back whenever tools are
// in use, which the append-only transcript of the Anthropic adapter already does.
// https://api-docs.deepseek.com/guides/anthropic_api
const defaultDeepSeekBaseURL = "https://api.deepseek.com/anthropic"

type deepSeekSettings struct {
	apiKey     string
	modelsURL  string
	httpClient *http.Client
}

func NewDeepSeek() Provider {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("DEEPSEEK_BASE_URL")), "/")
	if baseURL == "" {
		baseURL = defaultDeepSeekBaseURL
	}
	apiKey := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	return NewDeepSeekFor(baseURL, apiKey)
}

// NewDeepSeekFor talks to DeepSeek at baseURL with apiKey, ignoring the environment.
func NewDeepSeekFor(baseURL, apiKey string) Provider {
	return newDeepSeek(baseURL, apiKey)
}

func newDeepSeek(baseURL, apiKey string) *anthropicProvider {
	httpClient := &http.Client{}
	return &anthropicProvider{
		name: DeepSeek,
		// Without environment defaults the client never sends ANTHROPIC_* credentials
		// meant for Anthropic to DeepSeek.
		client: anthropic.NewClient(
			option.WithoutEnvironmentDefaults(),
			option.WithHTTPClient(httpClient),
			option.WithBaseURL(baseURL),
			option.WithAPIKey(apiKey),
		),
		// DeepSeek ignores cache_control (it caches prefixes automatically) and has no
		// refusal fallbacks.
		customBaseURL: true,
		fallbacks:     false,
		deepseek: &deepSeekSettings{
			apiKey:     apiKey,
			modelsURL:  strings.TrimSuffix(baseURL, "/anthropic") + "/models",
			httpClient: httpClient,
		},
		capabilities: map[string]anthropicModelCapabilities{},
	}
}

// deepSeekEffort mirrors DeepSeek's own mapping, so /effort shows what is really used:
// minimal and low are low; medium, high and xhigh are high; max is max.
func deepSeekEffort(level Effort) (anthropic.BetaOutputConfigEffort, bool) {
	switch level {
	case EffortNone:
		return "", false
	case EffortMinimal, EffortLow:
		return anthropic.BetaOutputConfigEffortLow, true
	case EffortMax:
		return anthropic.BetaOutputConfigEffortMax, true
	default:
		return anthropic.BetaOutputConfigEffortHigh, true
	}
}

// applyDeepSeekReasoning leaves thinking on (DeepSeek's default) with the mapped effort,
// or turns it off for effort "none".
func applyDeepSeekReasoning(params *anthropic.BetaMessageNewParams, effort Effort) {
	level, thinking := deepSeekEffort(effort)
	if !thinking {
		params.Thinking = anthropic.BetaThinkingConfigParamUnion{OfDisabled: &anthropic.BetaThinkingConfigDisabledParam{}}
		return
	}
	params.OutputConfig = anthropic.BetaOutputConfigParam{Effort: level}
}

// deepSeekModel is one entry of GET /models.
// https://api-docs.deepseek.com/api/list-models
type deepSeekModel struct {
	ID              string `json:"id"`
	MaxOutputTokens int64  `json:"max_output_tokens"`
}

func (p *anthropicProvider) deepSeekModels(ctx context.Context) ([]deepSeekModel, error) {
	listCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(listCtx, http.MethodGet, p.deepseek.modelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.deepseek.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := p.deepseek.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing DeepSeek models failed: %s: %s", resp.Status, truncate(strings.TrimSpace(string(body)), 200))
	}

	var list struct {
		Data []deepSeekModel `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode DeepSeek models: %w", err)
	}
	return list.Data, nil
}

func (p *anthropicProvider) deepSeekModelIDs(ctx context.Context) ([]string, error) {
	models, err := p.deepSeekModels(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids, nil
}

const (
	// deepSeekDefaultMaxTokens and deepSeekMaxEffortMaxTokens are DeepSeek's own default
	// output limits with thinking on, and at effort max.
	deepSeekDefaultMaxTokens   = 64 * 1024
	deepSeekMaxEffortMaxTokens = 128 * 1024
)

// deepSeekMaxTokens uses DeepSeek's default output limit for the effort, within the
// model's limit from /models when known (caps.maxTokens is 0 when it is not).
func deepSeekMaxTokens(effort Effort, caps anthropicModelCapabilities) int64 {
	limit := int64(deepSeekDefaultMaxTokens)
	if effort == EffortMax {
		limit = deepSeekMaxEffortMaxTokens
	}
	if caps.maxTokens > 0 {
		limit = min(limit, caps.maxTokens)
	}
	return limit
}

// deepSeekCapabilities reads the model's output limit from the model list. DeepSeek
// serves an unknown model name as deepseek-flash, so an unlisted model gets a warning.
// listed is false when the model list could not be read.
func (p *anthropicProvider) deepSeekCapabilities(ctx context.Context, model string) (caps anthropicModelCapabilities, listed bool) {
	models, err := p.deepSeekModels(ctx)
	if err != nil {
		return caps, false
	}
	for _, info := range models {
		if info.ID == model {
			caps.maxTokens = info.MaxOutputTokens
			return caps, true
		}
	}
	Warnf("DeepSeek does not list a model named %q; it will answer with deepseek-flash instead", model)
	return caps, true
}

// CheckDeepSeekModel refuses a model DeepSeek does not list, when the list can be read.
func CheckDeepSeekModel(ctx context.Context, provider Provider, model string) error {
	deepseek, ok := provider.(*anthropicProvider)
	if !ok || deepseek.deepseek == nil {
		return nil
	}
	ids, err := deepseek.deepSeekModelIDs(ctx)
	if err != nil {
		return nil
	}
	for _, id := range ids {
		if id == model {
			return nil
		}
	}
	return fmt.Errorf("DeepSeek has no model named %q (available: %s)", model, strings.Join(ids, ", "))
}
