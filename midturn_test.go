package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/subagent"
	"github.com/azs06/goat/internal/tools"
)

func TestQueueRunsAgentsAndReadOnlyCommandsAtMainsPause(t *testing.T) {
	resetTurnQueue(t)
	withColor(t, true)

	// Outside main's turn nothing is taken for a pause.
	typeQueueKeys("/ps\r")
	if _, ok := takeMidTurnCommand(); ok {
		t.Fatal("a command was taken outside main's turn")
	}
	turnQueue.items = nil
	for len(lineQueued) > 0 {
		<-lineQueued
	}

	end := beginMidTurn()
	typeQueueKeys("steer this\r")
	<-lineQueued
	typeQueueKeys("/agents log a1\r")
	if len(lineQueued) != 1 {
		t.Fatal("a queued command should wake main")
	}
	typeQueueKeys("/model x\r")
	typeQueueKeys("help\r") // a bare word stays text for the model
	typeQueueKeys("/TASKS stop a1\r")
	queueKey("\x07")

	turnQueue.capturing = true
	turnQueue.footer = []string{"model", "effort medium"}
	lines, _, _ := turnFrameLines(100, 30)
	screen := style.StripANSI(strings.Join(lines, "\n"))
	if !strings.Contains(screen, "/agents +2 waiting for a pause") {
		t.Errorf("the footer should name the commands waiting for a pause:\n%s", screen)
	}
	if !strings.Contains(screen, "queued  /model x · runs after the turn") || strings.Contains(screen, "queued  /agents") || strings.Contains(screen, "steer this · runs") {
		t.Errorf("only the commands that wait for the turn's end should say so:\n%s", screen)
	}

	var taken []string
	for {
		line, ok := takeMidTurnCommand()
		if !ok {
			break
		}
		taken = append(taken, line)
	}
	if got := strings.Join(taken, "|"); got != "/agents|/agents log a1|/TASKS stop a1" {
		t.Fatalf("taken for the pause: %q", got)
	}
	if got := strings.Join(turnQueue.items, "|"); got != "steer this|/model x|help" {
		t.Fatalf("the rest stays queued in order: %q", got)
	}
	end()
	if text, _ := nextQueuedInput(); text != "steer this" {
		t.Fatalf("got %q", text)
	}
	if text, _ := nextQueuedInput(); text != "/model x" {
		t.Fatalf("got %q", text)
	}
}

// midTurnSetup is an interactive color session whose main turn spawns foreground
// agents for the scripts, holding the first request of each held key until its channel
// is closed.
func midTurnSetup(t *testing.T, scripts map[string][]string, held ...string) (*config, *agentHost, *routedAnthropic, map[string]chan struct{}) {
	t.Helper()
	withToolTestColor(t, true)
	resetTurnQueue(t)
	fake := newRoutedAnthropic(t, scripts)
	holds := map[string]chan struct{}{}
	for _, key := range held {
		holds[key] = make(chan struct{})
	}
	fake.hold = holds
	cfg, h := agentTestConfig(t, fake, subagent.Config{}, true)
	t.Cleanup(func() {
		for _, hold := range holds {
			select {
			case <-hold:
			default:
				close(hold)
			}
		}
	})
	return cfg, h, fake, holds
}

// runPromptAsync runs main's turn on its own goroutine.
func runPromptAsync(ctx context.Context, cfg *config, prompt string) <-chan error {
	result := make(chan error, 1)
	go func() { result <- runPrompt(ctx, cfg, prompt) }()
	return result
}

func agentByID(t *testing.T, h *agentHost, id string) *subagent.Agent {
	t.Helper()
	var a *subagent.Agent
	waitFor(t, id+" to exist", func() bool {
		var err error
		a, err = h.m.Get(id)
		return err == nil
	})
	return a
}

