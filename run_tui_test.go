//go:build unix

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/tools"
)

// TestRunTUIChecksGoatsOwnScreen is the case run_tui was made for: goat starting
// itself in a terminal, typing commands and reading what its interface shows.
func TestRunTUIChecksGoatsOwnScreen(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	config := filepath.Join(workDir, "config")
	command := "GOAT_TEST_MAIN=1 ANTHROPIC_API_KEY=test XDG_CONFIG_HOME=" + shellQuote(config) + " " + shellQuote(os.Args[0]) + " -test.run='^TestMainProcess$'"
	arguments, _ := json.Marshal(map[string]any{
		"command": command,
		"cols":    90,
		"rows":    24,
		"steps": []map[string]any{
			{"wait_for": "Welcome to G.O.A.T"},
			{"keys": "/status<Enter>", "wait_for": "Workspace:", "snapshot": true},
			{"keys": "/exit<Enter>", "wait_for": "Goodbye"},
		},
	})
	var approved string
	out, err := tools.Execute(context.Background(), provider.ToolCall{Name: tools.RunTUI, Arguments: string(arguments)}, tools.Options{
		ApproveBash: func(_ context.Context, command, _ string) error {
			approved = command
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("run_tui output:\n%s", out)
	for _, want := range []string{"(90x24, exited with status 0)", "After step 2:", "Model:     anthropic:", "Workspace:", "Goodbye"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in the screen:\n%s", want, out)
		}
	}
	if !strings.Contains(approved, "# then types: /status<Enter>\n# then types: /exit<Enter>") {
		t.Errorf("the approval should show the keys, got %q", approved)
	}
	// The goat inside kept its history in its own scratch state directory.
	if _, err := os.Stat(statePath(t, "history.jsonl")); err == nil {
		t.Error("the goat in the terminal wrote to this goat's state directory")
	}
}

func TestRunTUIIsBlockedInPlanModeAndAsksFirst(t *testing.T) {
	t.Chdir(t.TempDir())
	call := provider.ToolCall{Name: tools.RunTUI, Arguments: `{"command":"echo hi"}`}
	if _, err := tools.Execute(context.Background(), call, tools.Options{PlanMode: true}); err != tools.ErrPlanMode {
		t.Fatalf("expected plan mode to refuse run_tui, got %v", err)
	}
	declined := tools.Options{ApproveBash: func(context.Context, string, string) error { return errBashCommandDeclined }}
	if _, err := tools.Execute(context.Background(), call, declined); err != errBashCommandDeclined {
		t.Fatalf("expected the decline, got %v", err)
	}
	// A bad step is refused before the user is asked.
	asked := false
	ask := tools.Options{ApproveBash: func(context.Context, string, string) error { asked = true; return nil }}
	bad := provider.ToolCall{Name: tools.RunTUI, Arguments: `{"command":"cat","steps":[{"keys":"<Entr>"}]}`}
	if _, err := tools.Execute(context.Background(), bad, ask); err == nil || asked {
		t.Fatalf("expected the unknown key to be refused unasked, got %v (asked %v)", err, asked)
	}
	typo := provider.ToolCall{Name: tools.RunTUI, Arguments: `{"command":"cat","steps":[{"text":"hi"}]}`}
	if _, err := tools.Execute(context.Background(), typo, ask); err == nil || !strings.Contains(err.Error(), `unknown field "text"`) || asked {
		t.Fatalf("expected a misspelled step field to be refused, got %v", err)
	}
}

func TestRunTUIRendersAsTerminal(t *testing.T) {
	call := provider.ToolCall{Name: tools.RunTUI, Arguments: `{"command":"goat","workdir":"app","cols":120,"steps":[{"keys":"q"},{"wait_for":"x"}]}`}
	if name := toolDisplayName(call.Name); name != "Terminal" {
		t.Errorf("got %q", name)
	}
	if summary, flags := toolArgSummary(call.Name, call.Arguments); summary != "goat" || strings.Join(flags, ", ") != "in app, 2 steps, 120x30" {
		t.Errorf("got %q %q", summary, flags)
	}
	first, body, _ := summarizeToolOutput(call, "Terminal screen for \"goat\" in app (120x30, exited with status 0):\nhello\n(cursor at line 2, column 1)")
	if first != "Screen 120x30, exited with status 0" || len(body) != 2 || body[0] != "hello" {
		t.Errorf("got %q %q", first, body)
	}
}
