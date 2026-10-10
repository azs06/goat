package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

const deepSeekModelList = `{"object":"list","data":[{"id":"deepseek-v4-pro","object":"model","owned_by":"deepseek","max_output_tokens":32000},{"id":"deepseek-flash","object":"model","owned_by":"deepseek"}]}`

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
