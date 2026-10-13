package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// allToolNames names every tool, run_code included.
func allToolNames() []string {
	var names []string
	for _, spec := range CodeModeSpecs {
		names = append(names, spec.Name)
	}
	return names
}

func TestEveryToolHasAHandler(t *testing.T) {
	names := allToolNames()
	for _, name := range names {
		if toolHandlers[name] == nil {
			t.Errorf("tool %s has a schema but no handler", name)
		}
	}
	if len(toolHandlers) != len(names) {
		t.Errorf("%d handlers for %d tools", len(toolHandlers), len(names))
	}
}

func TestPlanModePolicyMatchesTheRefusal(t *testing.T) {
	want := map[string]PlanModePolicy{
		"read_dir": PlanModeAllowed, "tree_dir": PlanModeAllowed, "read_file": PlanModeAllowed,
		"find_files": PlanModeAllowed, "grep_files": PlanModeAllowed, "read_output": PlanModeAllowed, "view_image": PlanModeAllowed,
		"write_file": PlanModePreview, "edit_file": PlanModePreview, "edit_json_file": PlanModePreview,
		"copy_file": PlanModePreview, "move_file": PlanModePreview, "delete_file": PlanModePreview,
		"copy_dir": PlanModePreview, "move_dir": PlanModePreview, "delete_dir": PlanModePreview,
		"undo_last_change": PlanModePreview,
		"run_bash":         PlanModeBlocked,
		"run_code":         PlanModePerCall,
	}
	if workspace.TUIAvailable {
		want[RunTUI] = PlanModeBlocked
	}
	if len(want) != len(allToolNames()) {
		t.Fatalf("the policy table covers %d of %d tools", len(want), len(allToolNames()))
	}
	for _, name := range allToolNames() {
		if got := PlanModePolicyFor(name); got != want[name] {
			t.Errorf("PlanModePolicyFor(%s) = %v, want %v", name, got, want[name])
		}
	}

	// The policy and the refusal itself agree, call by call.
	for _, name := range Names() {
		call := func(arguments string) bool {
			return allowedInPlanMode(provider.ToolCall{Name: name, Arguments: arguments})
		}
		switch PlanModePolicyFor(name) {
		case PlanModeAllowed:
			if !call("{}") || !call(`{"preview":true}`) {
				t.Errorf("%s: a read-only tool should always be allowed", name)
			}
		case PlanModePreview:
			if call("{}") || call(`{"preview":false}`) || !call(`{"preview":true}`) {
				t.Errorf("%s: a preview tool should need preview=true", name)
			}
		case PlanModeBlocked:
			if call("{}") || call(`{"preview":true}`) {
				t.Errorf("%s: a blocked tool should never be allowed", name)
			}
		case PlanModePerCall:
			if !call("{}") {
				t.Errorf("%s: a per-call tool should be allowed; its calls are checked", name)
			}
		default:
			t.Errorf("%s: policy %v is not covered", name, PlanModePolicyFor(name))
		}
	}
}

func TestRequiredArguments(t *testing.T) {
	grep, ok := findSpec("grep_files")
	if !ok {
		t.Fatal("grep_files has no spec")
	}
	if got := strings.Join(RequiredArguments(grep), ", "); got != "path, query" {
		t.Errorf("grep_files requires %q, want %q", got, "path, query")
	}
	undo, ok := findSpec("undo_last_change")
	if !ok {
		t.Fatal("undo_last_change has no spec")
	}
	if got := RequiredArguments(undo); len(got) != 0 {
		t.Errorf("undo_last_change requires %v, want none", got)
	}
}
