package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/azs06/goat/internal/provider"
)

const deepSeekModelList = `{"object":"list","data":[{"id":"deepseek-v4-pro","object":"model","owned_by":"deepseek","max_output_tokens":32000},{"id":"deepseek-flash","object":"model","owned_by":"deepseek"}]}`

func deepSeekThinking(s *sse, text string) *sse {
	// DeepSeek's thinking blocks carry the reasoning text and may have no signature.
	s.event("content_block_start", `{"type":"content_block_start","index":`+itoa(s.index)+`,"content_block":{"type":"thinking","thinking":""}}`)
	s.event("content_block_delta", `{"type":"content_block_delta","index":`+itoa(s.index)+`,"delta":{"type":"thinking_delta","thinking":`+quote(text)+`}}`)
	s.event("content_block_stop", `{"type":"content_block_stop","index":`+itoa(s.index)+`}`)
	s.index++
	return s
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func quote(text string) string {
	b, _ := json.Marshal(text)
	return string(b)
}

func newDeepSeekTestProvider(fake *fakeAnthropic) provider.Provider {
	return provider.NewDeepSeekFor(fake.server.URL+"/anthropic", "ds-key")
}

func TestDeepSeekRequestsAndReasoningReplay(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)
	// Credentials meant for Anthropic must never reach DeepSeek.
	// With only the auth token set, the SDK's defaults would send it as a bearer token.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "anthropic-token")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")

	keepAlive := ": keep-alive\n\n"
	fake := newFakeAnthropic(t,
		keepAlive+deepSeekThinking(newSSE("ds_1"), "Need to list files.").toolUse("call_1", "read_dir", `{"path":"."}`).stop("tool_use", ""),
		deepSeekThinking(newSSE("ds_2"), "Now answer.").text("Two files.").stop("end_turn", ""),
		newSSE("ds_3").text("Bye.").stop("end_turn", ""),
	)
	fake.deepseekModels = deepSeekModelList
	p := newDeepSeekTestProvider(fake)

	conv, err := sendPromptStream(context.Background(), p, "list files", conversationState{}, "deepseek-v4-pro", provider.EffortMedium)
	if err != nil {
		t.Fatalf("sendPromptStream: %v", err)
	}
	if conv.conversation.Provider != provider.DeepSeek {
		t.Fatalf("expected a deepseek conversation, got %q", conv.conversation.Provider)
	}

	var messageRequests []recordedRequest
	for i := 0; i < fake.requestCount(); i++ {
		if req := fake.request(i); req.body != nil {
			messageRequests = append(messageRequests, req)
		}
	}
	first := messageRequests[0]
	if got := first.header.Get("X-Api-Key"); got != "ds-key" {
		t.Errorf("expected the DeepSeek key, got %q", got)
	}
	if auth := first.header.Get("Authorization"); auth != "" {
		t.Errorf("expected no Anthropic bearer token to be sent, got %q", auth)
	}
	if _, ok := first.body["thinking"]; ok {
		t.Errorf("expected thinking to be left at DeepSeek's default, got %v", first.body["thinking"])
	}
	if effort := first.body["output_config"].(map[string]any)["effort"]; effort != "high" {
		t.Errorf("expected medium to be sent as high, got %v", effort)
	}
	for _, field := range []string{"fallbacks", "cache_control"} {
		if _, ok := first.body[field]; ok {
			t.Errorf("expected no %s for DeepSeek", field)
		}
	}
	if first.body["max_tokens"].(float64) != 32000 {
		t.Errorf("expected max_tokens from DeepSeek's model list, got %v", first.body["max_tokens"])
	}

	replayed := messagesOf(t, messageRequests[1])[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if replayed["type"] != "thinking" || replayed["thinking"] != "Need to list files." {
		t.Fatalf("expected the reasoning to be sent back, got %v", replayed)
	}

	// The next user turn must still carry every earlier turn's reasoning.
	if _, err := sendPromptStream(context.Background(), p, "thanks", conv, "deepseek-v4-pro", provider.EffortMedium); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	third := messagesOf(t, messageRequests[0])
	_ = third
	last := fake.request(fake.requestCount() - 1)
	messages := messagesOf(t, last)
	thinkingTexts := []string{}
	for _, message := range messages {
		if message.(map[string]any)["role"] != "assistant" {
			continue
		}
		for _, block := range message.(map[string]any)["content"].([]any) {
			if block.(map[string]any)["type"] == "thinking" {
				thinkingTexts = append(thinkingTexts, block.(map[string]any)["thinking"].(string))
			}
		}
	}
	if strings.Join(thinkingTexts, "|") != "Need to list files.|Now answer." {
		t.Fatalf("expected all earlier reasoning to be replayed, got %v", thinkingTexts)
	}
}

func TestDeepSeekEffortMapping(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	cases := map[provider.Effort]string{provider.EffortMinimal: "low", provider.EffortLow: "low", provider.EffortHigh: "high", provider.EffortXhigh: "high", provider.EffortMax: "max"}
	for effort, want := range cases {
		fake := newFakeAnthropic(t, newSSE("ds").text("ok").stop("end_turn", ""))
		if _, err := sendPromptStream(context.Background(), newDeepSeekTestProvider(fake), "hi", conversationState{}, "deepseek-flash", effort); err != nil {
			t.Fatalf("%s: %v", effort, err)
		}
		if got := fake.request(0).body["output_config"].(map[string]any)["effort"]; got != want {
			t.Errorf("%s: expected %s, got %v", effort, want, got)
		}
	}

	fake := newFakeAnthropic(t, newSSE("ds").text("ok").stop("end_turn", ""))
	if _, err := sendPromptStream(context.Background(), newDeepSeekTestProvider(fake), "hi", conversationState{}, "deepseek-flash", provider.EffortNone); err != nil {
		t.Fatalf("none: %v", err)
	}
	body := fake.request(0).body
	if thinking := body["thinking"].(map[string]any); thinking["type"] != "disabled" {
		t.Errorf("expected effort none to turn thinking off, got %v", thinking)
	}
	if _, ok := body["output_config"]; ok {
		t.Error("expected no effort when thinking is off")
	}
}

func TestDeepSeekBusyServerIsRetried(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	previousSleeper := streamRetrySleeper
	streamRetrySleeper = func(ctx context.Context, delay time.Duration) error { return nil }
	t.Cleanup(func() { streamRetrySleeper = previousSleeper })

	fake := newFakeAnthropic(t,
		newSSE("ds_1").stop("insufficient_system_resource", ""),
		newSSE("ds_2").text("ok").stop("end_turn", ""),
	)
	conv, err := sendPromptStream(context.Background(), newDeepSeekTestProvider(fake), "hi", conversationState{}, "deepseek-flash", provider.EffortHigh)
	if err != nil {
		t.Fatalf("expected a retry, got %v", err)
	}
	if fake.requestCount() != 2 || len(conv.conversation.Messages) != 2 {
		t.Fatalf("expected the busy response to be retried and discarded, got %d requests, %d messages", fake.requestCount(), len(conv.conversation.Messages))
	}
}

func TestDeepSeekModelsAndCredentials(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)
	setCredentials(t, false, false)
	t.Setenv("DEEPSEEK_API_KEY", "ds-key")

	choices := provider.ModelChoices(provider.Model{}, nil)
	if len(choices) == 0 {
		t.Fatal("expected DeepSeek models")
	}
	for _, choice := range choices {
		if choice.Provider != provider.DeepSeek {
			t.Fatalf("expected only DeepSeek models with only a DeepSeek key, got %v", choice)
		}
	}
	if model := provider.DefaultModel(); model != (provider.Model{Provider: provider.DeepSeek, ID: provider.DefaultDeepSeekModel}) {
		t.Fatalf("expected the DeepSeek default, got %v", model)
	}
	t.Setenv("GOAT_MODEL", "deepseek-flash")
	if model, _ := startupModel(nil); model != (provider.Model{Provider: provider.DeepSeek, ID: "deepseek-flash"}) {
		t.Fatalf("expected GOAT_MODEL to pick DeepSeek, got %v", model)
	}

	fake := newFakeAnthropic(t)
	fake.deepseekModels = deepSeekModelList
	ids, err := newDeepSeekTestProvider(fake).ListModels(context.Background())
	if err != nil || strings.Join(ids, ",") != "deepseek-v4-pro,deepseek-flash" {
		t.Fatalf("expected DeepSeek's model list, got %v (%v)", ids, err)
	}
	if auth := fake.request(0).header.Get("Authorization"); auth != "Bearer ds-key" {
		t.Fatalf("expected the DeepSeek key on the model list, got %q", auth)
	}
}

