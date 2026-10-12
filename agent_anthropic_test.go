package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/tools"
)

// fakeAnthropic is a local Messages API that replays scripted SSE streams and records
// every request, so tests exercise the real SDK end to end.
type fakeAnthropic struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	streams  []string
	requests []recordedRequest
	// modelInfo answers GET /v1/models/{id}; nil makes it return 404.
	modelInfo map[string]any
	// reject, when set, answers the next message request with this status and body.
	reject func(body map[string]any) (int, string, bool)
	// deepseekModels answers DeepSeek's GET /models when set.
	deepseekModels string
	// dropAfter, when set, makes the next stream close the connection after this text.
	dropAfter string
}

type recordedRequest struct {
	header http.Header
	body   map[string]any
	raw    string
}

func newFakeAnthropic(t *testing.T, streams ...string) *fakeAnthropic {
	fake := &fakeAnthropic{t: t, streams: streams}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeAnthropic) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/models" && f.deepseekModels != "" {
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{header: r.Header.Clone(), raw: "GET /models"})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, f.deepseekModels)
		return
	}
	// DeepSeek serves the Anthropic API under /anthropic.
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/anthropic")
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/models/") {
		if f.modelInfo == nil {
			http.Error(w, `{"type":"error","error":{"type":"not_found_error","message":"no"}}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(f.modelInfo)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		http.NotFound(w, r)
		return
	}

	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		f.t.Errorf("request body is not JSON: %v", err)
	}

	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{header: r.Header.Clone(), body: body, raw: string(raw)})
	if f.reject != nil {
		if status, message, ok := f.reject(body); ok {
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			io.WriteString(w, message)
			return
		}
	}
	if len(f.streams) == 0 {
		f.mu.Unlock()
		f.t.Errorf("unexpected request %d", len(f.requests))
		http.Error(w, "no scripted response", http.StatusInternalServerError)
		return
	}
	stream := f.streams[0]
	f.streams = f.streams[1:]
	drop := f.dropAfter
	f.dropAfter = ""
	f.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	if drop != "" {
		io.WriteString(w, drop)
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
		return
	}
	io.WriteString(w, stream)
}

func (f *fakeAnthropic) request(i int) recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.requests) {
		f.t.Fatalf("expected at least %d request(s), got %d", i+1, len(f.requests))
	}
	return f.requests[i]
}

func (f *fakeAnthropic) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// provider returns an adapter talking to the fake server. official makes it behave as
// it does against the real API (fallbacks and automatic caching on).
func (f *fakeAnthropic) provider(official bool) provider.Provider {
	return provider.NewAnthropicWith(official, option.WithBaseURL(f.server.URL), option.WithAPIKey("test-key"), option.WithMaxRetries(0))
}

// sse builds an Anthropic event stream.
type sse struct {
	events []string
	index  int
}

func newSSE(id string) *sse {
	s := &sse{}
	s.event("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":100,"cache_read_input_tokens":20,"cache_creation_input_tokens":10,"output_tokens":1}}}`, id))
	return s
}

func (s *sse) event(name, data string) *sse {
	s.events = append(s.events, fmt.Sprintf("event: %s\ndata: %s\n\n", name, data))
	return s
}

func (s *sse) thinking(signature string) *sse {
	s.event("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":"","signature":""}}`, s.index))
	s.event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":%q}}`, s.index, signature))
	s.event("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, s.index))
	s.index++
	return s
}

func (s *sse) text(parts ...string) *sse {
	s.event("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, s.index))
	for _, part := range parts {
		s.event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, s.index, part))
	}
	s.event("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, s.index))
	s.index++
	return s
}

func (s *sse) toolUse(id, name string, inputParts ...string) *sse {
	s.event("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, s.index, id, name))
	for _, part := range inputParts {
		s.event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%q}}`, s.index, part))
	}
	s.event("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, s.index))
	s.index++
	return s
}

func (s *sse) fallback(from, to string) *sse {
	s.event("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"fallback","from":{"model":%q},"to":{"model":%q}}}`, s.index, from, to))
	s.event("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, s.index))
	s.index++
	return s
}

