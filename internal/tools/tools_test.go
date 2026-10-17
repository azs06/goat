package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

// allToolNames names every tool, run_code and the subagent tools included.
func allToolNames() []string {
	var names []string
	for _, spec := range CodeModeSpecs {
		names = append(names, spec.Name)
	}
	for _, spec := range AgentSpecs {
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
		"agent_status":     PlanModeAllowed, "wait": PlanModeAllowed, "skill": PlanModeAllowed,
		"spawn_agent": PlanModeDelegated, "send_message": PlanModeDelegated, "stop_agent": PlanModeDelegated,
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
	for _, name := range allToolNames() {
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
		case PlanModeDelegated:
			if !call("{}") {
				t.Errorf("%s: a delegated tool should be allowed; its subagent's calls are checked", name)
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

// fakeAgentHost records what the subagent tools ask of it.
type fakeAgentHost struct {
	spawned []SpawnRequest
	calls   []string
	ids     []string
	all     bool
	timeout time.Duration
}

func (h *fakeAgentHost) Spawn(_ context.Context, req SpawnRequest) (string, error) {
	h.spawned = append(h.spawned, req)
	return "report", nil
}

func (h *fakeAgentHost) Status(id string) (string, error) {
	h.calls = append(h.calls, "status "+id)
	return "status", nil
}

func (h *fakeAgentHost) Send(_ context.Context, id, message string) (string, error) {
	h.calls = append(h.calls, "send "+id+" "+message)
	return "sent", nil
}

func (h *fakeAgentHost) Wait(_ context.Context, ids []string, all bool, timeout time.Duration) (string, error) {
	h.calls = append(h.calls, "wait")
	h.ids, h.all, h.timeout = ids, all, timeout
	return "waited", nil
}

func (h *fakeAgentHost) Stop(id string) (string, error) {
	h.calls = append(h.calls, "stop "+id)
	return "stopped", nil
}

func TestAgentToolsCallTheHost(t *testing.T) {
	host := &fakeAgentHost{}
	// Plan mode lets every subagent tool run; the subagent's own calls are checked.
	opts := Options{Agents: host, PlanMode: true}
	run := func(name, args string) string {
		t.Helper()
		out, err := Execute(context.Background(), provider.ToolCall{Name: name, Arguments: args}, opts)
		if err != nil {
			t.Fatalf("%s %s: %v", name, args, err)
		}
		return out
	}

	if out := run(SpawnAgent, `{"description":" find tests ","prompt":" list the tests "}`); out != "report" {
		t.Errorf("spawn_agent returned %q", out)
	}
	run(SpawnAgent, `{"description":"fix it","prompt":"fix the bug","mode":"General","background":true}`)
	want := []SpawnRequest{
		{Description: "find tests", Prompt: "list the tests", Mode: AgentModeExplore},
		{Description: "fix it", Prompt: "fix the bug", Mode: AgentModeGeneral, Background: true},
	}
	if !reflect.DeepEqual(host.spawned, want) {
		t.Errorf("spawned %+v, want %+v", host.spawned, want)
	}

	run(AgentStatus, ``)
	run(AgentStatus, `{"agent":"a1"}`)
	run(SendMessage, `{"agent":"a1","message":"and the docs"}`)
	run(StopAgent, `{"agent":"a2"}`)
	if got := strings.Join(host.calls, "|"); got != "status |status a1|send a1 and the docs|stop a2" {
		t.Errorf("calls %q", got)
	}

	run(WaitAgents, `{}`)
	if host.ids != nil || host.all || host.timeout != 120*time.Second {
		t.Errorf("wait with no arguments: ids %v, all %v, timeout %v", host.ids, host.all, host.timeout)
	}
	run(WaitAgents, `{"agents":["a1"," ","a2"],"all":true,"timeout":5000}`)
	if !reflect.DeepEqual(host.ids, []string{"a1", "a2"}) || !host.all || host.timeout != 600*time.Second {
		t.Errorf("wait: ids %v, all %v, timeout %v", host.ids, host.all, host.timeout)
	}

	for _, bad := range [][2]string{
		{SpawnAgent, `{"description":"x","prompt":"y","mode":"turbo"}`},
		{SpawnAgent, `{"description":"x","prompt":"  "}`},
		{SpawnAgent, `{"prompt":"y"}`},
		{SendMessage, `{"agent":"a1","message":""}`},
		{StopAgent, `{}`},
	} {
		if _, err := Execute(context.Background(), provider.ToolCall{Name: bad[0], Arguments: bad[1]}, opts); err == nil {
			t.Errorf("%s %s: expected an error", bad[0], bad[1])
		}
	}
	if len(host.spawned) != 2 {
		t.Errorf("a refused spawn reached the host: %+v", host.spawned)
	}
}

func TestAgentToolsWithoutAHost(t *testing.T) {
	for _, call := range []provider.ToolCall{
		{Name: SpawnAgent, Arguments: `{"description":"x","prompt":"y"}`},
		{Name: AgentStatus, Arguments: `{}`},
		{Name: SendMessage, Arguments: `{"agent":"a1","message":"hi"}`},
		{Name: WaitAgents, Arguments: `{}`},
		{Name: StopAgent, Arguments: `{"agent":"a1"}`},
	} {
		if _, err := Execute(context.Background(), call, Options{Subagent: true}); !errors.Is(err, ErrNoAgents) {
			t.Errorf("%s: expected ErrNoAgents, got %v", call.Name, err)
		}
	}
}

func TestAgentSpecsAreOfferedSeparately(t *testing.T) {
	offered := map[string]bool{}
	for _, spec := range CodeModeSpecs {
		offered[spec.Name] = true
	}
	for _, spec := range AgentSpecs {
		if offered[spec.Name] {
			t.Errorf("%s is in CodeModeSpecs; subagent tools belong only in AgentSpecs", spec.Name)
		}
		if _, ok := findSpec(spec.Name); !ok {
			t.Errorf("findSpec does not find %s", spec.Name)
		}
	}
}

func TestSubagentsCannotUndo(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range []string{`{}`, `{"preview":true}`} {
		_, err := Execute(context.Background(), provider.ToolCall{Name: "undo_last_change", Arguments: args}, Options{Subagent: true})
		if err == nil || !strings.Contains(err.Error(), "not available to subagents; undo is workspace-wide") {
			t.Errorf("undo_last_change %s in a subagent: got %v", args, err)
		}
	}
	if _, err := Execute(context.Background(), provider.ToolCall{Name: "undo_last_change", Arguments: `{}`}, Options{}); err != nil {
		t.Errorf("undo_last_change at the top level: %v", err)
	}
}
