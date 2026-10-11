package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// chatServer is a fake Chat Completions server. Each request is answered with the next
// list of SSE chunks; the request bodies and headers are kept.
type chatServer struct {
	*httptest.Server
	mu      sync.Mutex
	bodies  []map[string]any
	headers []http.Header
}

func newChatServer(t *testing.T, replies ...[]string) *chatServer {
	t.Helper()
	server := &chatServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"object":"list","data":[{"id":"qwen3-coder","object":"model"},{"id":"llama3.2:latest","object":"model"}]}`)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		server.mu.Lock()
		n := len(server.bodies)
		server.bodies = append(server.bodies, body)
		server.headers = append(server.headers, r.Header.Clone())
		server.mu.Unlock()
		if n >= len(replies) {
			http.Error(w, `{"error":{"message":"no more replies"}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range replies[n] {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server
}

func chunk(delta string, finish string) string {
	finishJSON := "null"
	if finish != "" {
		finishJSON = `"` + finish + `"`
	}
	return `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + finishJSON + `}]}`
}

const usageChunk = `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":7,"total_tokens":127,"prompt_tokens_details":{"cached_tokens":100}}}`

func TestChatProviderStreamsToolCallsAndKeepsTheTranscript(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-openai-SECRET")
	t.Setenv("OPENAI_ORG_ID", "org-SECRET")
	t.Setenv("OPENAI_PROJECT_ID", "proj-SECRET")
	server := newChatServer(t,
		[]string{
			chunk(`{"role":"assistant","content":"Let me look."}`, ""),
			chunk(`{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"pa"}}]}`, ""),
			chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"main.go\"}"}}]}`, ""),
			chunk(`{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"list_files","arguments":""}}]}`, ""),
			chunk(`{}`, "tool_calls"),
			usageChunk,
		},
		[]string{chunk(`{"content":"Done."}`, "stop")},
	)
	p := newChatProvider("local", server.URL+"/v1", "entry-key")

	var streamed strings.Builder
	resp, printed, err := p.Send(context.Background(), Request{
		Model:    "qwen3-coder",
		System:   "be brief",
		Tools:    []ToolSpec{{Name: "read_file", Description: "read", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}},
		UserText: "what is in main.go?",
	}, func(text string) { streamed.WriteString(text) })
	if err != nil {
		t.Fatal(err)
	}
	if !printed || streamed.String() != "Let me look." || resp.Text != "Let me look." || resp.Stop != StopToolUse {
		t.Fatalf("got %+v printed=%v streamed=%q", resp, printed, streamed.String())
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0] != (ToolCall{ID: "call_a", Name: "read_file", Arguments: `{"path":"main.go"}`}) || resp.ToolCalls[1] != (ToolCall{ID: "call_b", Name: "list_files", Arguments: "{}"}) {
		t.Fatalf("got %+v", resp.ToolCalls)
	}
	if resp.Usage != (Usage{InputTokens: 20, CacheReadTokens: 100, OutputTokens: 7}) {
		t.Fatalf("got %+v", resp.Usage)
	}
	if resp.Conversation.Provider != "local" || len(resp.Conversation.Messages) != 2 {
		t.Fatalf("got %+v", resp.Conversation)
	}

	header := server.headers[0]
	if header.Get("Authorization") != "Bearer entry-key" || header.Get("OpenAI-Organization") != "" || header.Get("OpenAI-Project") != "" {
		t.Fatalf("expected only the entry's own key, got %v", header)
	}
	first := server.bodies[0]
	if first["model"] != "qwen3-coder" || first["stream"] != true || first["reasoning_effort"] != nil {
		t.Fatalf("got %v", first)
	}

	// The tool results answer the calls, and the whole transcript is sent again.
	resp, _, err = p.Send(context.Background(), Request{
		Model:        "qwen3-coder",
		System:       "be brief",
		Conversation: resp.Conversation,
		ToolResults:  []ToolResult{{CallID: "call_a", Output: "package main"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "Done." || resp.Stop != StopEndTurn || len(resp.Conversation.Messages) != 5 {
		t.Fatalf("got %+v", resp)
	}
	messages, _ := json.Marshal(server.bodies[1]["messages"])
	want := `[{"content":"be brief","role":"system"},{"content":"what is in main.go?","role":"user"},` +
		`{"content":"Let me look.","role":"assistant","tool_calls":[{"function":{"arguments":"{\"path\":\"main.go\"}","name":"read_file"},"id":"call_a","type":"function"},{"function":{"arguments":"{}","name":"list_files"},"id":"call_b","type":"function"}]},` +
		`{"content":"package main","role":"tool","tool_call_id":"call_a"},` +
		`{"content":"This tool call was not run because the session changed before its result was delivered.","role":"tool","tool_call_id":"call_b"}]`
	if string(messages) != want {
		t.Fatalf("got  %s\nwant %s", messages, want)
	}
}

func TestChatProviderSendsNoKeyForAKeylessEntry(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-openai-SECRET")
	server := newChatServer(t, []string{chunk(`{"content":"hi"}`, "stop")})
	p := newChatProvider("local", server.URL+"/v1", "")
	if _, _, err := p.Send(context.Background(), Request{Model: "m", UserText: "hi"}, nil); err != nil {
		t.Fatal(err)
	}
	if auth := server.headers[0].Get("Authorization"); auth != "" {
		t.Fatalf("expected no Authorization header, got %q", auth)
	}
}

func TestChatProviderToleratesLooseServers(t *testing.T) {
	// No call IDs, every call at index 0 with its own ID, and "stop" after tool calls.
	server := newChatServer(t,
		[]string{
			chunk(`{"tool_calls":[{"index":0,"function":{"name":"read_file","arguments":"{}"}}]}`, "stop"),
		},
		[]string{
			chunk(`{"tool_calls":[{"index":0,"id":"x1","function":{"name":"a","arguments":"{}"}}]}`, ""),
			chunk(`{"tool_calls":[{"index":0,"id":"x2","function":{"name":"b","arguments":"{}"}}]}`, "tool_calls"),
		},
	)
	p := newChatProvider("local", server.URL+"/v1", "")
	resp, _, err := p.Send(context.Background(), Request{Model: "m", UserText: "hi"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Stop != StopToolUse || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID == "" {
		t.Fatalf("got %+v", resp)
	}
	resp, _, err = p.Send(context.Background(), Request{Model: "m", UserText: "hi"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].Name != "a" || resp.ToolCalls[1].Name != "b" {
		t.Fatalf("got %+v", resp.ToolCalls)
	}
}

func TestChatProviderRetriesACutOffStream(t *testing.T) {
	server := newChatServer(t, []string{chunk(`{"content":"par"}`, "")})
	p := newChatProvider("local", server.URL+"/v1", "")
	_, printed, err := p.Send(context.Background(), Request{Model: "m", UserText: "hi"}, func(string) {})
	if err == nil || !printed {
		t.Fatalf("expected an unfinished stream, got %v", err)
	}
	if _, retry := p.RetryDelay(err, 0); !retry {
		t.Fatalf("expected a retry for %v", err)
	}
}

func TestChatProviderSeedsCompletesAndListsModels(t *testing.T) {
	server := newChatServer(t, []string{chunk(`{"content":"a summary"}`, "stop")})
	p := newChatProvider("local", server.URL+"/v1", "")
	if text, err := p.Complete(context.Background(), "m", "sys", "summarize"); err != nil || text != "a summary" {
		t.Fatalf("got %q, %v", text, err)
	}
	conversation, err := p.Seed(context.Background(), "m", "we did things")
	if err != nil || conversation.Provider != "local" || len(conversation.Messages) != 2 {
		t.Fatalf("got %+v, %v", conversation, err)
	}
	models, err := p.ListModels(context.Background())
	if err != nil || strings.Join(models, " ") != "qwen3-coder llama3.2:latest" {
		t.Fatalf("got %v, %v", models, err)
	}
}

func TestEntryProvidersDoNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
	}))
	t.Cleanup(target.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)

	entriesTest(t, `{"chat": {"api": "openai", "base_url": "`+server.URL+`/v1", "key_env": "ENTRY_KEY"},
		"messages": {"api": "anthropic", "base_url": "`+server.URL+`", "key_env": "ENTRY_KEY"}}`)
	t.Setenv("ENTRY_KEY", "entry-key")
	for _, name := range []string{"chat", "messages"} {
		p, err := NewEntry(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.Send(context.Background(), Request{Model: "m", UserText: "hi"}, nil); err == nil {
			t.Errorf("%s: expected the redirect to fail the request", name)
		}
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("the redirect target got %d requests", n)
	}
}

func TestAnthropicEntrySendsOnlyItsOwnKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-SECRET")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ant-token-SECRET")
	var header http.Header
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		header = r.Header.Clone()
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"glm-5","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			`{"type":"message_stop"}`,
		} {
			var typed struct{ Type string }
			json.Unmarshal([]byte(event), &typed)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typed.Type, event)
		}
	}))
	t.Cleanup(server.Close)

	entriesTest(t, `{"glm": {"api": "anthropic", "base_url": "`+server.URL+`", "key_env": "GLM_KEY"}}`)
	t.Setenv("GLM_KEY", "glm-key")
	p, err := NewEntry("glm")
	if err != nil {
		t.Fatal(err)
	}
	resp, _, err := p.Send(context.Background(), Request{Model: "glm-5", UserText: "hi", Effort: EffortHigh}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "hello" || resp.Conversation.Provider != "glm" {
		t.Fatalf("got %+v", resp)
	}
	if header.Get("X-Api-Key") != "glm-key" || header.Get("Authorization") != "Bearer glm-key" {
		t.Fatalf("expected the entry's key, got %v", header)
	}
	if body["thinking"] != nil || body["output_config"] != nil || body["fallbacks"] != nil {
		t.Fatalf("expected no Claude-only settings for another model, got %v", body)
	}
}

func TestChatProviderNeverRunsAToolCallCutOffByTheLimit(t *testing.T) {
	server := newChatServer(t, []string{
		chunk(`{"tool_calls":[{"index":0,"id":"x1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"a.go\",\"con"}}]}`, ""),
		chunk(`{"tool_calls":[{"index":1,"id":"x2","type":"function","function":{"name":"run_bash","arguments":""}}]}`, "length"),
	})
	p := newChatProvider("local", server.URL+"/v1", "")
	resp, _, err := p.Send(context.Background(), Request{Model: "m", UserText: "hi"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Stop != StopMaxTokens {
		t.Fatalf("expected max tokens, got %s", resp.Stop)
	}
	for _, call := range resp.ToolCalls {
		if call.Arguments == "{}" {
			t.Fatalf("a call with no arguments must not be given empty ones: %+v", resp.ToolCalls)
		}
	}
}

func TestChatProviderFollowsCallsThatShareIndexZero(t *testing.T) {
	server := newChatServer(t, []string{
		chunk(`{"tool_calls":[{"index":0,"id":"x1","function":{"name":"a","arguments":"{\"p\":"}}]}`, ""),
		chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}`, ""),
		chunk(`{"tool_calls":[{"index":0,"id":"x2","function":{"name":"b","arguments":"{\"q\":"}}]}`, ""),
		chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"2}"}}]}`, "tool_calls"),
	})
	p := newChatProvider("local", server.URL+"/v1", "")
	resp, _, err := p.Send(context.Background(), Request{Model: "m", UserText: "hi"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []ToolCall{{ID: "x1", Name: "a", Arguments: `{"p":1}`}, {ID: "x2", Name: "b", Arguments: `{"q":2}`}}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0] != want[0] || resp.ToolCalls[1] != want[1] {
		t.Fatalf("got %+v", resp.ToolCalls)
	}
}

func TestChatProviderRetriesOnlyTransientStreamErrors(t *testing.T) {
	for _, test := range []struct {
		event string
		retry bool
	}{
		{`{"error":{"message":"Rate limit reached. Please try again in 7s.","type":"requests","code":"rate_limit_exceeded"}}`, true},
		{`{"error":{"message":"This model's maximum context length is 8192 tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`, false},
	} {
		server := newChatServer(t, []string{test.event})
		p := newChatProvider("local", server.URL+"/v1", "")
		_, _, err := p.Send(context.Background(), Request{Model: "m", UserText: "hi"}, nil)
		if err == nil {
			t.Fatal("expected the stream's error")
		}
		delay, retry := p.RetryDelay(err, 0)
		if retry != test.retry || retry && delay != 7*time.Second {
			t.Errorf("%s: got retry %v after %s", test.event, retry, delay)
		}
	}
}

