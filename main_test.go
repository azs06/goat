package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
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
	previousStreamer := streamedResponseCreator
	defer func() {
		streamedResponseCreator = previousStreamer
	}()

	responsesToReturn := []*responses.Response{
		{
			ID: "resp-1",
			Output: []responses.ResponseOutputItemUnion{
				mustResponseOutputItemUnion(t, `{"type":"function_call","name":"get_weather","call_id":"call-1","arguments":"{\"location\":\"Paris\"}"}`),
			},
		},
		{
			ID: "resp-2",
			Output: []responses.ResponseOutputItemUnion{
				mustResponseOutputItemUnion(t, `{"type":"function_call","name":"get_weather","call_id":"call-2","arguments":"{\"location\":\"Tokyo\"}"}`),
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

	responseID := sendPromptStream(context.Background(), nil, "check the weather twice", "")

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