func (s *sse) stop(reason string, extra string) string {
	delta := fmt.Sprintf(`{"stop_reason":%q,"stop_sequence":null%s}`, reason, extra)
	s.event("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":%s,"usage":{"output_tokens":50}}`, delta))
	s.event("message_stop", `{"type":"message_stop"}`)
	return strings.Join(s.events, "")
}

// unfinished returns the stream without message_delta or message_stop.
func (s *sse) unfinished() string {
	return strings.Join(s.events, "")
}

func messagesOf(t *testing.T, req recordedRequest) []any {
	t.Helper()
	messages, ok := req.body["messages"].([]any)
	if !ok {
		t.Fatalf("request has no messages: %s", req.raw)
	}
	return messages
}

func contentTypes(message any) []string {
	var types []string
	for _, block := range message.(map[string]any)["content"].([]any) {
		types = append(types, block.(map[string]any)["type"].(string))
	}
	return types
}

func TestAnthropicToolRoundTripKeepsHistoryAppendOnly(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	fake := newFakeAnthropic(t,
		newSSE("msg_1").thinking("sig-1").text("Let me look.").toolUse("toolu_1", "read_dir", `{"path":`, `"."}`).stop("tool_use", ""),
		newSSE("msg_2").thinking("sig-2").text("Done.").stop("end_turn", ""),
	)
	fake.modelInfo = map[string]any{
		"id": "claude-opus-5-5", "type": "model", "display_name": "Claude Opus 5.5", "created_at": "2026-01-01T00:00:00Z",
		"max_tokens": 128000, "max_input_tokens": 900000,
		"capabilities": map[string]any{
			"thinking": map[string]any{"supported": true, "types": map[string]any{"adaptive": map[string]any{"supported": true}, "enabled": map[string]any{"supported": false}}},
			"effort":   map[string]any{"supported": true, "low": map[string]any{"supported": true}, "medium": map[string]any{"supported": true}, "high": map[string]any{"supported": true}, "xhigh": map[string]any{"supported": true}, "max": map[string]any{"supported": true}},
		},
	}
	p := fake.provider(true)

	conv, err := sendPromptStream(context.Background(), p, "list files", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortHigh})
	if err != nil {
		t.Fatalf("sendPromptStream: %v", err)
	}

	if limits := provider.ModelLimits(p, "claude-opus-5-5"); limits.ContextWindow != 900000 {
		t.Errorf("expected the window the Models API reported, got %+v", limits)
	}
	first := fake.request(0)
	if first.body["max_tokens"].(float64) != 64000 {
		t.Errorf("expected max_tokens %d, got %v", 64000, first.body["max_tokens"])
	}
	if thinking := first.body["thinking"].(map[string]any); thinking["type"] != "adaptive" {
		t.Errorf("expected adaptive thinking, got %v", thinking)
	}
	if effort := first.body["output_config"].(map[string]any)["effort"]; effort != "high" {
		t.Errorf("expected effort high, got %v", effort)
	}
	if _, ok := first.body["cache_control"]; !ok {
		t.Error("expected automatic caching on the official API")
	}
	system := first.body["system"].([]any)[0].(map[string]any)
	if system["text"] != systemPrompt || system["cache_control"] == nil {
		t.Errorf("expected the cached system prompt, got %v", system)
	}
	if first.body["fallbacks"] != "default" || !strings.Contains(first.header.Get("anthropic-beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("expected fallbacks:default with its beta header, got %v / %q", first.body["fallbacks"], first.header.Get("anthropic-beta"))
	}
	sent := first.body["tools"].([]any)
	if len(sent) != len(tools.Specs) {
		t.Fatalf("expected %d tools, got %d", len(tools.Specs), len(sent))
	}
	if _, eager := sent[0].(map[string]any)["eager_input_streaming"]; eager {
		t.Error("expected eager input streaming to stay off")
	}

	second := messagesOf(t, fake.request(1))
	if len(second) != 3 {
		t.Fatalf("expected user, assistant, user in the second request, got %d messages", len(second))
	}
	firstMessages := messagesOf(t, first)
	if fmt.Sprint(firstMessages[0]) != fmt.Sprint(second[0]) {
		t.Fatalf("expected the second request to start with the first request's messages")
	}
	if got := strings.Join(contentTypes(second[1]), ","); got != "thinking,text,tool_use" {
		t.Errorf("expected the assistant turn replayed whole, got %s", got)
	}
	assistantBlocks := second[1].(map[string]any)["content"].([]any)
	if assistantBlocks[0].(map[string]any)["signature"] != "sig-1" {
		t.Errorf("expected the thinking signature to be replayed, got %v", assistantBlocks[0])
	}
	if input := assistantBlocks[2].(map[string]any)["input"].(map[string]any); input["path"] != "." {
		t.Errorf("expected the tool input to be replayed, got %v", input)
	}
	toolResult := second[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if toolResult["type"] != "tool_result" || toolResult["tool_use_id"] != "toolu_1" {
		t.Errorf("expected the tool result first in the next user message, got %v", toolResult)
	}

	if len(conv.conversation.Messages) != 4 || conv.conversation.Provider != provider.Anthropic || len(conv.pendingToolResults) != 0 {
		t.Fatalf("expected a four-message anthropic conversation, got %+v", conv)
	}

	entries, _ := loadConversationHistoryEntries()
	last := entries[len(entries)-1]
	if last.Type != "assistant" || last.ResponseID != "msg_2" || last.ContextTokens != 180 || last.Provider != provider.Anthropic {
		t.Fatalf("expected the assistant entry with usage, got %+v", last)
	}
	if last.PromptTokens != 130 || last.CacheReadTokens != 20 || last.CacheWriteTokens != 10 {
		t.Errorf("expected the cache usage recorded, got %+v", last)
	}
	// The first response's usage is on its tool call entry; it counts once.
	if usage := conversationCacheUsage(entries); usage.responses != 2 || usage.promptTokens != 260 || usage.read != 40 || usage.written != 20 {
		t.Errorf("expected the cache usage of two responses, got %+v", usage)
	}
}

func TestAnthropicCustomBaseURLSkipsOfficialOnlyFeatures(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	fake := newFakeAnthropic(t, newSSE("msg_1").text("hi").stop("end_turn", ""))
	if _, err := sendPromptStream(context.Background(), fake.provider(false), "hello", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortMedium}); err != nil {
		t.Fatalf("sendPromptStream: %v", err)
	}

	body := fake.request(0).body
	if _, ok := body["fallbacks"]; ok {
		t.Error("expected no fallbacks behind a custom base URL")
	}
	if _, ok := body["cache_control"]; ok {
		t.Error("expected no automatic caching behind a custom base URL")
	}
}

func lastBlockCacheControl(message any) any {
	blocks := message.(map[string]any)["content"].([]any)
	return blocks[len(blocks)-1].(map[string]any)["cache_control"]
}

func TestAnthropicCustomBaseURLMarksOnlyTheNewestMessageForCaching(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	fake := newFakeAnthropic(t,
		newSSE("msg_1").toolUse("toolu_1", "read_dir", `{"path":"."}`).stop("tool_use", ""),
		newSSE("msg_2").text("Done.").stop("end_turn", ""),
	)
	conv, err := sendPromptStream(context.Background(), fake.provider(false), "list files", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortMedium})
	if err != nil {
		t.Fatalf("sendPromptStream: %v", err)
	}

	first := messagesOf(t, fake.request(0))
	if lastBlockCacheControl(first[0]) == nil {
		t.Error("expected a cache breakpoint on the prompt")
	}
	second := messagesOf(t, fake.request(1))
	if lastBlockCacheControl(second[0]) != nil {
		t.Error("expected the earlier breakpoint to be gone from the next request")
	}
	if lastBlockCacheControl(second[2]) == nil {
		t.Error("expected a cache breakpoint on the tool results")
	}
	for i, raw := range conv.conversation.Messages {
		if strings.Contains(string(raw), "cache_control") {
			t.Errorf("expected the stored transcript to carry no breakpoints, message %d: %s", i, raw)
		}
	}
}

func TestAnthropicOneOffCallsAreNotCached(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	fake := newFakeAnthropic(t, newSSE("msg_1").text("summary").stop("end_turn", ""))
	if _, err := fake.provider(true).Complete(context.Background(), "claude-opus-5-5", "Summarize.", "history"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	body := fake.request(0).body
	if _, ok := body["cache_control"]; ok {
		t.Error("expected no automatic caching on a one-off call")
	}
	if system := body["system"].([]any)[0].(map[string]any); system["cache_control"] != nil {
		t.Errorf("expected no breakpoint on a one-off system prompt, got %v", system)
	}
}

func TestAnthropicLegacyModelsGetNoThinkingOrEffort(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	fake := newFakeAnthropic(t, newSSE("msg_1").text("hi").stop("end_turn", ""))
	if _, err := sendPromptStream(context.Background(), fake.provider(true), "hello", conversationState{}, turnOptions{model: "claude-haiku-4-5-20251001", effort: provider.EffortHigh}); err != nil {
		t.Fatalf("sendPromptStream: %v", err)
	}

	body := fake.request(0).body
	for _, field := range []string{"thinking", "output_config", "fallbacks"} {
		if _, ok := body[field]; ok {
			t.Errorf("expected no %s for a legacy model, got %v", field, body[field])
		}
	}
	if body["max_tokens"].(float64) != 32000 {
		t.Errorf("expected max_tokens %d, got %v", 32000, body["max_tokens"])
	}
}

func TestAnthropicRefusalDiscardsTheResponse(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	fake := newFakeAnthropic(t, newSSE("msg_1").text("I can").stop("refusal", `,"stop_details":{"type":"refusal","category":"cyber","explanation":"policy"}`))
	start := conversationState{
		conversation:       provider.Conversation{Provider: provider.Anthropic, Messages: []json.RawMessage{json.RawMessage(`{"role":"user","content":[{"type":"text","text":"hi"}]}`), json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_9","name":"read_dir","input":{"path":"."}}]}`)}},
		pendingToolResults: []provider.ToolResult{{CallID: "toolu_9", Name: "read_dir", Output: "a.txt"}},
	}

	conv, err := sendPromptStream(context.Background(), fake.provider(true), "next", start, turnOptions{model: "claude-opus-5-5", effort: provider.EffortMedium})
	if err == nil || !strings.Contains(err.Error(), "declined") || !strings.Contains(err.Error(), "cyber") {
		t.Fatalf("expected a refusal error naming the category, got %v", err)
	}
	if len(conv.conversation.Messages) != 2 || len(conv.pendingToolResults) != 1 {
		t.Fatalf("expected the conversation and pending results to be unchanged, got %+v", conv)
	}
}

