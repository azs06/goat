//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/tools"
	"github.com/azs06/goat/internal/workspace"
)

// TestSubagentsInTheTerminal drives goat in a pseudo-terminal against a fake provider:
// the model starts two background agents, the footer counts the one still running,
// Ctrl-G opens the picker, /agents a1 attaches, a message typed there gets the agent's
// answer, /back and Ctrl-C both return to main, and /exit stops the agent left running
// and exits cleanly.
func TestSubagentsInTheTerminal(t *testing.T) {
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[main]]": {
			newSSE("m1").
				toolUse("tm1", tools.SpawnAgent, `{"description":"kid helper","prompt":"[[kid]] say hi","background":true}`).
				toolUse("tm2", tools.SpawnAgent, `{"description":"slow worker","prompt":"[[slow]] take your time","background":true}`).
				stop("tool_use", ""),
			newSSE("m2").text("Spawned both.").stop("end_turn", ""),
		},
		"[[kid]]": {
			newSSE("k1").text("hi from kid").stop("end_turn", ""),
			newSSE("k2").text("kid got your message").stop("end_turn", ""),
		},
		"[[slow]]": {blockStream},
	})
	workDir := t.TempDir()
	t.Chdir(workDir)
	config := filepath.Join(workDir, "config")
	command := "GOAT_TEST_MAIN=1 ANTHROPIC_API_KEY=test ANTHROPIC_BASE_URL=" + shellQuote(fake.server.URL) + " XDG_CONFIG_HOME=" + shellQuote(config) + " " + shellQuote(os.Args[0]) + " -test.run='^TestMainProcess$'"
	out, err := workspace.RunTUI(context.Background(), workspace.TUIRun{
		Command: command,
		Cols:    100,
		Rows:    40,
		Steps: []workspace.TUIStep{
			{WaitFor: "Welcome to G.O.A.T"},
			// 2: the model starts both agents in the background.
			{Keys: "[[main]] start the helpers<Enter>", WaitFor: "Spawned both."},
			// 3: a1 finishes; its notice shows and the badge counts the one still running.
			{WaitFor: "kid helper finished", Snapshot: true},
			{WaitFor: "1 agent running (/agents)", Snapshot: true},
			// 5: Ctrl-G opens the picker; Esc closes it.
			{Keys: "<C-g>", WaitFor: "a2  slow worker", Snapshot: true},
			{Keys: "<Esc>", WaitMS: 300},
			// 7: attach to a1, which replays what it said.
			{Keys: "/agents a1<Enter>", WaitFor: "/back or Ctrl-C returns", Snapshot: true},
			// 8: a message typed there is answered by the agent.
			{Keys: "hello kid<Enter>", WaitFor: "kid got your message", Snapshot: true},
			{Keys: "/back<Enter>", WaitFor: "Back on main.", Snapshot: true},
			// 10: attach again, and leave with Ctrl-C.
			{Keys: "/agents a1<Enter>", WaitFor: "type a message for a1"},
			{Keys: "<C-c>", WaitFor: "a1 keeps going", Snapshot: true},
			{Keys: "/exit<Enter>", WaitFor: "Goodbye"},
		},
	})
	t.Logf("run_tui output:\n%s", out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"exited with status 0",
		"After step 5:", "Subagents", "main", "a1  kid helper",
		"After step 7:", "── a1 · kid helper · explore ──", "hi from kid", "ctrl-c returns to main",
		"After step 8:", "hello kid",
		"Stopping 1 subagent.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q on the screen", want)
		}
	}
	if strings.Contains(out, "still running at exit") {
		t.Error("an agent was left running at exit")
	}
	// The second turn of a1 carried the user's message.
	kid := fake.requests("[[kid]]")
	if len(kid) != 2 || !strings.Contains(kid[1].raw, "hello kid") {
		t.Fatalf("expected a1's second request to carry the message, got %d requests", len(kid))
	}
	// Main got nothing typed in the attach view.
	for _, req := range fake.requests("[[main]]") {
		if strings.Contains(req.raw, "hello kid") {
			t.Fatal("the message for a1 reached main")
		}
	}
	if n := len(fake.requests("[[main]]")); n != 2 {
		t.Fatalf("main made %d requests, want 2", n)
	}
}

