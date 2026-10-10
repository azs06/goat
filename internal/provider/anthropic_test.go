package provider

import (
	"testing"
)

func TestAnthropicEffortClampsToSupportedLevel(t *testing.T) {
	caps := guessAnthropicCapabilities("claude-opus-4-6")
	if level, ok := anthropicEffort(EffortXhigh, caps); !ok || level != "high" {
		t.Fatalf("expected xhigh to clamp to high on Opus 4.6, got %q %v", level, ok)
	}
	if level, ok := anthropicEffort(EffortNone, guessAnthropicCapabilities("claude-opus-5-5")); !ok || level != "low" {
		t.Fatalf("expected none to map to low, got %q %v", level, ok)
	}
	if _, ok := anthropicEffort(EffortHigh, guessAnthropicCapabilities("claude-haiku-4-5")); ok {
		t.Fatal("expected no effort for Haiku 4.5")
	}
}

func TestAnthropicLegacyAliasesAndFallbackModels(t *testing.T) {
	for _, model := range []string{"claude-opus-4-0", "claude-sonnet-4-0", "claude-opus-4-20250514", "claude-haiku-4-5-20251001"} {
		if caps := guessAnthropicCapabilities(model); caps.adaptiveThinking || len(caps.efforts) != 0 {
			t.Errorf("%s: expected no thinking or effort, got %+v", model, caps)
		}
	}
	provider := &anthropicProvider{fallbacks: true}
	for model, want := range map[string]bool{"claude-fable-5-1": true, "claude-opus-5-5": true, "claude-sonnet-5-5": true, "claude-haiku-5-5": false} {
		if got := provider.useFallbacks(model); got != want {
			t.Errorf("%s: fallbacks %v, want %v", model, got, want)
		}
	}
}

func TestAnthropicCacheTTL(t *testing.T) {
	for raw, want := range map[string]string{"": "", "5m": "", "1h": "1h", " 1H ": "1h", "bogus": ""} {
		if got := string(anthropicCacheTTL(raw)); got != want {
			t.Errorf("%q: got %q, want %q", raw, got, want)
		}
	}
}
