package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/workspace"
)

func TestToolCallsWithMissingRequiredArgumentsDoNotRun(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	if err := workspace.WriteFile("a.txt", "keep"); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, call := range []provider.ToolCall{
		{Name: "edit_file", Arguments: `{"path":"a.txt","new_text":"x"}`},
		{Name: "delete_file", Arguments: `{}`},
		{Name: "write_file", Arguments: `{"path":"a.txt","content":"par`},
		{Name: "write_file", Arguments: `{"path":"a.txt","content":null}`},
	} {
		if _, err := Execute(context.Background(), call, Options{}); err == nil {
			t.Errorf("%s %s: expected a validation error", call.Name, call.Arguments)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(workDir, "a.txt")); string(data) != "keep" {
		t.Fatalf("expected the file untouched, got %q", data)
	}
	if _, err := Execute(context.Background(), provider.ToolCall{Name: "undo_last_change", Arguments: ""}, Options{}); err != nil {
		t.Fatalf("expected a tool without required arguments to accept empty input, got %v", err)
	}
}

func TestPlanModeRefusesChanges(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	os.WriteFile("a.txt", []byte("hello\n"), 0o644)

	call := func(name, args string) (string, error) {
		return Execute(context.Background(), provider.ToolCall{Name: name, Arguments: args}, Options{PlanMode: true})
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
		if _, err := call(blocked[0], blocked[1]); !errors.Is(err, ErrPlanMode) {
			t.Errorf("%s: expected plan mode to refuse, got %v", blocked[0], err)
		}
	}
	if data, _ := os.ReadFile("a.txt"); string(data) != "hello\n" {
		t.Fatalf("file changed in plan mode: %q", data)
	}
}

func TestEveryToolHasAHandler(t *testing.T) {
	names := Names()
	for _, name := range names {
		if toolHandlers[name] == nil {
			t.Errorf("tool %s has a schema but no handler", name)
		}
	}
	if len(toolHandlers) != len(names) {
		t.Errorf("%d handlers for %d tools", len(toolHandlers), len(names))
	}
}