func TestDeepSeekReasoningRejectionTriggersRecovery(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	fake := newFakeAnthropic(t)
	fake.reject = func(body map[string]any) (int, string, bool) {
		return http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"The reasoning_content in the thinking mode must be passed back to the API."}}`, true
	}
	_, _, err := newDeepSeekTestProvider(fake).Send(context.Background(), provider.Request{Model: "deepseek-flash", UserText: "hi"}, nil)
	if !provider.IsThinkingBindingError(err) {
		t.Fatalf("expected the rejection to be recognised, got %v", err)
	}
}

func TestDeepSeekUnknownModelIsRefused(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)
	setCredentials(t, false, false)
	t.Setenv("DEEPSEEK_API_KEY", "ds-key")

	fake := newFakeAnthropic(t)
	fake.deepseekModels = deepSeekModelList
	cfg := &config{model: provider.Model{Provider: provider.DeepSeek, ID: "deepseek-v4-pro"}, reasoningEffort: provider.EffortHigh, providers: map[string]provider.Provider{provider.DeepSeek: newDeepSeekTestProvider(fake)}}

	err := commandModel(context.Background(), cfg, []string{"deepseek-v4-por"})
	if err == nil || !strings.Contains(err.Error(), "no model named") {
		t.Fatalf("expected a typo to be refused, got %v", err)
	}
	if err := commandModel(context.Background(), cfg, []string{"deepseek-flash"}); err != nil {
		t.Fatalf("expected a listed model to be accepted, got %v", err)
	}
}

func TestStreamDroppedAfterKeepAliveOrPingIsRetried(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	previousSleeper := streamRetrySleeper
	streamRetrySleeper = func(ctx context.Context, delay time.Duration) error { return nil }
	t.Cleanup(func() { streamRetrySleeper = previousSleeper })

	for name, prelude := range map[string]string{"keep-alive": ": keep-alive\n\n", "ping": "event: ping\ndata: {\"type\":\"ping\"}\n\n"} {
		fake := newFakeAnthropic(t, "unused", newSSE("ds_2").text("ok").stop("end_turn", ""))
		fake.dropAfter = prelude
		if _, err := sendPromptStream(context.Background(), newDeepSeekTestProvider(fake), "hi", conversationState{}, "deepseek-flash", provider.EffortHigh); err != nil {
			t.Fatalf("%s: expected the dropped stream to be retried, got %v", name, err)
		}
	}
}
