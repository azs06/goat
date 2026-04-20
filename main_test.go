package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
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

	t.Cleanup(func() {
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

func TestRunBashCommandUsesRelativeWorkdir(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	subdir := filepath.Join(workspace, "nested")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatalf("create subdir: %v", err)
	}

	output, err := runBashCommand(context.Background(), "pwd", "nested")
	if err != nil {
		t.Fatalf("run bash command: %v", err)
	}

	if !strings.Contains(output, filepath.Join(workspace, "nested")) {
		t.Fatalf("expected output to contain resolved workdir, got %q", output)
	}
}

func TestRunBashCommandRejectsEscapingWorkspace(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	_, err := runBashCommand(context.Background(), "pwd", "../outside")
	if err == nil {
		t.Fatal("expected escaping workdir to fail")
	}

	if !strings.Contains(err.Error(), "path escapes the workspace") {
		t.Fatalf("expected workspace escape error, got %v", err)
	}
}

func TestRunBashCommandIncludesFailureOutput(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	_, err := runBashCommand(context.Background(), "echo failure >&2; exit 7", ".")
	if err == nil {
		t.Fatal("expected bash command to fail")
	}

	if !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("expected exit status in error, got %v", err)
	}

	if !strings.Contains(err.Error(), "failure") {
		t.Fatalf("expected stderr in error, got %v", err)
	}
}

