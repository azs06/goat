package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/azs06/goat/internal/provider"
)

func chdirForTest(t *testing.T, dir string) {
	t.Helper()

	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working dir: %v", err)
	}

	if err := os.Chdir(dir); err != nil {
		t.Fatalf("change working dir: %v", err)
	}

	previousConversationID := activeConversationID
	activeConversationID = ""
	t.Cleanup(func() {
		activeConversationID = previousConversationID
		if err := os.Chdir(previousDir); err != nil {
			t.Fatalf("restore working dir: %v", err)
		}
	})
}

func mustResponseOutputItemUnion(t *testing.T, raw string) responses.ResponseOutputItemUnion {
	t.Helper()

	var item responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("unmarshal response output item: %v", err)
	}

	return item
}

func TestSendPromptStreamContinuesThroughToolCallRounds(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	responsesToReturn := []*responses.Response{
		{
			ID: "resp-1",
			Output: []responses.ResponseOutputItemUnion{
				mustResponseOutputItemUnion(t, `{"type":"function_call","name":"read_dir","call_id":"call-1","arguments":"{\"path\":\".\"}"}`),
			},
		},
		{
			ID: "resp-2",
			Output: []responses.ResponseOutputItemUnion{
				mustResponseOutputItemUnion(t, `{"type":"function_call","name":"read_dir","call_id":"call-2","arguments":"{\"path\":\".\"}"}`),
			},
		},
		{
			ID: "resp-3",
			Output: []responses.ResponseOutputItemUnion{
				mustResponseOutputItemUnion(t, `{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Done."}]}`),
			},
		},
	}

	var receivedParams []responses.ResponseNewParams
	stream := func(ctx context.Context, params responses.ResponseNewParams, onText func(string)) (*responses.Response, bool, error) {
		receivedParams = append(receivedParams, params)
		index := len(receivedParams) - 1
		if index >= len(responsesToReturn) {
			t.Fatalf("unexpected streamed response request %d", index+1)
		}
		return responsesToReturn[index], false, nil
	}

	conv, err := sendPromptStream(context.Background(), provider.NewOpenAIWith(nil, stream), "list files twice", conversationState{}, turnOptions{model: provider.DefaultOpenAIModel, effort: provider.DefaultEffort})
	if err != nil {
		t.Fatalf("send prompt stream: %v", err)
	}

	if conv.conversation.ResponseID != "resp-3" {
		t.Fatalf("expected final response ID resp-3, got %q", conv.conversation.ResponseID)
	}

	if len(receivedParams) != 3 {
		t.Fatalf("expected 3 streamed response requests, got %d", len(receivedParams))
	}

	if receivedParams[0].PreviousResponseID.Value != "" {
		t.Fatalf("expected initial request to omit previous response ID, got %q", receivedParams[0].PreviousResponseID.Value)
	}

	if receivedParams[1].PreviousResponseID.Value != "resp-1" {
		t.Fatalf("expected second request to continue from resp-1, got %q", receivedParams[1].PreviousResponseID.Value)
	}

	if receivedParams[2].PreviousResponseID.Value != "resp-2" {
		t.Fatalf("expected third request to continue from resp-2, got %q", receivedParams[2].PreviousResponseID.Value)
	}

	if len(receivedParams[1].Input.OfInputItemList) != 1 {
		t.Fatalf("expected one tool output in second request, got %d", len(receivedParams[1].Input.OfInputItemList))
	}

	secondToolOutputJSON, err := json.Marshal(receivedParams[1].Input.OfInputItemList[0])
	if err != nil {
		t.Fatalf("marshal second tool output: %v", err)
	}

	if !strings.Contains(string(secondToolOutputJSON), `"call_id":"call-1"`) {
		t.Fatalf("expected second request to include tool output for call-1, got %s", secondToolOutputJSON)
	}

	if len(receivedParams[2].Input.OfInputItemList) != 1 {
		t.Fatalf("expected one tool output in third request, got %d", len(receivedParams[2].Input.OfInputItemList))
	}

	thirdToolOutputJSON, err := json.Marshal(receivedParams[2].Input.OfInputItemList[0])
	if err != nil {
		t.Fatalf("marshal third tool output: %v", err)
	}

	if !strings.Contains(string(thirdToolOutputJSON), `"call_id":"call-2"`) {
		t.Fatalf("expected third request to include tool output for call-2, got %s", thirdToolOutputJSON)
	}
}