func TestAnthropicIncompleteStreamIsRetriedAndNeverStored(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	previousSleeper := streamRetrySleeper
	streamRetrySleeper = func(ctx context.Context, delay time.Duration) error { return nil }
	t.Cleanup(func() { streamRetrySleeper = previousSleeper })

	fake := newFakeAnthropic(t,
		newSSE("msg_1").thinking("sig-cut").unfinished(),
		// message_stop without a stop reason: the message is still incomplete.
		newSSE("msg_2").thinking("sig-cut").event("message_stop", `{"type":"message_stop"}`).unfinished(),
		newSSE("msg_3").text("ok").stop("end_turn", ""),
	)
	conv, err := sendPromptStream(context.Background(), fake.provider(true), "hello", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortMedium})
	if err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
	if fake.requestCount() != 3 {
		t.Fatalf("expected two retries, got %d request(s)", fake.requestCount())
	}
	if strings.Contains(string(conv.conversation.Messages[1]), "sig-cut") {
		t.Fatal("expected the unfinished response to be discarded")
	}
}

func TestAnthropicOverloadedStreamErrorIsRetried(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	previousSleeper := streamRetrySleeper
	streamRetrySleeper = func(ctx context.Context, delay time.Duration) error { return nil }
	t.Cleanup(func() { streamRetrySleeper = previousSleeper })

	overloaded := newSSE("msg_1").event("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`).unfinished()
	fake := newFakeAnthropic(t, overloaded, newSSE("msg_2").text("ok").stop("end_turn", ""))
	if _, err := sendPromptStream(context.Background(), fake.provider(true), "hello", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortMedium}); err != nil {
		t.Fatalf("expected the overload to be retried, got %v", err)
	}
	if fake.requestCount() != 2 {
		t.Fatalf("expected two requests, got %d", fake.requestCount())
	}
}

