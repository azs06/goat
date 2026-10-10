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