func TestSendPromptStreamWritesConversationHistory(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	stream := func(ctx context.Context, params responses.ResponseNewParams, onText func(string)) (*responses.Response, bool, error) {
		return &responses.Response{
			ID: "resp-history",
			Output: []responses.ResponseOutputItemUnion{
				mustResponseOutputItemUnion(t, `{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"History saved."}]}`),
			},
		}, false, nil
	}

	conv, err := sendPromptStream(context.Background(), provider.NewOpenAIWith(nil, stream), "hello history", conversationState{}, turnOptions{model: provider.DefaultOpenAIModel, effort: provider.DefaultEffort})
	if err != nil {
		t.Fatalf("send prompt stream: %v", err)
	}
	if conv.conversation.ResponseID != "resp-history" {
		t.Fatalf("expected resp-history, got %q", conv.conversation.ResponseID)
	}

	historyData, err := os.ReadFile(filepath.Join(statePath(t, ""), "history.jsonl"))
	if err != nil {
		t.Fatalf("read history file: %v", err)
	}

	historyText := string(historyData)
	if !strings.Contains(historyText, `"type":"user"`) || !strings.Contains(historyText, `"content":"hello history"`) {
		t.Fatalf("expected user prompt in history, got %s", historyText)
	}
	if !strings.Contains(historyText, `"type":"assistant"`) || !strings.Contains(historyText, `"response_id":"resp-history"`) {
		t.Fatalf("expected assistant response in history, got %s", historyText)
	}
}

func TestSendPromptStreamRetriesRateLimitErrors(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	previousSleeper := streamRetrySleeper
	defer func() {
		streamRetrySleeper = previousSleeper
	}()

	var callCount int
	stream := func(ctx context.Context, params responses.ResponseNewParams, onText func(string)) (*responses.Response, bool, error) {
		callCount++
		if callCount == 1 {
			return nil, false, &ssestream.StreamError{
				Message: "received error while streaming: rate limit",
				Event: ssestream.Event{
					Data: []byte(`{"error":{"type":"tokens","code":"rate_limit_exceeded","message":"Rate limit reached. Please try again in 1.022s.","param":null}}`),
				},
			}
		}

		return &responses.Response{
			ID: "resp-after-retry",
			Output: []responses.ResponseOutputItemUnion{
				mustResponseOutputItemUnion(t, `{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Recovered."}]}`),
			},
		}, false, nil
	}

	var slept []time.Duration
	streamRetrySleeper = func(ctx context.Context, delay time.Duration) error {
		slept = append(slept, delay)
		return nil
	}

	conv, err := sendPromptStream(context.Background(), provider.NewOpenAIWith(nil, stream), "retry please", conversationState{}, turnOptions{model: provider.DefaultOpenAIModel, effort: provider.DefaultEffort})
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}

	if conv.conversation.ResponseID != "resp-after-retry" {
		t.Fatalf("expected resp-after-retry, got %q", conv.conversation.ResponseID)
	}
	if callCount != 2 {
		t.Fatalf("expected 2 streamed calls, got %d", callCount)
	}
	if len(slept) != 1 {
		t.Fatalf("expected 1 sleep, got %d", len(slept))
	}
	if slept[0] < time.Second || slept[0] > 1100*time.Millisecond {
		t.Fatalf("expected parsed retry delay around 1.022s, got %s", slept[0])
	}
}

func TestSendPromptStreamReturnsErrorAfterPartialOutputInterruption(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	var callCount int
	stream := func(ctx context.Context, params responses.ResponseNewParams, onText func(string)) (*responses.Response, bool, error) {
		callCount++
		return nil, true, &ssestream.StreamError{
			Message: "received error while streaming: rate limit",
			Event: ssestream.Event{
				Data: []byte(`{"error":{"type":"tokens","code":"rate_limit_exceeded","message":"Rate limit reached. Please try again in 1.022s.","param":null}}`),
			},
		}
	}

	conv, err := sendPromptStream(context.Background(), provider.NewOpenAIWith(nil, stream), "partial retry", conversationState{conversation: provider.Conversation{Provider: provider.OpenAI, ResponseID: "resp-prev"}}, turnOptions{model: provider.DefaultOpenAIModel, effort: provider.DefaultEffort})
	if err == nil {
		t.Fatal("expected interrupted partial output to return an error")
	}
	if !strings.Contains(err.Error(), "interrupted after partial text") {
		t.Fatalf("expected partial-output error, got %v", err)
	}
	if conv.conversation.ResponseID != "resp-prev" {
		t.Fatalf("expected response ID to remain unchanged, got %q", conv.conversation.ResponseID)
	}
	if callCount != 1 {
		t.Fatalf("expected no retry after partial output, got %d call(s)", callCount)
	}
}