func TestAnthropicCutOffToolCallIsNotRun(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	fake := newFakeAnthropic(t,
		newSSE("msg_1").toolUse("toolu_1", "write_file", `{"path":"a.txt","content":"par`).stop("max_tokens", ""),
		newSSE("msg_2").text("ok").stop("end_turn", ""),
	)
	p := fake.provider(true)

	conv, err := sendPromptStream(context.Background(), p, "write it", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortMedium})
	if err == nil || !strings.Contains(err.Error(), "cut off") {
		t.Fatalf("expected a cut-off error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "a.txt")); statErr == nil {
		t.Fatal("expected the cut-off write not to run")
	}
	if len(conv.pendingToolResults) != 1 || !conv.pendingToolResults[0].IsError {
		t.Fatalf("expected an error result to be pending, got %+v", conv.pendingToolResults)
	}

	if _, err := sendPromptStream(context.Background(), p, "try again", conv, turnOptions{model: "claude-opus-5-5", effort: provider.EffortMedium}); err != nil {
		t.Fatalf("next prompt: %v", err)
	}
	user := messagesOf(t, fake.request(1))[2].(map[string]any)["content"].([]any)
	if user[0].(map[string]any)["is_error"] != true || user[1].(map[string]any)["text"] != modelNote("claude-opus-5-5", "anthropic")+"try again" {
		t.Fatalf("expected the error result before the new prompt, got %v", user)
	}
}