func TestEditFileReplacesSingleMatch(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("hello goat"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	replaced, err := editFile("sample.txt", "goat", "agent", false)
	if err != nil {
		t.Fatalf("edit file: %v", err)
	}

	if replaced != 1 {
		t.Fatalf("expected 1 replacement, got %d", replaced)
	}

	content, err := os.ReadFile(filepath.Join(workspace, "sample.txt"))
	if err != nil {
		t.Fatalf("read edited file: %v", err)
	}

	if string(content) != "hello agent" {
		t.Fatalf("unexpected content %q", string(content))
	}
}

func TestEditFileRejectsAmbiguousMatchWithoutReplaceAll(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("goat goat"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	_, err := editFile("sample.txt", "goat", "agent", false)
	if err == nil {
		t.Fatal("expected ambiguous edit to fail")
	}

	if !strings.Contains(err.Error(), "matched 2 times") {
		t.Fatalf("expected ambiguous match error, got %v", err)
	}
}

func TestEditFileReplaceAllReplacesEveryMatch(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("goat goat goat"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	replaced, err := editFile("sample.txt", "goat", "agent", true)
	if err != nil {
		t.Fatalf("replace all edit failed: %v", err)
	}

	if replaced != 3 {
		t.Fatalf("expected 3 replacements, got %d", replaced)
	}

	content, err := os.ReadFile(filepath.Join(workspace, "sample.txt"))
	if err != nil {
		t.Fatalf("read edited file: %v", err)
	}

	if string(content) != "agent agent agent" {
		t.Fatalf("unexpected content %q", string(content))
	}
}

func TestSendPromptStreamContinuesThroughToolCallRounds(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	previousStreamer := streamedResponseCreator
	defer func() {
		streamedResponseCreator = previousStreamer
	}()

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
	streamedResponseCreator = func(ctx context.Context, c *openai.Client, params responses.ResponseNewParams) (*responses.Response, bool, error) {
		receivedParams = append(receivedParams, params)
		index := len(receivedParams) - 1
		if index >= len(responsesToReturn) {
			t.Fatalf("unexpected streamed response request %d", index+1)
		}
		return responsesToReturn[index], false, nil
	}

	responseID, err := sendPromptStream(context.Background(), nil, "list files twice", "", defaultModel, defaultReasoningEffort)
	if err != nil {
		t.Fatalf("send prompt stream: %v", err)
	}

	if responseID != "resp-3" {
		t.Fatalf("expected final response ID resp-3, got %q", responseID)
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
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	previousStreamer := streamedResponseCreator
	defer func() {
		streamedResponseCreator = previousStreamer
	}()

	streamedResponseCreator = func(ctx context.Context, c *openai.Client, params responses.ResponseNewParams) (*responses.Response, bool, error) {
		return &responses.Response{
			ID: "resp-history",
			Output: []responses.ResponseOutputItemUnion{
				mustResponseOutputItemUnion(t, `{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"History saved."}]}`),
			},
		}, false, nil
	}

	responseID, err := sendPromptStream(context.Background(), nil, "hello history", "", defaultModel, defaultReasoningEffort)
	if err != nil {
		t.Fatalf("send prompt stream: %v", err)
	}
	if responseID != "resp-history" {
		t.Fatalf("expected resp-history, got %q", responseID)
	}

	historyData, err := os.ReadFile(filepath.Join(workspace, ".goat", "history.jsonl"))
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
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	previousStreamer := streamedResponseCreator
	previousSleeper := streamRetrySleeper
	defer func() {
		streamedResponseCreator = previousStreamer
		streamRetrySleeper = previousSleeper
	}()

	var callCount int
	streamedResponseCreator = func(ctx context.Context, c *openai.Client, params responses.ResponseNewParams) (*responses.Response, bool, error) {
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

	responseID, err := sendPromptStream(context.Background(), nil, "retry please", "", defaultModel, defaultReasoningEffort)
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}

	if responseID != "resp-after-retry" {
		t.Fatalf("expected resp-after-retry, got %q", responseID)
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
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	previousStreamer := streamedResponseCreator
	defer func() {
		streamedResponseCreator = previousStreamer
	}()

	var callCount int
	streamedResponseCreator = func(ctx context.Context, c *openai.Client, params responses.ResponseNewParams) (*responses.Response, bool, error) {
		callCount++
		return nil, true, &ssestream.StreamError{
			Message: "received error while streaming: rate limit",
			Event: ssestream.Event{
				Data: []byte(`{"error":{"type":"tokens","code":"rate_limit_exceeded","message":"Rate limit reached. Please try again in 1.022s.","param":null}}`),
			},
		}
	}

	responseID, err := sendPromptStream(context.Background(), nil, "partial retry", "resp-prev", defaultModel, defaultReasoningEffort)
	if err == nil {
		t.Fatal("expected interrupted partial output to return an error")
	}
	if !strings.Contains(err.Error(), "interrupted after partial text") {
		t.Fatalf("expected partial-output error, got %v", err)
	}
	if responseID != "resp-prev" {
		t.Fatalf("expected response ID to remain unchanged, got %q", responseID)
	}
	if callCount != 1 {
		t.Fatalf("expected no retry after partial output, got %d call(s)", callCount)
	}
}

func TestSendPromptStreamReturnsErrorAfterRateLimitRetriesExhausted(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	previousStreamer := streamedResponseCreator
	previousSleeper := streamRetrySleeper
	defer func() {
		streamedResponseCreator = previousStreamer
		streamRetrySleeper = previousSleeper
	}()

	var callCount int
	streamedResponseCreator = func(ctx context.Context, c *openai.Client, params responses.ResponseNewParams) (*responses.Response, bool, error) {
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

	responseID, err := sendPromptStream(context.Background(), nil, "still limited", "resp-prev", defaultModel, defaultReasoningEffort)
	if err == nil {
		t.Fatal("expected rate-limit exhaustion to return an error")
	}
	if !strings.Contains(err.Error(), "request failed after") {
		t.Fatalf("expected exhausted-retries error, got %v", err)
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("expected original rate-limit detail to be preserved, got %v", err)
	}
	if responseID != "resp-prev" {
		t.Fatalf("expected response ID to remain unchanged, got %q", responseID)
	}
	if callCount != maxStreamRateLimitRetries+1 {
		t.Fatalf("expected %d streamed calls, got %d", maxStreamRateLimitRetries+1, callCount)
	}
	if len(slept) != maxStreamRateLimitRetries {
		t.Fatalf("expected %d sleeps, got %d", maxStreamRateLimitRetries, len(slept))
	}
}

func TestSendPromptReturnsErrorInsteadOfPanicking(t *testing.T) {
	previousCreator := responseCreator
	defer func() {
		responseCreator = previousCreator
	}()

	responseCreator = func(ctx context.Context, c *openai.Client, params responses.ResponseNewParams) (*responses.Response, error) {
		return nil, fmt.Errorf("rate_limit_exceeded: request too large")
	}

	if err := sendPrompt(context.Background(), nil, defaultModel, defaultReasoningEffort, "hello"); err == nil {
		t.Fatal("expected sendPrompt to return an error")
	} else if !strings.Contains(err.Error(), "rate_limit_exceeded") {
		t.Fatalf("expected rate-limit error to be preserved, got %v", err)
	}
}

func TestSendPromptStreamTruncatesLargeRunBashToolOutputForModel(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	previousStreamer := streamedResponseCreator
	defer func() {
		streamedResponseCreator = previousStreamer
	}()

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
	streamedResponseCreator = func(ctx context.Context, c *openai.Client, params responses.ResponseNewParams) (*responses.Response, bool, error) {
		receivedParams = append(receivedParams, params)
		index := len(receivedParams) - 1
		if index >= len(responsesToReturn) {
			t.Fatalf("unexpected streamed response request %d", index+1)
		}
		return responsesToReturn[index], false, nil
	}

	responseID, err := sendPromptStream(context.Background(), nil, "run a large bash command", "", defaultModel, defaultReasoningEffort)
	if err != nil {
		t.Fatalf("sendPromptStream returned error: %v", err)
	}
	if responseID != "resp-2" {
		t.Fatalf("expected resp-2, got %q", responseID)
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

func TestSendPromptStreamReturnsLatestSuccessfulResponseIDWhenFollowupFails(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	previousStreamer := streamedResponseCreator
	defer func() {
		streamedResponseCreator = previousStreamer
	}()

	argumentsJSON, err := json.Marshal(map[string]string{"path": "."})
	if err != nil {
		t.Fatalf("marshal read_dir arguments: %v", err)
	}

	callCount := 0
	streamedResponseCreator = func(ctx context.Context, c *openai.Client, params responses.ResponseNewParams) (*responses.Response, bool, error) {
		callCount++
		if callCount == 1 {
			return &responses.Response{
				ID: "resp-tool-round",
				Output: []responses.ResponseOutputItemUnion{
					mustResponseOutputItemUnion(t, fmt.Sprintf(`{"type":"function_call","name":"read_dir","call_id":"call-1","arguments":%q}`, string(argumentsJSON))),
				},
			}, false, nil
		}
		return nil, false, fmt.Errorf("upstream failure")
	}

	responseID, err := sendPromptStream(context.Background(), nil, "list files", "resp-prev", defaultModel, defaultReasoningEffort)
	if err == nil {
		t.Fatal("expected follow-up failure to be returned")
	}
	if responseID != "resp-tool-round" {
		t.Fatalf("expected latest successful response ID, got %q", responseID)
	}
}

func TestParseReasoningEffortDefaultsToMedium(t *testing.T) {
	effort, err := parseReasoningEffort("")
	if err != nil {
		t.Fatalf("parse reasoning effort: %v", err)
	}
	if effort != shared.ReasoningEffortMedium {
		t.Fatalf("expected medium reasoning effort, got %q", effort)
	}
}

func TestCommandCompactCreatesFreshResponseChain(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	recordHistory(historyEntry{Type: "user", Content: "Please fix the rate-limit handling."})
	recordHistory(historyEntry{Type: "assistant", Content: "Working on it.", ResponseID: "resp-old"})
	recordHistory(historyEntry{Type: "tool_call", ToolName: "read_file", Arguments: "{\"path\":\"main.go\"}", ResponseID: "resp-old"})
	recordHistory(historyEntry{Type: "tool_output", ToolName: "read_file", Content: "package main", ResponseID: "resp-old", Status: "ok"})

	previousCreator := responseCreator
	defer func() {
		responseCreator = previousCreator
	}()

	var receivedParams []responses.ResponseNewParams
	responseCreator = func(ctx context.Context, c *openai.Client, params responses.ResponseNewParams) (*responses.Response, error) {
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
			t.Fatalf("unexpected responseCreator call %d", len(receivedParams))
			return nil, nil
		}
	}

	cfg := &config{
		model:           defaultModel,
		reasoningEffort: defaultReasoningEffort,
		responseID:      "resp-old",
		client:          &openai.Client{},
	}

	if err := commandCompact(cfg); err != nil {
		t.Fatalf("commandCompact: %v", err)
	}
	if cfg.responseID != "resp-seeded" {
		t.Fatalf("expected compacted response ID resp-seeded, got %q", cfg.responseID)
	}
	if len(receivedParams) != 2 {
		t.Fatalf("expected 2 responseCreator calls, got %d", len(receivedParams))
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

func TestShouldAutoCompactConversationByEntryCount(t *testing.T) {
	entries := make([]historyEntry, 0, autoCompactHistoryItems)
	for i := 0; i < autoCompactHistoryItems; i++ {
		entries = append(entries, historyEntry{Type: "user", Content: fmt.Sprintf("message %d", i)})
	}

	shouldCompact, reason := shouldAutoCompactConversation(entries)
	if !shouldCompact {
		t.Fatal("expected auto compaction to trigger by entry count")
	}
	if !strings.Contains(reason, "entries") {
		t.Fatalf("expected entry-count reason, got %q", reason)
	}
}

func TestMaybeAutoCompactConversationCompactsAndUpdatesResponseID(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	for i := 0; i < autoCompactHistoryItems; i++ {
		recordHistory(historyEntry{Type: "user", Content: fmt.Sprintf("request %d", i)})
	}
	recordHistory(historyEntry{Type: "assistant", Content: "Previous context", ResponseID: "resp-old"})

	previousCreator := responseCreator
	defer func() {
		responseCreator = previousCreator
	}()

	var receivedParams []responses.ResponseNewParams
	responseCreator = func(ctx context.Context, c *openai.Client, params responses.ResponseNewParams) (*responses.Response, error) {
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
			t.Fatalf("unexpected responseCreator call %d", len(receivedParams))
			return nil, nil
		}
	}

	cfg := &config{
		model:           defaultModel,
		reasoningEffort: defaultReasoningEffort,
		responseID:      "resp-old",
		client:          &openai.Client{},
	}

	compacted, err := maybeAutoCompactConversation(context.Background(), cfg)
	if err != nil {
		t.Fatalf("maybeAutoCompactConversation: %v", err)
	}
	if !compacted {
		t.Fatal("expected conversation to be auto-compacted")
	}
	if cfg.responseID != "resp-auto-seeded" {
		t.Fatalf("expected auto compacted response ID resp-auto-seeded, got %q", cfg.responseID)
	}
	if len(receivedParams) != 2 {
		t.Fatalf("expected 2 responseCreator calls, got %d", len(receivedParams))
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