func TestFollowRunsMidTurnCommandsWithoutWaitingForTheAgent(t *testing.T) {
	cfg, h, fake, holds := midTurnSetup(t, map[string][]string{
		"[[main]]": {
			newSSE("m1").toolUse("tm1", tools.SpawnAgent, `{"description":"kid helper","prompt":"[[kid]] look"}`).stop("tool_use", ""),
			newSSE("m2").text("Main done.").stop("end_turn", ""),
		},
		"[[kid]]": {newSSE("k1").text("kid report").stop("end_turn", "")},
	}, "[[kid]]")
	before := midTurnRuns.Load()

	var err error
	out := captureTerminal(t, func() {
		result := runPromptAsync(context.Background(), cfg, "[[main]] ask the kid")
		waitFor(t, "the kid's request", func() bool { return len(fake.requests("[[kid]]")) == 1 })
		a := agentByID(t, h, "a1")
		typeQueueKeys("/agents log a1\r")
		typeQueueKeys("/model nope\r")
		typeQueueKeys("/ps\r")
		waitFor(t, "both commands", func() bool { return midTurnRuns.Load() == before+2 })
		if !a.State().Busy() || len(fake.requests("[[main]]")) != 1 {
			t.Errorf("the commands should run while main follows the running agent: %s, %d main requests", a.State(), len(fake.requests("[[main]]")))
		}
		turnQueue.mu.Lock()
		turnQueue.capturing = true
		turnQueue.mu.Unlock()
		lines, _ := queuedInputLines(100)
		if !strings.Contains(style.StripANSI(strings.Join(lines, "\n")), "/model nope · runs after the turn") {
			t.Errorf("the command that waits should say so: %q", lines)
		}
		turnQueue.mu.Lock()
		turnQueue.capturing = false
		turnQueue.mu.Unlock()
		close(holds["[[kid]]"])
		err = <-result
	})
	if err != nil {
		t.Fatal(err)
	}
	plain := style.StripANSI(out)
	for _, want := range []string{"a1 · kid helper · explore ── transcript", "Nothing goat started is running in the background.", "Main done."} {
		if !strings.Contains(plain, want) {
			t.Errorf("expected %q:\n%s", want, plain)
		}
	}
	if midTurnRuns.Load() != before+2 {
		t.Errorf("only the two commands for the pause ran: %d", midTurnRuns.Load()-before)
	}
	reqs := fake.requests("[[main]]")
	if len(reqs) != 2 || !strings.Contains(fmt.Sprint(toolResultsOf(t, reqs[1])), "kid report") {
		t.Fatalf("main should get the kid's report: %d requests", len(reqs))
	}
	if text, ok := nextQueuedInput(); !ok || text != "/model nope" {
		t.Fatalf("the other command runs after the turn: %q %v", text, ok)
	}
}

func TestGroupFollowRunsMidTurnCommandsWithoutWaitingForTheAgents(t *testing.T) {
	cfg, h, fake, holds := midTurnSetup(t, map[string][]string{
		"[[main]]": {
			newSSE("m1").
				toolUse("tm1", tools.SpawnAgent, `{"description":"one","prompt":"[[c1]] look"}`).
				toolUse("tm2", tools.SpawnAgent, `{"description":"two","prompt":"[[c2]] look"}`).
				stop("tool_use", ""),
			newSSE("m2").text("Main done.").stop("end_turn", ""),
		},
		"[[c1]]": {newSSE("c1").text("report one").stop("end_turn", "")},
		"[[c2]]": {newSSE("c2").text("report two").stop("end_turn", "")},
	}, "[[c1]]", "[[c2]]")
	before := midTurnRuns.Load()

	var err error
	out := captureTerminal(t, func() {
		result := runPromptAsync(context.Background(), cfg, "[[main]] ask both")
		waitFor(t, "both requests", func() bool {
			return len(fake.requests("[[c1]]")) == 1 && len(fake.requests("[[c2]]")) == 1
		})
		a1, a2 := agentByID(t, h, "a1"), agentByID(t, h, "a2")
		typeQueueKeys("/agents log a2\r")
		typeQueueKeys("/ps\r")
		waitFor(t, "both commands", func() bool { return midTurnRuns.Load() == before+2 })
		if !a1.State().Busy() || !a2.State().Busy() {
			t.Errorf("the commands should run while the group runs: %s %s", a1.State(), a2.State())
		}
		close(holds["[[c1]]"])
		close(holds["[[c2]]"])
		err = <-result
	})
	if err != nil {
		t.Fatal(err)
	}
	plain := style.StripANSI(out)
	for _, want := range []string{"a2 · two · explore ── transcript", "Nothing goat started is running in the background.", "Main done."} {
		if !strings.Contains(plain, want) {
			t.Errorf("expected %q:\n%s", want, plain)
		}
	}
	results := fmt.Sprint(toolResultsOf(t, fake.requests("[[main]]")[1]))
	if !strings.Contains(results, "report one") || !strings.Contains(results, "report two") {
		t.Fatalf("main should get both reports: %s", results)
	}
}