func TestSendPromptStreamReturnsErrorAfterRateLimitRetriesExhausted(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	previousSleeper := streamRetrySleeper
	defer func() {
		streamRetrySleeper = previousSleeper
	}()

	var callCount int
	stream := func(ctx context.Context, params responses.ResponseNewParams, onText func(string)) (*responses.Response, bool, error) {
		callCount++
		return nil, false, &ssestream.StreamError{
			Message: "received error while streaming: rate limit",
			Event: ssestream.Event{
				Data: []byte(`{"error":{"type":"tokens","code":"rate_limit_exceeded","message":"Rate limit reached. Please try again in 0.500s.","param":null}}`),
			},
		}
	}

	var slept []time.Duration
	streamRetrySleeper = func(ctx context.Context, delay time.Duration) error {
		slept = append(slept, delay)
		return nil
	}

	conv, err := sendPromptStream(context.Background(), provider.NewOpenAIWith(nil, stream), "still limited", conversationState{conversation: provider.Conversation{Provider: provider.OpenAI, ResponseID: "resp-prev"}}, turnOptions{model: provider.DefaultOpenAIModel, effort: provider.DefaultEffort})
	if err == nil {
		t.Fatal("expected rate-limit exhaustion to return an error")
	}
	if !strings.Contains(err.Error(), "request failed after") {
		t.Fatalf("expected exhausted-retries error, got %v", err)
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("expected original rate-limit detail to be preserved, got %v", err)
	}
	if conv.conversation.ResponseID != "resp-prev" {
		t.Fatalf("expected response ID to remain unchanged, got %q", conv.conversation.ResponseID)
	}
	if callCount != maxStreamRateLimitRetries+1 {
		t.Fatalf("expected %d streamed calls, got %d", maxStreamRateLimitRetries+1, callCount)
	}
	if len(slept) != maxStreamRateLimitRetries {
		t.Fatalf("expected %d sleeps, got %d", maxStreamRateLimitRetries, len(slept))
	}
}

func TestSendPromptStreamTruncatesLargeRunBashToolOutputForModel(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	command := "i=1; while [ $i -le 260 ]; do printf 'line-%03d\\n' \"$i\"; i=$((i+1)); done"
	argumentsJSON, err := json.Marshal(map[string]string{
		"command": command,
		"workdir": ".",
	})
	if err != nil {
		t.Fatalf("marshal run_bash arguments: %v", err)
	}

	responsesToReturn := []*responses.Response{
		{
			ID: "resp-1",
			Output: []responses.ResponseOutputItemUnion{
				mustResponseOutputItemUnion(t, fmt.Sprintf(`{"type":"function_call","name":"run_bash","call_id":"call-1","arguments":%q}`, string(argumentsJSON))),
			},
		},
		{
			ID: "resp-2",
			Output: []responses.ResponseOutputItemUnion{
				mustResponseOutputItemUnion(t, `{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Done."}]}`),
			},
		},
	}

	var receivedParams []responses.ResponseNewParams
	stream := func(ctx context.Context, params responses.ResponseNewParams, onText func(string)) (*responses.Response, bool, error) {
		receivedParams = append(receivedParams, params)
		index := len(receivedParams) - 1
		if index >= len(responsesToReturn) {
			t.Fatalf("unexpected streamed response request %d", index+1)
		}
		return responsesToReturn[index], false, nil
	}

	conv, err := sendPromptStream(context.Background(), provider.NewOpenAIWith(nil, stream), "run a large bash command", conversationState{}, turnOptions{model: provider.DefaultOpenAIModel, effort: provider.DefaultEffort})
	if err != nil {
		t.Fatalf("sendPromptStream returned error: %v", err)
	}
	if conv.conversation.ResponseID != "resp-2" {
		t.Fatalf("expected resp-2, got %q", conv.conversation.ResponseID)
	}
	if len(receivedParams) != 2 {
		t.Fatalf("expected 2 streamed response requests, got %d", len(receivedParams))
	}
	if len(receivedParams[1].Input.OfInputItemList) != 1 {
		t.Fatalf("expected one tool output in second request, got %d", len(receivedParams[1].Input.OfInputItemList))
	}

	toolOutputJSON, err := json.Marshal(receivedParams[1].Input.OfInputItemList[0])
	if err != nil {
		t.Fatalf("marshal tool output: %v", err)
	}

	toolOutputText := string(toolOutputJSON)
	if !strings.Contains(toolOutputText, `"call_id":"call-1"`) {
		t.Fatalf("expected second request to include tool output for call-1, got %s", toolOutputJSON)
	}
	if !strings.Contains(toolOutputText, "omitted for token safety") {
		t.Fatalf("expected tool output to be truncated for token safety, got %s", toolOutputJSON)
	}
	if !strings.Contains(toolOutputText, "line-001") || !strings.Contains(toolOutputText, "line-260") {
		t.Fatalf("expected truncated output to preserve the start and end of the command output, got %s", toolOutputJSON)
	}
	if strings.Contains(toolOutputText, "line-130") || strings.Contains(toolOutputText, "line-131") {
		t.Fatalf("expected the middle of the command output to be omitted, got %s", toolOutputJSON)
	}
}