func TestAnthropicFallbackTurnOnlyRunsServedToolCalls(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	fake := newFakeAnthropic(t,
		newSSE("msg_1").thinking("sig-declined").text("Partial ").toolUse("toolu_declined", "delete_dir", `{"path":"src"}`).
			fallback("claude-opus-5-5", "claude-opus-4-8").text("Served.").toolUse("toolu_served", "read_dir", `{"path":"."}`).stop("tool_use", ""),
		newSSE("msg_2").text("done").stop("end_turn", ""),
	)
	if err := os.Mkdir(filepath.Join(workDir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}

	conv, err := sendPromptStream(context.Background(), fake.provider(true), "go", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortMedium})
	if err != nil {
		t.Fatalf("sendPromptStream: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "src")); statErr != nil {
		t.Fatal("expected the declined delete_dir not to run")
	}

	second := messagesOf(t, fake.request(1))
	if got := strings.Join(contentTypes(second[1]), ","); got != "text,fallback,text,tool_use" {
		t.Fatalf("expected only text before the fallback block, got %s", got)
	}
	results := second[2].(map[string]any)["content"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["tool_use_id"] != "toolu_served" {
		t.Fatalf("expected a result for the served call only, got %v", results)
	}
	if len(conv.conversation.Messages) != 4 {
		t.Fatalf("expected four messages, got %d", len(conv.conversation.Messages))
	}
}

func TestAnthropicFallbacksAreDroppedWhenRejected(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	fake := newFakeAnthropic(t, newSSE("msg_1").text("ok").stop("end_turn", ""), newSSE("msg_2").text("ok").stop("end_turn", ""))
	fake.reject = func(body map[string]any) (int, string, bool) {
		if _, ok := body["fallbacks"]; ok {
			return http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"fallbacks: not available for this organization"}}`, true
		}
		return 0, "", false
	}
	p := fake.provider(true)

	if _, err := sendPromptStream(context.Background(), p, "hello", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortMedium}); err != nil {
		t.Fatalf("expected a retry without fallbacks, got %v", err)
	}
	if _, err := sendPromptStream(context.Background(), p, "again", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortMedium}); err != nil {
		t.Fatalf("second prompt: %v", err)
	}
	if fake.requestCount() != 3 {
		t.Fatalf("expected one rejected and two successful requests, got %d", fake.requestCount())
	}
	if _, ok := fake.request(2).body["fallbacks"]; ok {
		t.Fatal("expected fallbacks to stay off for the rest of the session")
	}
}

func TestAnthropicAnswersEveryUnansweredToolUse(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	fake := newFakeAnthropic(t, newSSE("msg_1").text("ok").stop("end_turn", ""))
	start := conversationState{conversation: provider.Conversation{Provider: provider.Anthropic, Messages: []json.RawMessage{
		json.RawMessage(`{"role":"user","content":[{"type":"text","text":"hi"}]}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_a","name":"read_dir","input":{"path":"."}},{"type":"tool_use","id":"toolu_b","name":"read_dir","input":{"path":"."}}]}`),
	}}, pendingToolResults: []provider.ToolResult{{CallID: "toolu_b", Output: "b"}, {CallID: "toolu_stale", Output: "x"}}}

	if _, err := sendPromptStream(context.Background(), fake.provider(true), "next", start, turnOptions{model: "claude-opus-5-5", effort: provider.EffortMedium}); err != nil {
		t.Fatalf("sendPromptStream: %v", err)
	}
	user := messagesOf(t, fake.request(0))[2].(map[string]any)["content"].([]any)
	if len(user) != 3 || user[0].(map[string]any)["tool_use_id"] != "toolu_a" || user[0].(map[string]any)["is_error"] != true || user[1].(map[string]any)["tool_use_id"] != "toolu_b" {
		t.Fatalf("expected results for exactly toolu_a (error) and toolu_b, then the prompt; got %v", user)
	}
}