func TestWaitToolRunsMidTurnCommands(t *testing.T) {
	cfg, h, fake, holds := midTurnSetup(t, map[string][]string{
		"[[main]]": {
			newSSE("m1").toolUse("tm1", tools.SpawnAgent, `{"description":"bg","prompt":"[[bg]] look","background":true}`).stop("tool_use", ""),
			newSSE("m2").toolUse("tm2", tools.WaitAgents, `{}`).stop("tool_use", ""),
			newSSE("m3").text("Main done.").stop("end_turn", ""),
		},
		"[[bg]]": {newSSE("b1").text("bg report").stop("end_turn", "")},
	}, "[[bg]]")
	before := midTurnRuns.Load()
	var err error
	captureTerminal(t, func() {
		result := runPromptAsync(context.Background(), cfg, "[[main]] wait for it")
		waitFor(t, "main's wait", func() bool { return len(fake.requests("[[main]]")) == 2 && len(fake.requests("[[bg]]")) == 1 })
		a := agentByID(t, h, "a1")
		typeQueueKeys("/ps\r")
		waitFor(t, "the command", func() bool { return midTurnRuns.Load() == before+1 })
		if !a.State().Busy() {
			t.Errorf("the command should run while main waits: %s", a.State())
		}
		close(holds["[[bg]]"])
		err = <-result
	})
	if err != nil {
		t.Fatal(err)
	}
	if reqs := fake.requests("[[main]]"); len(reqs) != 3 || !strings.Contains(fmt.Sprint(toolResultsOf(t, reqs[2])), "bg report") {
		t.Fatalf("the wait should still return the report")
	}
}

// attachMidTurn starts main's turn under a key capture fed by the returned writer,
// waits until main follows a1, queues a line and a draft for main, then /agents a1,
// and waits until the attach view is open with them held.
func attachMidTurn(t *testing.T, ctx context.Context, cfg *config, h *agentHost, fake *routedAnthropic) (<-chan error, *io.PipeWriter, *subagent.Agent) {
	t.Helper()
	input, keys := keyPipe(t)
	cfg.input = input
	startKeyCapture(context.Background(), input)
	t.Cleanup(stopKeyCapture)
	result := runPromptAsync(ctx, cfg, "[[main]] ask the kid")
	waitFor(t, "the kid's request", func() bool { return len(fake.requests("[[kid]]")) == 1 })
	a := agentByID(t, h, "a1")
	turnQueue.mu.Lock()
	turnQueue.items = append(turnQueue.items, "note for main", "/agents a1")
	turnQueue.editor.setText("half")
	turnQueue.mu.Unlock()
	nudgeLineQueued()
	waitFor(t, "the attach view", func() bool { return h.m.Attached() == "a1" && queueEditorEmpty() })
	return result, keys, a
}

func kidScripts(kid ...string) map[string][]string {
	return map[string][]string{
		"[[main]]": {
			newSSE("m1").toolUse("tm1", tools.SpawnAgent, `{"description":"kid helper","prompt":"[[kid]] look"}`).stop("tool_use", ""),
			newSSE("m2").text("Main done.").stop("end_turn", ""),
		},
		"[[kid]]": kid,
	}
}