func TestSendPromptStreamKeepsUnansweredToolOutputsWhenFollowupFails(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	argumentsJSON, err := json.Marshal(map[string]string{"path": "."})
	if err != nil {
		t.Fatalf("marshal read_dir arguments: %v", err)
	}

	var receivedParams []responses.ResponseNewParams
	stream := func(ctx context.Context, params responses.ResponseNewParams, onText func(string)) (*responses.Response, bool, error) {
		receivedParams = append(receivedParams, params)
		switch len(receivedParams) {
		case 1:
			return &responses.Response{
				ID: "resp-tool-round",
				Output: []responses.ResponseOutputItemUnion{
					mustResponseOutputItemUnion(t, fmt.Sprintf(`{"type":"function_call","name":"read_dir","call_id":"call-1","arguments":%q}`, string(argumentsJSON))),
				},
			}, false, nil
		case 2:
			return nil, false, fmt.Errorf("upstream failure")
		default:
			return &responses.Response{
				ID: "resp-recovered",
				Output: []responses.ResponseOutputItemUnion{
					mustResponseOutputItemUnion(t, `{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Recovered."}]}`),
				},
			}, false, nil
		}
	}

	conv, err := sendPromptStream(context.Background(), provider.NewOpenAIWith(nil, stream), "list files", conversationState{conversation: provider.Conversation{Provider: provider.OpenAI, ResponseID: "resp-prev"}}, turnOptions{model: provider.DefaultOpenAIModel, effort: provider.DefaultEffort})
	if err == nil {
		t.Fatal("expected follow-up failure to be returned")
	}
	if conv.conversation.ResponseID != "resp-tool-round" {
		t.Fatalf("expected latest successful response ID, got %q", conv.conversation.ResponseID)
	}
	if len(conv.pendingToolResults) != 1 {
		t.Fatalf("expected the undelivered tool output to be kept, got %d", len(conv.pendingToolResults))
	}

	conv, err = sendPromptStream(context.Background(), provider.NewOpenAIWith(nil, stream), "try again", conv, turnOptions{model: provider.DefaultOpenAIModel, effort: provider.DefaultEffort})
	if err != nil {
		t.Fatalf("expected the next prompt to succeed, got %v", err)
	}
	if conv.conversation.ResponseID != "resp-recovered" || len(conv.pendingToolResults) != 0 {
		t.Fatalf("expected a clean state after recovery, got %+v", conv)
	}

	retry := receivedParams[2]
	if retry.PreviousResponseID.Value != "resp-tool-round" {
		t.Fatalf("expected retry to continue from resp-tool-round, got %q", retry.PreviousResponseID.Value)
	}
	items := retry.Input.OfInputItemList
	if len(items) != 2 {
		t.Fatalf("expected pending tool output plus the new prompt, got %d item(s)", len(items))
	}
	itemsJSON, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal retry input: %v", err)
	}
	if !strings.Contains(string(itemsJSON), `"call_id":"call-1"`) || !strings.Contains(string(itemsJSON), "try again") {
		t.Fatalf("expected tool output for call-1 and the new prompt, got %s", itemsJSON)
	}
}