// TestSubagentApprovalInTheTerminal: a background agent's command is asked about on
// main, labelled with the agent, while main sits at its prompt; the answer goes to
// that agent, which then finishes.
func TestSubagentApprovalInTheTerminal(t *testing.T) {
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[main]]": {
			newSSE("m1").toolUse("tm1", tools.SpawnAgent, `{"description":"asker","prompt":"[[asker]] run it","mode":"general","background":true}`).stop("tool_use", ""),
			newSSE("m2").text("Started.").stop("end_turn", ""),
		},
		"[[asker]]": {
			newSSE("s1").toolUse("ts1", "run_bash", `{"command":"echo approved-ok"}`).stop("tool_use", ""),
			newSSE("s2").text("ran it").stop("end_turn", ""),
		},
	})
	workDir := t.TempDir()
	t.Chdir(workDir)
	config := filepath.Join(workDir, "config")
	command := "GOAT_TEST_MAIN=1 ANTHROPIC_API_KEY=test ANTHROPIC_BASE_URL=" + shellQuote(fake.server.URL) + " XDG_CONFIG_HOME=" + shellQuote(config) + " " + shellQuote(os.Args[0]) + " -test.run='^TestMainProcess$'"
	out, err := workspace.RunTUI(context.Background(), workspace.TUIRun{
		Command: command,
		Cols:    100,
		Rows:    40,
		Steps: []workspace.TUIStep{
			{WaitFor: "Welcome to G.O.A.T"},
			{Keys: "[[main]] start it<Enter>", WaitFor: "asker wants to run:", Snapshot: true},
			{Keys: "y<Enter>", WaitFor: "asker finished", Snapshot: true},
			{Keys: "/exit<Enter>", WaitFor: "Goodbye"},
		},
	})
	t.Logf("run_tui output:\n%s", out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"exited with status 0", "◌ a1 · asker wants to run:", "Run command?", "echo approved-ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q on the screen", want)
		}
	}
	asker := fake.requests("[[asker]]")
	if len(asker) != 2 || !strings.Contains(asker[1].raw, "approved-ok") {
		t.Fatalf("expected the approved command's output in a1's second request, got %d requests", len(asker))
	}
}

// TestMidTurnAttachInTheTerminal: while main's turn follows a running foreground agent,
// Ctrl-G opens the picker at once, the user attaches to the agent, types a message it
// answers, and /back returns to main's turn, which then finishes with the agent's
// report.
func TestMidTurnAttachInTheTerminal(t *testing.T) {
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[main]]": {
			newSSE("m1").toolUse("tm1", tools.SpawnAgent, `{"description":"kid helper","prompt":"[[kid]] look around"}`).stop("tool_use", ""),
			newSSE("m2").text("Main done.").stop("end_turn", ""),
		},
		"[[kid]]": {
			newSSE("k1").toolUse("tk1", "read_dir", `{"path":"."}`).stop("tool_use", ""),
			newSSE("k2").text("kid got your message").stop("end_turn", ""),
		},
	})
	// The kid's first request waits until the user's message is in its inbox; the
	// request after its tool call carries it.
	release := make(chan struct{})
	fake.hold = map[string]chan struct{}{"[[kid]]": release}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	workDir := t.TempDir()
	t.Chdir(workDir)
	config := filepath.Join(workDir, "config")
	command := "GOAT_TEST_MAIN=1 ANTHROPIC_API_KEY=test ANTHROPIC_BASE_URL=" + shellQuote(fake.server.URL) + " XDG_CONFIG_HOME=" + shellQuote(config) + " " + shellQuote(os.Args[0]) + " -test.run='^TestMainProcess$'"
	out, err := workspace.RunTUI(context.Background(), workspace.TUIRun{
		Command: command,
		Cols:    100,
		Rows:    40,
		Steps: []workspace.TUIStep{
			{WaitFor: "Welcome to G.O.A.T"},
			// 2: main follows the kid, which waits on its first request.
			{Keys: "[[main]] ask the kid<Enter>", WaitFor: "1 agent running (/agents)"},
			// 3: Ctrl-G opens the picker while main's turn runs.
			{Keys: "<C-g>", WaitFor: "a1  kid helper", Snapshot: true},
			// 4: attach to the kid; main's turn is paused.
			{Keys: "<Down><Enter>", WaitFor: "type a message for a1", Snapshot: true},
			// 5: Ctrl-C leaves the view, not main's turn; attach again by name.
			{Keys: "<C-c>", WaitFor: "a1 keeps going", Snapshot: true},
			{Keys: "/agents a1<Enter>", WaitFor: "type a message for a1"},
			// 7: the message waits in the kid's inbox; then its request goes on.
			{Keys: "hello kid<Enter>", WaitFor: "a1 reads it at its next step", Then: func() { close(release) }},
			{WaitFor: "kid got your message", Snapshot: true},
			// 9: back to main's turn, which finishes with the kid's report.
			{Keys: "/back<Enter>", WaitFor: "Main done.", Snapshot: true},
			{Keys: "/exit<Enter>", WaitFor: "Goodbye"},
		},
	})
	t.Logf("run_tui output:\n%s", out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"exited with status 0",
		"After step 3:", "Subagents", "a1  kid helper",
		"After step 4:", "── a1 · kid helper · explore ── main's turn paused", "attached to a1 · main's turn paused",
		"After step 5:", "Back on main; a1 keeps going",
		"After step 9:", "Back on main.", "Main done.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q on the screen", want)
		}
	}
	kid := fake.requests("[[kid]]")
	if len(kid) != 2 || !strings.Contains(kid[1].raw, "hello kid") {
		t.Fatalf("expected the kid's second request to carry the message, got %d requests", len(kid))
	}
	main := fake.requests("[[main]]")
	if len(main) != 2 {
		t.Fatalf("main made %d requests, want 2", len(main))
	}
	if !strings.Contains(main[1].raw, "kid got your message") || strings.Contains(main[1].raw, "hello kid") {
		t.Fatal("main should get the kid's report and none of what was typed for the kid")
	}
	if strings.Contains(out, "Interrupted") {
		t.Error("Ctrl-C in the attach view interrupted main's turn")
	}
	if strings.Contains(out, "kid helper finished") {
		t.Error("a followed agent's turn is not announced")
	}
}