func TestMidTurnAttachMessagesTheAgentAndReturnsToTheTurn(t *testing.T) {
	cfg, h, fake, holds := midTurnSetup(t, kidScripts(
		newSSE("k1").text("kid working").stop("end_turn", ""),
		newSSE("k2").text("kid got your message").stop("end_turn", ""),
	), "[[kid]]")

	var err error
	var partial string
	out := captureTerminal(t, func() {
		result, keys, a := attachMidTurn(t, context.Background(), cfg, h, fake)
		io.WriteString(keys, "hello kid\r")
		waitFor(t, "the message in the inbox", func() bool { return a.Info().Inbox == 1 })
		close(holds["[[kid]]"])
		waitFor(t, "the kid's answer", func() bool {
			return a.State() == subagent.Idle && a.Report() == "kid got your message"
		})
		if n := len(fake.requests("[[main]]")); n != 1 {
			t.Errorf("main's turn should wait while the user is attached: %d requests", n)
		}
		io.WriteString(keys, "/back\r")
		err = <-result
		stopKeyCapture()
		partial = cfg.input.partial
	})
	if err != nil {
		t.Fatal(err)
	}
	plain := style.StripANSI(out)
	for _, want := range []string{"main's turn paused · type to message", "Back on main.", "Main done."} {
		if !strings.Contains(plain, want) {
			t.Errorf("expected %q:\n%s", want, plain)
		}
	}
	// Each of the agent's events shows once: in the attach view, not again when main's
	// follow goes on.
	// The report shows again in the spawn call's result.
	for event, times := range map[string]int{"kid working": 2, "kid got your message": 1} {
		if n := strings.Count(plain, event); n != times {
			t.Errorf("%q shown %d times:\n%s", event, n, plain)
		}
	}
	main := fake.requests("[[main]]")
	if len(main) != 2 {
		t.Fatalf("main made %d requests", len(main))
	}
	// The message went on in a turn of the user's; main gets the task's report.
	if results := fmt.Sprint(toolResultsOf(t, main[1])); !strings.Contains(results, "kid working") || strings.Contains(results, "kid got your message") {
		t.Errorf("main should get the report of the turn it followed: %s", results)
	}
	// What was queued for main went to main; the agent got only its message.
	if !strings.Contains(main[1].raw, "note for main") || strings.Contains(main[1].raw, "hello kid") {
		t.Errorf("main's queued line should steer main, without the agent's message")
	}
	kid := fake.requests("[[kid]]")
	if len(kid) != 2 || !strings.Contains(kid[1].raw, "hello kid") || strings.Contains(kid[1].raw, "note for main") {
		t.Errorf("the kid's second request should carry only its message")
	}
	if partial != "half" {
		t.Errorf("the draft should come back for the prompt: %q", partial)
	}
}

func TestCtrlCInMidTurnAttachLeavesTheTurnRunning(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	cfg, h, fake, holds := midTurnSetup(t, kidScripts(
		newSSE("k1").text("kid report").stop("end_turn", ""),
	), "[[kid]]")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var err error
	var editor string
	out := captureTerminal(t, func() {
		before := midTurnRuns.Load()
		result, keys, a := attachMidTurn(t, ctx, cfg, h, fake)
		defer keys.Close()
		// The turn's Ctrl-C, as handleInput's notifyInterrupt delivers it.
		deliverInterrupt(cancel)
		waitFor(t, "the attach view to close", func() bool { return midTurnRuns.Load() == before+1 && h.m.Attached() == "" })
		if ctx.Err() != nil {
			t.Error("Ctrl-C in the attach view cancelled main's turn")
		}
		if !a.State().Busy() {
			t.Errorf("the agent should keep running: %s", a.State())
		}
		turnQueue.mu.Lock()
		editor = turnQueue.editorText()
		turnQueue.editor.setText("")
		turnQueue.mu.Unlock()
		// Commands still run at main's pauses after the view closed.
		io.WriteString(keys, "/ps\r")
		waitFor(t, "a command after the view closed", func() bool { return midTurnRuns.Load() == before+2 })
		close(holds["[[kid]]"])
		err = <-result
		stopKeyCapture()
		stopSpinner()
	})
	if err != nil {
		t.Fatal(err)
	}
	fake.server.CloseClientConnections()
	fake.server.Close()
	http.DefaultClient.CloseIdleConnections()
	cfg.input = nil
	// The key capture, the spinner, the attach view and the agent's turn are all gone.
	goleak.VerifyNone(t, ignore)
	if interruptRedirect.Load() != nil {
		t.Error("the redirect outlived the attach view")
	}
	// As after Ctrl-C between turns, what was queued for main waits in the input.
	if editor != "note for main\nhalf" {
		t.Errorf("held input: %q", editor)
	}
	plain := style.StripANSI(out)
	if !strings.Contains(plain, "Back on main; a1 keeps going") || !strings.Contains(plain, "Main done.") {
		t.Errorf("expected the view to close and the turn to finish:\n%s", plain)
	}
	if results := fmt.Sprint(toolResultsOf(t, fake.requests("[[main]]")[1])); !strings.Contains(results, "kid report") {
		t.Errorf("main should get the report: %s", results)
	}

	// Outside the view Ctrl-C ends the turn again.
	turnCtx, turnCancel := context.WithCancel(context.Background())
	deliverInterrupt(turnCancel)
	if !errors.Is(turnCtx.Err(), context.Canceled) {
		t.Error("Ctrl-C outside the attach view should cancel the turn")
	}
}