func TestSendPromptStreamAnswersRemainingCallsWhenInterrupted(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	ctx, cancel := context.WithCancel(context.Background())
	callCount := 0
	stream := func(ctx context.Context, params responses.ResponseNewParams, onText func(string)) (*responses.Response, bool, error) {
		callCount++
		cancel()
		return &responses.Response{
			ID: "resp-calls",
			Output: []responses.ResponseOutputItemUnion{
				mustResponseOutputItemUnion(t, `{"type":"function_call","name":"read_dir","call_id":"call-1","arguments":"{\"path\":\".\"}"}`),
				mustResponseOutputItemUnion(t, `{"type":"function_call","name":"read_dir","call_id":"call-2","arguments":"{\"path\":\".\"}"}`),
			},
		}, false, nil
	}

	conv, err := sendPromptStream(ctx, provider.NewOpenAIWith(nil, stream), "list", conversationState{}, turnOptions{model: provider.DefaultOpenAIModel, effort: provider.DefaultEffort})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected no follow-up request after interruption, got %d request(s)", callCount)
	}
	if conv.conversation.ResponseID != "resp-calls" || len(conv.pendingToolResults) != 2 {
		t.Fatalf("expected both calls to be answered and kept pending, got %+v", conv)
	}
}

func TestSendPromptStreamStopsAfterMaxToolRounds(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	callCount := 0
	stream := func(ctx context.Context, params responses.ResponseNewParams, onText func(string)) (*responses.Response, bool, error) {
		callCount++
		return &responses.Response{
			ID: fmt.Sprintf("resp-%d", callCount),
			Output: []responses.ResponseOutputItemUnion{
				mustResponseOutputItemUnion(t, fmt.Sprintf(`{"type":"function_call","name":"read_dir","call_id":"call-%d","arguments":"{\"path\":\".\"}"}`, callCount)),
			},
		}, false, nil
	}

	conv, err := sendPromptStream(context.Background(), provider.NewOpenAIWith(nil, stream), "loop forever", conversationState{}, turnOptions{model: provider.DefaultOpenAIModel, effort: provider.DefaultEffort})
	if err == nil || !strings.Contains(err.Error(), "tool rounds") {
		t.Fatalf("expected tool round limit error, got %v", err)
	}
	if callCount != maxToolRoundsPerTurn {
		t.Fatalf("expected %d requests, got %d", maxToolRoundsPerTurn, callCount)
	}
	if len(conv.pendingToolResults) != 1 {
		t.Fatalf("expected the last round's output to stay pending, got %d", len(conv.pendingToolResults))
	}
}

func TestCommandCompactCreatesFreshResponseChain(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	recordHistory(historyEntry{Type: "user", Content: "Please fix the rate-limit handling."})
	recordHistory(historyEntry{Type: "assistant", Content: "Working on it.", ResponseID: "resp-old"})
	recordHistory(historyEntry{Type: "tool_call", ToolName: "read_file", Arguments: "{\"path\":\"main.go\"}", ResponseID: "resp-old"})
	recordHistory(historyEntry{Type: "tool_output", ToolName: "read_file", Content: "package main", ResponseID: "resp-old", Status: "ok"})

	var receivedParams []responses.ResponseNewParams
	create := func(ctx context.Context, params responses.ResponseNewParams) (*responses.Response, error) {
		receivedParams = append(receivedParams, params)
		switch len(receivedParams) {
		case 1:
			return &responses.Response{
				ID: "resp-summary",
				Output: []responses.ResponseOutputItemUnion{
					mustResponseOutputItemUnion(t, `{"type":"message","id":"msg-sum","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Goal: finish the rate-limit fix. Files changed: main.go. Next step: verify retries and compact the session."}]}`),
				},
			}, nil
		case 2:
			return &responses.Response{
				ID: "resp-seeded",
				Output: []responses.ResponseOutputItemUnion{
					mustResponseOutputItemUnion(t, `{"type":"message","id":"msg-seed","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Context loaded."}]}`),
				},
			}, nil
		default:
			t.Fatalf("unexpected create call %d", len(receivedParams))
			return nil, nil
		}
	}

	cfg := &config{
		model:           provider.Model{Provider: provider.OpenAI, ID: provider.DefaultOpenAIModel},
		reasoningEffort: provider.DefaultEffort,
		conversation:    conversationState{conversation: provider.Conversation{Provider: provider.OpenAI, ResponseID: "resp-old"}},
		providers:       map[string]provider.Provider{provider.OpenAI: provider.NewOpenAIWith(create, nil)},
	}

	if err := commandCompact(context.Background(), cfg, nil); err != nil {
		t.Fatalf("commandCompact: %v", err)
	}
	if cfg.conversation.conversation.ResponseID != "resp-seeded" {
		t.Fatalf("expected compacted response ID resp-seeded, got %q", cfg.conversation.conversation.ResponseID)
	}
	if len(receivedParams) != 2 {
		t.Fatalf("expected 2 create calls, got %d", len(receivedParams))
	}
	if receivedParams[0].PreviousResponseID.Value != "" {
		t.Fatalf("expected local-history compaction summary to avoid previous response chaining, got %q", receivedParams[0].PreviousResponseID.Value)
	}
	if receivedParams[0].Reasoning.Effort != shared.ReasoningEffortLow {
		t.Fatalf("expected low reasoning for compaction summary, got %q", receivedParams[0].Reasoning.Effort)
	}
	if !strings.Contains(receivedParams[0].Input.OfString.Value, "Please fix the rate-limit handling.") {
		t.Fatalf("expected summary request to include local history, got %q", receivedParams[0].Input.OfString.Value)
	}
	if receivedParams[1].PreviousResponseID.Value != "" {
		t.Fatalf("expected seed request to start a fresh chain, got %q", receivedParams[1].PreviousResponseID.Value)
	}

	latestResponseID, err := loadLatestConversationResponseID()
	if err != nil {
		t.Fatalf("load latest response ID: %v", err)
	}
	if latestResponseID != "resp-seeded" {
		t.Fatalf("expected persisted compacted response ID resp-seeded, got %q", latestResponseID)
	}
}

