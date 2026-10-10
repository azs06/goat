package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

func TestOpenAIResponseMapping(t *testing.T) {
	mustResponse := func(raw string) *responses.Response {
		var resp responses.Response
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return &resp
	}
	previous := Conversation{Provider: OpenAI, ResponseID: "resp-prev"}

	usage := convertOpenAIResponse(mustResponse(`{"id":"r1","status":"completed","output":[],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40},"output_tokens":10,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":110}}`), previous)
	if usage.Usage.ContextTokens() != 110 || usage.Usage.CacheReadTokens != 40 || usage.Stop != StopEndTurn {
		t.Fatalf("expected usage to add up to total_tokens, got %+v", usage)
	}

	refusal := convertOpenAIResponse(mustResponse(`{"id":"r2","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I can't help with that."}]}]}`), previous)
	if refusal.Stop != StopRefusal || refusal.Conversation.ResponseID != "resp-prev" || !strings.Contains(refusal.RefusalDetail, "can't help") {
		t.Fatalf("expected a refusal that keeps the previous conversation, got %+v", refusal)
	}

	cut := convertOpenAIResponse(mustResponse(`{"id":"r3","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"function_call","id":"fc","call_id":"call-1","name":"write_file","arguments":"{\"path\":"}]}`), previous)
	if cut.Stop != StopMaxTokens || len(cut.ToolCalls) != 1 || cut.ToolCalls[0].ID != "call-1" {
		t.Fatalf("expected a cut-off response with its call, got %+v", cut)
	}
}

func TestOpenAIRequestShape(t *testing.T) {
	provider := &openAIProvider{}
	params := provider.requestParams(Request{Model: "gpt-4.1", Effort: EffortMax, UserText: "hi", Tools: []ToolSpec{{Name: "read_file", Parameters: map[string]any{"type": "object"}}}})
	if params.Reasoning.Effort != "" {
		t.Fatalf("expected no reasoning setting for gpt-4.1, got %q", params.Reasoning.Effort)
	}
	params = provider.requestParams(Request{Model: "gpt-6-luna", Effort: EffortMax, UserText: "hi"})
	if params.Reasoning.Effort != "xhigh" {
		t.Fatalf("expected max to map to xhigh, got %q", params.Reasoning.Effort)
	}
	params = provider.requestParams(Request{Model: "gpt-6-luna", ToolResults: []ToolResult{{CallID: "call-1", Output: "out"}}})
	if len(params.Input.OfInputItemList) != 1 {
		t.Fatalf("expected only the tool output without a prompt, got %d items", len(params.Input.OfInputItemList))
	}
}

func TestOpenAIStreamErrorEventIsReportedAndRetried(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"Rate limit reached. Please try again in 0.5s.\",\"param\":null,\"sequence_number\":1}\n\n")
	}))
	t.Cleanup(server.Close)

	client := openai.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("x"), option.WithMaxRetries(0))
	_, _, err := streamResponse(context.Background(), &client, responses.ResponseNewParams{Model: "gpt-6-luna"}, func(string) {})
	var eventErr *openAIStreamError
	if !errors.As(err, &eventErr) || eventErr.Code != "rate_limit_exceeded" {
		t.Fatalf("expected the stream error event, got %v", err)
	}
	if delay, ok := streamRetryDelay(err, 0); !ok || delay != 500*time.Millisecond {
		t.Fatalf("expected a 0.5s retry, got %v %v", delay, ok)
	}
}

func TestOpenAIRequestsAskForPromptCaching(t *testing.T) {
	p := NewOpenAIWith(nil, nil).(*openAIProvider)
	tools := []ToolSpec{{Name: "read_file", Description: "Read a file", Parameters: map[string]any{"type": "object"}}}
	req := Request{Model: "gpt-5.5", System: "system", Tools: tools, UserText: "hi"}

	params := p.requestParams(req)
	key := params.PromptCacheKey.Value
	if !strings.HasPrefix(key, "goat-") || params.PromptCacheRetention != responses.ResponseNewParamsPromptCacheRetention24h {
		t.Fatalf("expected a cache key and 24h retention, got %q %q", key, params.PromptCacheRetention)
	}
	req.UserText, req.Conversation = "next", Conversation{Provider: OpenAI, ResponseID: "resp-1"}
	if again := p.requestParams(req).PromptCacheKey.Value; again != key {
		t.Errorf("expected the key to stay the same across a conversation, got %q then %q", key, again)
	}
	req.System = "changed"
	if changed := p.requestParams(req).PromptCacheKey.Value; changed == key {
		t.Error("expected a different key when the prefix changes")
	}

	req.Model = "gpt-4o"
	if retention := p.requestParams(req).PromptCacheRetention; retention != "" {
		t.Errorf("expected no 24h retention for a model without it, got %q", retention)
	}
	req.Model = "gpt-6-luna"
	if retention := p.requestParams(req).PromptCacheRetention; retention == "" {
		t.Error("expected 24h retention to be asked for on a newer model")
	}
}

func TestOpenAIDropsRejectedCacheRetention(t *testing.T) {
	var sent []responses.ResponseNewParams
	p := NewOpenAIWith(nil, func(_ context.Context, params responses.ResponseNewParams, _ func(string)) (*responses.Response, bool, error) {
		sent = append(sent, params)
		if params.PromptCacheRetention != "" {
			return nil, false, &openai.Error{StatusCode: http.StatusBadRequest, Param: "prompt_cache_retention", Message: "Unsupported value"}
		}
		return &responses.Response{ID: "resp-1", Status: responses.ResponseStatusCompleted}, false, nil
	})

	req := Request{Model: "gpt-5.5", System: "system", UserText: "hi"}
	if _, _, err := p.Send(context.Background(), req, func(string) {}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, _, err := p.Send(context.Background(), req, func(string) {}); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	if len(sent) != 3 || sent[0].PromptCacheRetention == "" || sent[1].PromptCacheRetention != "" || sent[2].PromptCacheRetention != "" {
		t.Fatalf("expected one rejected request, a retry without retention, and no retention afterwards; got %d requests", len(sent))
	}
	if !sent[2].PromptCacheKey.Valid() {
		t.Error("expected the cache key to be kept")
	}
	req.Model = "gpt-6-luna"
	if _, _, err := p.Send(context.Background(), req, func(string) {}); err != nil {
		t.Fatalf("Send on another model: %v", err)
	}
	if len(sent) != 5 || sent[3].PromptCacheRetention == "" {
		t.Errorf("expected another model to be asked for 24h retention again, got %d requests", len(sent))
	}
}

func TestOpenAIRecognisesARealCacheRetentionRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"Unsupported parameter: 'prompt_cache_retention' is not supported with this model.","type":"invalid_request_error","param":"prompt_cache_retention","code":"unsupported_parameter"}}`)
	}))
	defer server.Close()

	client := openai.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test-key"), option.WithMaxRetries(0))
	_, _, err := streamResponse(context.Background(), &client, responses.ResponseNewParams{Model: "gpt-6-luna"}, func(string) {})
	if err == nil || !isCacheRetentionRejection(err) {
		t.Fatalf("expected the API's 400 to be recognised, got %v", err)
	}
}