func TestAwayEventsAreSummedUp(t *testing.T) {
	withColor(t, false)
	_, h := uiTestHost(t, blockingRunner(false))
	a := spawnTest(t, h, "busy", "go")
	ring := a.Ring()
	for i := range 12 {
		ring.Append(subagent.Event{Kind: subagent.EventText, Text: fmt.Sprintf("event %d", i)})
	}
	all := ring.Since(0)
	var cursor uint64
	// The attach view showed the first three.
	h.noteShown(a.ID, all[2].Seq)
	out := captureTerminal(t, func() { h.catchUp(a.ID, ring, &cursor, true) })
	if cursor != all[len(all)-1].Seq || !strings.Contains(out, "a1 ... 9 events while you were away (/agents log a1)") {
		t.Fatalf("cursor %d, out %q", cursor, out)
	}
	// A few are shown as ever.
	cursor = all[len(all)-4].Seq
	h.serveMidTurn(context.Background())
	if out := captureTerminal(t, func() { h.catchUp(a.ID, ring, &cursor, false) }); out != "" || cursor != all[len(all)-4].Seq {
		t.Fatalf("a few events should be left to the follow: %q", out)
	}
}

func TestMidTurnPlanAppliesToMainsNextToolCall(t *testing.T) {
	cfg, _, fake, _ := midTurnSetup(t, map[string][]string{
		"[[main]]": {
			newSSE("m1").toolUse("tm1", "read_dir", `{"path":"."}`).stop("tool_use", ""),
			newSSE("m2").toolUse("tm2", "write_file", `{"path":"made.txt","content":"x"}`).stop("tool_use", ""),
			newSSE("m3").text("Main done.").stop("end_turn", ""),
		},
	})
	before := midTurnRuns.Load()
	// Typed before the turn's first pause, after its first tool call.
	typeQueueKeys("/plan\r")
	var err error
	captureTerminal(t, func() { err = runPrompt(context.Background(), cfg, "[[main]] make a file") })
	if err != nil {
		t.Fatal(err)
	}
	if midTurnRuns.Load() != before+1 || !cfg.planMode {
		t.Fatalf("/plan should run at the pause: %d runs, plan mode %v", midTurnRuns.Load()-before, cfg.planMode)
	}
	reqs := fake.requests("[[main]]")
	if len(reqs) != 3 || !strings.Contains(fmt.Sprint(toolResultsOf(t, reqs[2])), tools.ErrPlanMode.Error()) {
		t.Fatalf("the next tool call should be refused in plan mode")
	}
	if _, statErr := os.Stat("made.txt"); statErr == nil {
		t.Fatal("the file was written in plan mode")
	}
	if len(turnQueue.items) != 0 {
		t.Fatalf("nothing should be left queued: %q", turnQueue.items)
	}
}

// The end-of-turn verify follows plan mode as /plan leaves it partway through the turn.
func TestMidTurnPlanOffVerifiesTheTurnsChanges(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "verified")
	t.Setenv("GOAT_VERIFY", "touch "+filepath.ToSlash(marker))
	cfg, _, _, _ := midTurnSetup(t, map[string][]string{
		"[[main]]": {
			newSSE("m1").toolUse("tm1", "read_dir", `{"path":"."}`).stop("tool_use", ""),
			newSSE("m2").toolUse("tm2", "write_file", `{"path":"made.txt","content":"x"}`).stop("tool_use", ""),
			newSSE("m3").text("Main done.").stop("end_turn", ""),
		},
	})
	cfg.planMode = true
	typeQueueKeys("/plan off\r")
	var err error
	captureTerminal(t, func() { err = runPrompt(context.Background(), cfg, "[[main]] make a file") })
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat("made.txt"); statErr != nil {
		t.Fatal("the write after /plan off should go through")
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatal("the turn's change should be verified when it ends outside plan mode")
	}
}

func TestMidTurnPlanOnPutsOffTheVerify(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "verified")
	t.Setenv("GOAT_VERIFY", "touch "+filepath.ToSlash(marker))
	cfg, _, _, _ := midTurnSetup(t, map[string][]string{
		"[[main]]": {
			newSSE("m1").toolUse("tm1", "write_file", `{"path":"made.txt","content":"x"}`).stop("tool_use", ""),
			newSSE("m2").text("Main done.").stop("end_turn", ""),
		},
	})
	typeQueueKeys("/plan on\r")
	var err error
	captureTerminal(t, func() { err = runPrompt(context.Background(), cfg, "[[main]] make a file") })
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.planMode {
		t.Fatal("/plan on should run at the pause")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("no verify command should run in plan mode")
	}
	if !cfg.verifyPending {
		t.Fatal("the change made before /plan on should be checked by a later turn")
	}
}
