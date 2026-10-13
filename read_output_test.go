package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
	"github.com/openai/openai-go/v3/responses"
)

func TestModelReadsTheRestOfAShortenedOutput(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	bashArgs, _ := json.Marshal(map[string]string{"command": "i=1; while [ $i -le 260 ]; do printf 'line-%03d\\n' \"$i\"; i=$((i+1)); done"})
	call := func(id, name, args string) responses.ResponseOutputItemUnion {
		return mustResponseOutputItemUnion(t, fmt.Sprintf(`{"type":"function_call","name":%q,"call_id":%q,"arguments":%q}`, name, id, args))
	}
	replies := []*responses.Response{
		{ID: "resp-1", Output: []responses.ResponseOutputItemUnion{call("call-1", "run_bash", string(bashArgs))}},
		{ID: "resp-2", Output: []responses.ResponseOutputItemUnion{call("call-2", "read_output", `{"output":1,"offset":129,"limit":3}`)}},
		{ID: "resp-3", Output: []responses.ResponseOutputItemUnion{
			mustResponseOutputItemUnion(t, `{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Done."}]}`),
		}},
	}
	var sent []string
	stream := func(ctx context.Context, params responses.ResponseNewParams, onText func(string)) (*responses.Response, bool, error) {
		encoded, _ := json.Marshal(params.Input.OfInputItemList)
		sent = append(sent, string(encoded))
		if len(sent) > len(replies) {
			t.Fatalf("unexpected request %d", len(sent))
		}
		return replies[len(sent)-1], false, nil
	}

	if _, err := sendPromptStream(context.Background(), provider.NewOpenAIWith(nil, stream), "run it", conversationState{}, turnOptions{model: provider.DefaultOpenAIModel, effort: provider.DefaultEffort}); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(sent))
	}
	if !strings.Contains(sent[1], "omitted for token safety") || !strings.Contains(sent[1], "read_output with output 1 shows all of it") {
		t.Fatalf("expected the shortened output to say how to read the rest:\n%s", sent[1])
	}
	for _, want := range []string{"Output 1, Bash(", "(lines 129-131 of", "line-128", "line-130", "continue with offset 132"} {
		if !strings.Contains(sent[2], want) {
			t.Errorf("read_output result lacks %q:\n%s", want, sent[2])
		}
	}
	if strings.Contains(sent[2], "line-127") || strings.Contains(sent[2], "shows all of it") {
		t.Errorf("expected only the requested lines and no second note:\n%s", sent[2])
	}
}

func TestConversationToolOutputNumbersFromTheStart(t *testing.T) {
	chdirForTest(t, t.TempDir())
	if _, ok := latestToolOutputNumber(); ok {
		t.Fatal("expected no outputs in a new workspace")
	}
	for i, content := range []string{"first", "second"} {
		recordHistory(historyEntry{Type: "tool_call", ToolName: "read_file", Arguments: `{"path":"a.txt"}`, ResponseID: fmt.Sprint("r", i)})
		recordHistory(historyEntry{Type: "tool_output", ToolName: "read_file", Content: content, ResponseID: fmt.Sprint("r", i), Status: "ok"})
	}
	if n, ok := latestToolOutputNumber(); !ok || n != 2 {
		t.Fatalf("got %d, %v", n, ok)
	}
	if title, content, err := conversationToolOutput(1); err != nil || content != "first" || !strings.Contains(title, "a.txt") {
		t.Fatalf("got %q %q %v", title, content, err)
	}
	if _, _, err := conversationToolOutput(3); err == nil || !strings.Contains(err.Error(), "there is no output 3; this conversation has 2") {
		t.Fatalf("got %v", err)
	}
}