func TestAnthropicCompactionSeedsATranscript(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	recordHistory(historyEntry{Type: "user", Content: "Please fix the parser."})
	recordHistory(historyEntry{Type: "assistant", Content: "Working on it.", ResponseID: "msg_old", Provider: provider.Anthropic})

	fake := newFakeAnthropic(t, newSSE("msg_sum").thinking("sig").text("Goal: fix the parser.").stop("end_turn", ""))
	cfg := &config{
		model:           provider.Model{Provider: provider.Anthropic, ID: "claude-opus-5-5"},
		reasoningEffort: provider.EffortHigh,
		conversation:    conversationState{conversation: provider.Conversation{Provider: provider.Anthropic, Messages: []json.RawMessage{json.RawMessage(`{"role":"user","content":[{"type":"text","text":"old"}]}`)}}},
		providers:       map[string]provider.Provider{provider.Anthropic: fake.provider(true)},
	}

	if err := commandCompact(context.Background(), cfg, nil); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if effort := fake.request(0).body["output_config"].(map[string]any)["effort"]; effort != "low" {
		t.Errorf("expected low effort for the summary, got %v", effort)
	}
	if _, ok := fake.request(0).body["tools"]; ok {
		t.Error("expected no tools on the summary request")
	}
	messages := cfg.conversation.conversation.Messages
	if len(messages) != 2 || !strings.Contains(string(messages[0]), "Goal: fix the parser.") || !strings.Contains(string(messages[1]), "Context loaded.") {
		t.Fatalf("expected a seeded two-message transcript, got %d messages", len(messages))
	}
}

func TestAnthropicThinkingBindingErrorIsRecognised(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	fake := newFakeAnthropic(t)
	fake.reject = func(body map[string]any) (int, string, bool) {
		return http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.0: thinking block is bound to a different conversation"}}`, true
	}
	_, _, err := fake.provider(true).Send(context.Background(), provider.Request{Model: "claude-opus-5-5", UserText: "hi"}, nil)
	if !provider.IsThinkingBindingError(err) {
		t.Fatalf("expected a thinking binding error, got %v", err)
	}
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an API error, got %T", err)
	}
}

func TestAnthropicDroppedConnectionIsRetried(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	previousSleeper := streamRetrySleeper
	streamRetrySleeper = func(ctx context.Context, delay time.Duration) error { return nil }
	t.Cleanup(func() { streamRetrySleeper = previousSleeper })

	fake := newFakeAnthropic(t, "unused", newSSE("msg_2").text("ok").stop("end_turn", ""))
	fake.dropAfter = newSSE("msg_1").thinking("sig").unfinished()
	if _, err := sendPromptStream(context.Background(), fake.provider(true), "hello", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortMedium}); err != nil {
		t.Fatalf("expected the dropped connection to be retried, got %v", err)
	}
	if fake.requestCount() != 2 {
		t.Fatalf("expected two requests, got %d", fake.requestCount())
	}
}

func TestAnthropicTurnCompactedBetweenToolRoundsSendsTheResultsAsText(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	big := &sse{}
	big.event("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":1}}}`, defaultClaudeAutoCompactTokens+1))
	fake := newFakeAnthropic(t,
		big.toolUse("toolu_1", "read_dir", `{"path":"."}`).stop("tool_use", ""),
		newSSE("msg_sum").text("Goal: look around.").stop("end_turn", ""),
		newSSE("msg_2").text("done").stop("end_turn", ""),
	)
	cfg := &config{
		model:           provider.Model{Provider: provider.Anthropic, ID: "claude-opus-5-5"},
		reasoningEffort: provider.EffortHigh,
		providers:       map[string]provider.Provider{provider.Anthropic: fake.provider(true)},
		approver:        &bashApprover{},
	}

	if err := runPrompt(context.Background(), cfg, "look around"); err != nil {
		t.Fatalf("runPrompt: %v", err)
	}
	if fake.requestCount() != 3 {
		t.Fatalf("expected the turn, the summary and the continued turn, got %d requests", fake.requestCount())
	}
	messages := messagesOf(t, fake.request(2))
	encoded, _ := json.Marshal(messages)
	if strings.Contains(string(encoded), "tool_result") || strings.Contains(string(encoded), "tool_use") {
		t.Fatalf("expected no tool blocks after compacting, got %s", encoded)
	}
	last, _ := json.Marshal(messages[len(messages)-1])
	if !strings.Contains(string(last), `"role":"user"`) || !strings.Contains(string(last), "Result of read_dir (ok)") || !strings.Contains(string(encoded), "Goal: look around.") {
		t.Fatalf("expected the summary transcript and a user message carrying the tool results, got %s", encoded)
	}
}