func TestShouldAutoCompactConversationUsesLatestTokenUsage(t *testing.T) {
	entries := []historyEntry{
		{Type: "user", Content: "first"},
		{Type: "assistant", Content: "big", ResponseID: "resp-1", ContextTokens: 90000},
		{Type: "user", Content: "second"},
		{Type: "assistant", Content: "small", ResponseID: "resp-2", ContextTokens: 1000},
	}

	if shouldCompact, _ := shouldAutoCompactConversation(entries, 50000); shouldCompact {
		t.Fatal("expected only the latest turn's usage to count")
	}

	entries = append(entries, historyEntry{Type: "assistant", Content: "bigger", ResponseID: "resp-3", ContextTokens: 60000})
	shouldCompact, reason := shouldAutoCompactConversation(entries, 50000)
	if !shouldCompact {
		t.Fatal("expected auto compaction once usage passes the threshold")
	}
	if !strings.Contains(reason, "tokens") {
		t.Fatalf("expected token reason, got %q", reason)
	}
}

func TestShouldAutoCompactConversationIgnoresManySmallEntries(t *testing.T) {
	var entries []historyEntry
	for i := 0; i < 100; i++ {
		entries = append(entries, historyEntry{Type: "tool_output", ToolName: "read_dir", Content: "a.txt"})
	}

	if shouldCompact, reason := shouldAutoCompactConversation(entries, 50000); shouldCompact {
		t.Fatalf("expected small history not to compact, got %q", reason)
	}
}

func TestShouldAutoCompactConversationFallsBackToCharacters(t *testing.T) {
	entries := []historyEntry{{Type: "user", Content: strings.Repeat("x", 4000)}}

	shouldCompact, reason := shouldAutoCompactConversation(entries, 500)
	if !shouldCompact {
		t.Fatal("expected history without usage to compact by size")
	}
	if !strings.Contains(reason, "tokens") {
		t.Fatalf("expected token estimate reason, got %q", reason)
	}
}

func TestShouldAutoCompactConversationCountsOutputAfterLatestUsage(t *testing.T) {
	entries := []historyEntry{
		{Type: "user", Content: "first"},
		{Type: "assistant", Content: "ok", ResponseID: "resp-1", ContextTokens: 50000},
		{Type: "user", Content: "read everything"},
	}
	for i := 0; i < 40; i++ {
		responseID := fmt.Sprintf("resp-round-%d", i)
		entries = append(entries,
			historyEntry{Type: "tool_call", ToolName: "read_file", Arguments: `{"path":"big.txt"}`, ResponseID: responseID, ContextTokens: int64(50000 + i*100)},
			historyEntry{Type: "tool_output", ToolName: "read_file", Content: strings.Repeat("x", 16000), ResponseID: responseID, Status: "ok"},
		)
	}
	entries = append(entries, historyEntry{Type: "error", Content: "stopped after 40 tool rounds", Status: "error"})

	shouldCompact, reason := shouldAutoCompactConversation(entries, 80000)
	if shouldCompact {
		t.Fatalf("expected the latest per-round usage plus one tool output to stay under the threshold, got %q", reason)
	}

	// Without per-round usage, the tool output after the last reported usage must count.
	for i := range entries {
		if entries[i].Type == "tool_call" {
			entries[i].ContextTokens = 0
		}
	}
	if shouldCompact, _ := shouldAutoCompactConversation(entries, 80000); !shouldCompact {
		t.Fatal("expected tool output recorded after the last usage to trigger compaction")
	}
}