func TestChatProviderReplaysReasoning(t *testing.T) {
	server := newChatServer(t,
		[]string{
			chunk(`{"reasoning_content":"I should read "}`, ""),
			chunk(`{"reasoning_content":"the file."}`, ""),
			chunk(`{"tool_calls":[{"index":0,"id":"x1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}`, "tool_calls"),
		},
		[]string{chunk(`{"content":"ok"}`, "stop")},
	)
	p := newChatProvider("local", server.URL+"/v1", "")
	resp, _, err := p.Send(context.Background(), Request{Model: "m", UserText: "hi"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Send(context.Background(), Request{Model: "m", Conversation: resp.Conversation, ToolResults: []ToolResult{{CallID: "x1", Output: "x"}}}, nil); err != nil {
		t.Fatal(err)
	}
	messages := server.bodies[1]["messages"].([]any)
	assistant := messages[1].(map[string]any)
	if assistant["reasoning_content"] != "I should read the file." || assistant["reasoning"] != nil {
		t.Fatalf("expected the reasoning sent back as it came, got %v", assistant)
	}
	if first := server.bodies[0]["messages"].([]any)[0].(map[string]any); first["reasoning_content"] != nil {
		t.Fatalf("a user message carries no reasoning: %v", first)
	}
}

func TestChatProviderGivesEachResponseItsOwnID(t *testing.T) {
	empty := `{"id":"","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"a"},"finish_reason":"stop"}]}`
	server := newChatServer(t, []string{chunk(`{"content":"a"}`, "stop")}, []string{chunk(`{"content":"b"}`, "stop")}, []string{empty})
	p := newChatProvider("local", server.URL+"/v1", "")
	seen := map[string]bool{}
	for range 3 {
		resp, _, err := p.Send(context.Background(), Request{Model: "m", UserText: "hi"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.ID == "" || seen[resp.ID] {
			t.Fatalf("got a repeated or empty ID %q", resp.ID)
		}
		seen[resp.ID] = true
	}
}

func TestChatProviderListsModelsFromABareArray(t *testing.T) {
	for body, want := range map[string]string{
		`[{"id":"meta-llama/Llama-4","object":"model"},{"id":"qwen/Qwen3"}]`: "meta-llama/Llama-4 qwen/Qwen3",
		`{"object":"list","data":[{"id":"a"},{"id":""}]}`:                    "a",
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, body)
		}))
		models, err := newChatProvider("together", server.URL+"/v1", "k").ListModels(context.Background())
		server.Close()
		if err != nil || strings.Join(models, " ") != want {
			t.Fatalf("%s: got %v, %v", body, models, err)
		}
	}
}
