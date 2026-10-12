package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

const deepSeekModelList = `{"object":"list","data":[{"id":"deepseek-v4-pro","object":"model","owned_by":"deepseek","max_output_tokens":32000,"context_window":500000},{"id":"deepseek-flash","object":"model","owned_by":"deepseek"}]}`

func TestDeepSeekMaxTokensFollowEffort(t *testing.T) {
	known := anthropicModelCapabilities{maxTokens: 393216}
	if got := deepSeekMaxTokens(EffortHigh, known); got != deepSeekDefaultMaxTokens {
		t.Errorf("expected 64K by default, got %d", got)
	}
	if got := deepSeekMaxTokens(EffortMax, known); got != deepSeekMaxEffortMaxTokens {
		t.Errorf("expected 128K at effort max, got %d", got)
	}
	if got := deepSeekMaxTokens(EffortMax, anthropicModelCapabilities{maxTokens: 32000}); got != 32000 {
		t.Errorf("expected the model's own limit to cap it, got %d", got)
	}
	if got := deepSeekMaxTokens(EffortLow, anthropicModelCapabilities{}); got != deepSeekDefaultMaxTokens {
		t.Errorf("expected 64K when the limit is unknown, got %d", got)
	}
}

func TestTransportFailureBeforeTheStreamIsNotRetriedTwice(t *testing.T) {
	provider := newDeepSeek("http://127.0.0.1:1/anthropic", "ds-key")
	_, _, err := provider.Send(context.Background(), Request{Model: "deepseek-flash", UserText: "hi"}, nil)
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if _, retry := provider.RetryDelay(err, 0); retry {
		t.Fatalf("expected the SDK's own retries to be the only ones, got a retry for %v", err)
	}
}

func TestDeepSeekCapabilitiesAreNotCachedWhenTheListFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, deepSeekModelList)
	}))
	t.Cleanup(server.Close)
	provider := newDeepSeek(server.URL+"/anthropic", "ds-key")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	provider.modelCapabilities(cancelled, "deepseek-v4-pro")
	if caps := provider.modelCapabilities(context.Background(), "deepseek-v4-pro"); caps.maxTokens != 32000 {
		t.Fatalf("expected the listed limit after a failed lookup, got %d", caps.maxTokens)
	}
}

func TestModelLimitsUseTheWindowTheAPIReported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, deepSeekModelList)
	}))
	t.Cleanup(server.Close)
	provider := newDeepSeek(server.URL+"/anthropic", "ds-key")

	// Nothing was sent yet, so the published window applies without asking the API.
	if limits := ModelLimits(provider, "deepseek-v4-pro"); limits.ContextWindow != 1_048_576 {
		t.Fatalf("expected the published window before any request, got %+v", limits)
	}
	provider.modelCapabilities(context.Background(), "deepseek-v4-pro")
	if limits := ModelLimits(provider, "deepseek-v4-pro"); limits.ContextWindow != 500000 || limits.PriceStep != 0 {
		t.Fatalf("expected the listed window, got %+v", limits)
	}
	// A model the list gives no window for keeps the published one.
	provider.modelCapabilities(context.Background(), "deepseek-flash")
	if limits := ModelLimits(provider, "deepseek-flash"); limits.ContextWindow != 1_048_576 {
		t.Fatalf("expected the published window, got %+v", limits)
	}
}

func TestKnownLimits(t *testing.T) {
	for model, want := range map[string]Limits{
		"claude-haiku-5-5":          {ContextWindow: 1_000_000, PriceStep: 100_000},
		"claude-opus-5-5":           {ContextWindow: 1_000_000},
		"claude-haiku-4-5-20251001": {ContextWindow: 200_000},
		"gpt-6-luna":                {ContextWindow: 922_000, PriceStep: 272_000},
		"gpt-5.6-terra":             {ContextWindow: 922_000, PriceStep: 272_000},
		"qwen3:8b":                  {},
	} {
		if got := KnownLimits(model); got != want {
			t.Errorf("%s: got %+v, want %+v", model, got, want)
		}
	}
	if limits := ModelLimits(&anthropicProvider{capabilities: map[string]anthropicModelCapabilities{}}, "qwen3:8b"); limits.ContextWindow != DefaultContextWindow {
		t.Errorf("expected an unknown model to get the default window, got %+v", limits)
	}
}