func TestShouldAutoCompactConversationCountsEveryToolOutputOfLatestResponse(t *testing.T) {
	entries := []historyEntry{
		{Type: "tool_call", ToolName: "read_file", ResponseID: "resp-1", ContextTokens: 70000},
		{Type: "tool_output", ToolName: "read_file", Content: strings.Repeat("x", 16000), ResponseID: "resp-1"},
		{Type: "tool_call", ToolName: "read_file", ResponseID: "resp-1", ContextTokens: 70000},
		{Type: "tool_output", ToolName: "read_file", Content: strings.Repeat("y", 16000), ResponseID: "resp-1"},
		{Type: "tool_call", ToolName: "read_file", ResponseID: "resp-1", ContextTokens: 70000},
		{Type: "tool_output", ToolName: "read_file", Content: strings.Repeat("z", 16000), ResponseID: "resp-1"},
	}

	if shouldCompact, _ := shouldAutoCompactConversation(entries, 80000); !shouldCompact {
		t.Fatal("expected all of the latest response's pending tool outputs to count")
	}
}

func TestMaybeAutoCompactConversationCompactsAndUpdatesResponseID(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	recordHistory(historyEntry{Type: "user", Content: "request"})
	recordHistory(historyEntry{Type: "assistant", Content: "Previous context", ResponseID: "resp-old", ContextTokens: defaultAutoCompactTokens + 1})

	var receivedParams []responses.ResponseNewParams
	create := func(ctx context.Context, params responses.ResponseNewParams) (*responses.Response, error) {
		receivedParams = append(receivedParams, params)
		switch len(receivedParams) {
		case 1:
			return &responses.Response{
				ID: "resp-summary-auto",
				Output: []responses.ResponseOutputItemUnion{
					mustResponseOutputItemUnion(t, `{"type":"message","id":"msg-sum","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Auto summary."}]}`),
				},
			}, nil
		case 2:
			return &responses.Response{
				ID: "resp-auto-seeded",
				Output: []responses.ResponseOutputItemUnion{
					mustResponseOutputItemUnion(t, `{"type":"message","id":"msg-seed","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Context loaded."}]}`),
				},
			}, nil
		default:
			t.Fatalf("unexpected create call %d", len(receivedParams))
			return nil, nil
		}
	}

	cfg := &config{
		model:           provider.Model{Provider: provider.OpenAI, ID: provider.DefaultOpenAIModel},
		reasoningEffort: provider.DefaultEffort,
		conversation:    conversationState{conversation: provider.Conversation{Provider: provider.OpenAI, ResponseID: "resp-old"}},
		providers:       map[string]provider.Provider{provider.OpenAI: provider.NewOpenAIWith(create, nil)},
	}

	compacted, err := maybeAutoCompactConversation(context.Background(), cfg)
	if err != nil {
		t.Fatalf("maybeAutoCompactConversation: %v", err)
	}
	if !compacted {
		t.Fatal("expected conversation to be auto-compacted")
	}
	if cfg.conversation.conversation.ResponseID != "resp-auto-seeded" {
		t.Fatalf("expected auto compacted response ID resp-auto-seeded, got %q", cfg.conversation.conversation.ResponseID)
	}
	if len(receivedParams) != 2 {
		t.Fatalf("expected 2 create calls, got %d", len(receivedParams))
	}

	entries, err := loadConversationHistoryEntries()
	if err != nil {
		t.Fatalf("load conversation history: %v", err)
	}
	foundAutoCompact := false
	for _, entry := range entries {
		if entry.Type == "compact" && entry.Status == "auto" {
			foundAutoCompact = true
			break
		}
	}
	if !foundAutoCompact {
		t.Fatal("expected history to record an auto compaction entry")
	}
}
