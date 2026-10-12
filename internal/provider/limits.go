package provider

import "strings"

// Limits are the sizes that matter when deciding how long a conversation may grow.
type Limits struct {
	// ContextWindow is the most tokens one request may take, as the provider states it.
	// 0 means unknown.
	ContextWindow int64
	// PriceStep is the prompt size above which the whole request is billed at a higher
	// rate. 0 means the price does not depend on the prompt's size.
	PriceStep int64
}

// DefaultContextWindow is assumed for a model whose window goat cannot learn, such as
// one behind a providers.json entry. pi assumes the same.
const DefaultContextWindow = 128000

// knownLimits are the providers' published limits (2026-10), by model ID prefix; the
// first match wins. A window the provider reports at runtime replaces the table's.
//   - Claude: platform.claude.com/docs/en/about-claude/pricing; only Claude Haiku 5.5
//     costs more past 100K prompt tokens.
//   - OpenAI: developers.openai.com/api/docs/pricing; past 272K input tokens the whole
//     request costs more. The window is the stated maximum input.
//   - DeepSeek: api-docs.deepseek.com/quick_start/pricing; one price at any size.
var knownLimits = []struct {
	prefix string
	limits Limits
}{
	{"claude-haiku-5-5", Limits{ContextWindow: 1_000_000, PriceStep: 100_000}},
	{"claude-haiku-4-5", Limits{ContextWindow: 200_000}},
	{"claude-3", Limits{ContextWindow: 200_000}},
	{"claude-sonnet-4-5", Limits{ContextWindow: 200_000}},
	{"claude-opus-4-5", Limits{ContextWindow: 200_000}},
	{"claude-opus-4-1", Limits{ContextWindow: 200_000}},
	{"claude-opus-4-0", Limits{ContextWindow: 200_000}},
	{"claude-sonnet-4-0", Limits{ContextWindow: 200_000}},
	{"claude-opus-4-2025", Limits{ContextWindow: 200_000}},
	{"claude-sonnet-4-2025", Limits{ContextWindow: 200_000}},
	{"claude-", Limits{ContextWindow: 1_000_000}},
	{"gpt-5.6", Limits{ContextWindow: 922_000, PriceStep: 272_000}},
	{"gpt-6", Limits{ContextWindow: 922_000, PriceStep: 272_000}},
	{"deepseek-", Limits{ContextWindow: 1_048_576}},
}

// KnownLimits returns the published limits of model, or zero Limits for a model goat
// knows nothing about.
func KnownLimits(model string) Limits {
	model = strings.ToLower(model)
	for _, known := range knownLimits {
		if strings.HasPrefix(model, known.prefix) {
			return known.limits
		}
	}
	return Limits{}
}

// KnownLimitsFor is KnownLimits for a model of the named provider. Only the built-in
// providers serve the models the table describes: a providers.json entry's model that
// shares a name, such as an Ollama build of a DeepSeek model, may have a far smaller
// window.
func KnownLimitsFor(providerName, model string) Limits {
	switch providerName {
	case OpenAI, Anthropic, DeepSeek:
		return KnownLimits(model)
	}
	return Limits{}
}

// windowReporter is implemented by providers that learn a model's window from the API.
type windowReporter interface {
	contextWindow(model string) int64
}

// ModelLimits returns model's limits: the window p has learned from the API, else the
// published one, else DefaultContextWindow.
func ModelLimits(p Provider, model string) Limits {
	limits := KnownLimitsFor(p.Name(), model)
	if reporter, ok := p.(windowReporter); ok {
		if window := reporter.contextWindow(model); window > 0 {
			limits.ContextWindow = window
		}
	}
	if limits.ContextWindow <= 0 {
		limits.ContextWindow = DefaultContextWindow
	}
	return limits
}
