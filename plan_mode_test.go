package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
)

func TestPlanModeRefusesChanges(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)
	os.WriteFile("a.txt", []byte("hello\n"), 0o644)
	planMode = true
	t.Cleanup(func() { planMode = false })

	call := func(name, args string) (string, error) {
		return executeToolCall(context.Background(), provider.ToolCall{Name: name, Arguments: args})
	}
	if _, err := call("read_file", `{"path":"a.txt"}`); err != nil {
		t.Fatalf("read_file should work: %v", err)
	}
	if _, err := call("edit_file", `{"path":"a.txt","old_text":"hello","new_text":"bye","preview":true}`); err != nil {
		t.Fatalf("a preview should work: %v", err)
	}
	for _, blocked := range [][2]string{
		{"edit_file", `{"path":"a.txt","old_text":"hello","new_text":"bye"}`},
		{"write_file", `{"path":"b.txt","content":"x"}`},
		{"run_bash", `{"command":"ls"}`},
		{"delete_file", `{"path":"a.txt"}`},
	} {
		if _, err := call(blocked[0], blocked[1]); !errors.Is(err, errPlanMode) {
			t.Errorf("%s: expected plan mode to refuse, got %v", blocked[0], err)
		}
	}
	if data, _ := os.ReadFile("a.txt"); string(data) != "hello\n" {
		t.Fatalf("file changed in plan mode: %q", data)
	}
}

func TestPlanModeTellsTheModel(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)
	planMode = true
	t.Cleanup(func() { planMode = false })

	fake := newFakeAnthropic(t, newSSE("msg_1").text("plan").stop("end_turn", ""))
	if _, err := sendPromptStream(context.Background(), fake.provider(true), "refactor it", conversationState{}, "claude-opus-5-5", provider.EffortLow); err != nil {
		t.Fatal(err)
	}
	text := messagesOf(t, fake.request(0))[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(text, "[Plan mode is on") || !strings.HasSuffix(text, "refactor it") {
		t.Fatalf("got %q", text)
	}
}

func TestPlanModeNoteSurvivesProjectInstructions(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	os.WriteFile("AGENTS.md", []byte("Use tabs."), 0o644)
	planMode = true
	previous := projectInstructions
	projectInstructions = projectInstructionsText
	t.Cleanup(func() { planMode = false; projectInstructions = previous })

	fake := newFakeAnthropic(t, newSSE("msg_1").text("plan").stop("end_turn", ""))
	if _, err := sendPromptStream(context.Background(), fake.provider(true), "refactor it", conversationState{}, "claude-opus-5-5", provider.EffortLow); err != nil {
		t.Fatal(err)
	}
	text := messagesOf(t, fake.request(0))[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "Use tabs.") || !strings.Contains(text, "[Plan mode is on") || !strings.HasSuffix(text, "refactor it") {
		t.Fatalf("expected the instructions and the plan note, got %q", text)
	}
}
