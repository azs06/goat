package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/tools"
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

// A part of any size the model asks for reaches it whole, so the offset it suggests
// continues exactly after the lines the model saw.
func TestReadOutputPartsAreNeverShortenedAgain(t *testing.T) {
	var lines []string
	for i := 1; i <= 5000; i++ {
		lines = append(lines, fmt.Sprintf("%d", i))
	}
	source := func(int) (string, string, error) { return "Bash(seq)", strings.Join(lines, "\n"), nil }
	out, err := tools.Execute(context.Background(), provider.ToolCall{Name: "read_output", Arguments: `{"output": 1, "limit": 5000}`}, tools.Options{Outputs: source})
	if err != nil {
		t.Fatal(err)
	}
	if shown := prepareToolOutputForModel("read_output", out); shown != out {
		t.Fatalf("a read_output part was shortened for the model: %d lines, %d chars", strings.Count(out, "\n")+1, len(out))
	}
	if tools.ReadOutputMaxLines+2 > maxToolOutputLinesForModel {
		t.Fatalf("ReadOutputMaxLines (%d) plus its header and footer exceeds the model's line budget (%d)", tools.ReadOutputMaxLines, maxToolOutputLinesForModel)
	}
}

func TestToolOutputsAreNumberedFromTheStartOfTheHistory(t *testing.T) {
	chdirForTest(t, t.TempDir())
	previous := activeConversationID
	t.Cleanup(func() { activeConversationID = previous })
	activeConversationID = "c-first"
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
	// A new conversation, as compaction starts, goes on from the same numbers.
	activeConversationID = "c-second"
	recordHistory(historyEntry{Type: "tool_call", ToolName: "read_file", Arguments: `{"path":"b.txt"}`, ResponseID: "r0"})
	recordHistory(historyEntry{Type: "tool_output", ToolName: "read_file", Content: "third", ResponseID: "r0", Status: "ok"})
	if n, ok := latestToolOutputNumber(); !ok || n != 3 {
		t.Fatalf("got %d, %v", n, ok)
	}
	for n, want := range map[int]string{1: "first", 2: "second", 3: "third"} {
		if _, content, err := toolOutputByNumber(n); err != nil || content != want {
			t.Errorf("output %d: got %q %v, want %q", n, content, err, want)
		}
	}
	if _, _, err := toolOutputByNumber(4); err == nil || !strings.Contains(err.Error(), "there is no output 4; the latest is 3") {
		t.Fatalf("got %v", err)
	}
}

// A shortened result carried into a conversation compacted partway through the turn
// keeps a number read_output can still find.
func TestReadOutputWorksAfterMidTurnCompaction(t *testing.T) {
	chdirForTest(t, t.TempDir())
	previous := activeConversationID
	t.Cleanup(func() { activeConversationID = previous })
	activeConversationID = "c-before"

	bashArgs, _ := json.Marshal(map[string]string{"command": "i=1; while [ $i -le 260 ]; do printf 'line-%03d\\n' \"$i\"; i=$((i+1)); done"})
	call := func(id, name, args string) responses.ResponseOutputItemUnion {
		return mustResponseOutputItemUnion(t, fmt.Sprintf(`{"type":"function_call","name":%q,"call_id":%q,"arguments":%q}`, name, id, args))
	}
	var sent []string
	var note string
	stream := func(ctx context.Context, params responses.ResponseNewParams, onText func(string)) (*responses.Response, bool, error) {
		encoded, _ := json.Marshal(params.Input)
		sent = append(sent, string(encoded))
		switch len(sent) {
		case 1:
			return &responses.Response{ID: "resp-1", Output: []responses.ResponseOutputItemUnion{call("call-1", "run_bash", string(bashArgs))}}, false, nil
		case 2:
			// The model asks for the output by the number the carried note gives.
			start := strings.Index(sent[1], "read_output with output ")
			if start < 0 {
				t.Fatalf("expected the carried result to keep its note:\n%s", sent[1])
			}
			fmt.Sscanf(sent[1][start:], "read_output with output %s", &note)
			args := fmt.Sprintf(`{"output":%s,"offset":129,"limit":3}`, note)
			return &responses.Response{ID: "resp-2", Output: []responses.ResponseOutputItemUnion{call("call-2", "read_output", args)}}, false, nil
		default:
			return &responses.Response{ID: "resp-3", Output: []responses.ResponseOutputItemUnion{
				mustResponseOutputItemUnion(t, `{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Done."}]}`),
			}}, false, nil
		}
	}
	compacted := false
	opts := turnOptions{model: provider.DefaultOpenAIModel, effort: provider.DefaultEffort,
		// Compacting starts a new conversation, as startConversation does.
		compact: func(context.Context) (provider.Conversation, bool) {
			if compacted {
				return provider.Conversation{}, false
			}
			compacted = true
			activeConversationID = "c-after"
			recordHistory(historyEntry{Type: "reset"})
			return provider.Conversation{Provider: provider.OpenAI}, true
		},
	}
	if _, err := sendPromptStream(context.Background(), provider.NewOpenAIWith(nil, stream), "run it", conversationState{}, opts); err != nil {
		t.Fatal(err)
	}
	if !compacted || len(sent) != 3 {
		t.Fatalf("expected a compaction and 3 requests, got %v and %d", compacted, len(sent))
	}
	for _, want := range []string{"(lines 129-131 of", "line-128", "line-130"} {
		if !strings.Contains(sent[2], want) {
			t.Errorf("read_output after compaction lacks %q:\n%s", want, sent[2])
		}
	}
	if strings.Contains(sent[2], "there is no output") {
		t.Errorf("the carried number no longer finds its output:\n%s", sent[2])
	}
}
