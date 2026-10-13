package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/subagent"
)

// A /model switch that carries the conversation over keeps its agents reporting to it.
func TestModelSwitchCarryOverKeepsAgentsReporting(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{})
	cfg, h := agentTestConfig(t, fake, subagent.Config{}, true)
	spawnFinished(t, fake, h, "switch", "switch report")
	before := activeConversationID
	captureTerminal(t, func() {
		carryOverToModel(cfg, cfg.model, provider.Conversation{Provider: provider.Anthropic}, "summary")
	})
	if activeConversationID == before {
		t.Fatal("the switch should rotate the conversation id")
	}
	var notices string
	captureTerminal(t, func() { notices = h.turnNotices() })
	if !strings.Contains(notices, "switch report") {
		t.Fatalf("the report should reach the carried-over conversation: %q", notices)
	}
}

// /approve off reaches the subagents, even after goat was started with --yes; it also
// ends the "always" answered to any of them.
func TestApproveOffAsksAgainForSubagents(t *testing.T) {
	withColor(t, false)
	cfg := &config{approver: &bashApprover{}}
	// As --yes or GOAT_AUTO_APPROVE_BASH sets it.
	cfg.approver.autoApprove.Store(true)
	cfg.approver.sessionAutoApprove.Store(true)
	h := newAgentHost(cfg, subagent.Config{}, false)
	cfg.agents = h
	t.Cleanup(func() { h.m.StopAll(3 * time.Second) })
	a, err := h.m.Spawn(subagent.SpawnOptions{Label: "general", Mode: "general", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	approve := h.childApprover(a)
	if err := approve(context.Background(), "rm -rf build", "."); err != nil {
		t.Fatalf("with auto-approval on the command runs: %v", err)
	}

	captureTerminal(t, func() {
		if err := commandApprove(context.Background(), cfg, []string{"off"}); err != nil {
			t.Fatal(err)
		}
	})
	if err := approve(context.Background(), "rm -rf build", "."); !errors.Is(err, errBashCommandDeclined) {
		t.Fatalf("after /approve off the subagent's command must be asked about: %v", err)
	}

	// "always" answered to the agent ends with /approve off too.
	a.AutoApprove.Store(true)
	captureTerminal(t, func() { _ = commandApprove(context.Background(), cfg, []string{"off"}) })
	if a.AutoApprove.Load() {
		t.Fatal("/approve off should end the agent's always")
	}

	captureTerminal(t, func() { _ = commandApprove(context.Background(), cfg, []string{"on"}) })
	if err := approve(context.Background(), "rm -rf build", "."); err != nil {
		t.Fatalf("/approve on reaches the subagent: %v", err)
	}
}

// A wait the user interrupts gives the model nothing, so the reports it found still
// arrive as notices.
func TestAnInterruptedWaitLeavesTheReportsForTheModel(t *testing.T) {
	withColor(t, false)
	_, h := uiTestHost(t, func(ctx context.Context, a *subagent.Agent, prompt string) (string, error) {
		if prompt == "block" {
			<-ctx.Done()
			return "", context.Cause(ctx)
		}
		return "report of " + prompt, nil
	})
	previous := activeConversationID
	activeConversationID = "c-main"
	t.Cleanup(func() { activeConversationID = previous })
	a1 := spawnTest(t, h, "one", "quick")
	spawnTest(t, h, "two", "block")
	waitFor(t, "a1 to finish", func() bool { return a1.State() == subagent.Idle })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if out, err := h.Wait(ctx, nil, true, time.Minute); err == nil {
		t.Fatalf("the wait should be interrupted: %q", out)
	}
	var notices string
	captureTerminal(t, func() { notices = h.turnNotices() })
	if !strings.Contains(notices, "report of quick") {
		t.Fatalf("a1's report should still reach the model: %q", notices)
	}
	if out, _ := h.Wait(context.Background(), []string{a1.ID}, true, time.Second); !strings.Contains(out, "delivered above") {
		t.Fatalf("once delivered, a wait says so: %q", out)
	}
}

// A stopped agent whose turn has not ended, with the user's resume queued behind it,
// may still run a turn: /login, /theme and /clear stay refused.
func TestRefuseWhileAgentsRunCountsStoppedStragglers(t *testing.T) {
	withColor(t, false)
	release := make(chan struct{})
	var turns atomic.Int32
	cfg, h := uiTestHost(t, func(ctx context.Context, a *subagent.Agent, prompt string) (string, error) {
		if turns.Add(1) == 1 {
			<-release // ignores its context, like a slow tool
		}
		return "ok", nil
	})
	a := spawnTest(t, h, "slow", "go")
	waitFor(t, "the turn", func() bool { return turns.Load() == 1 })
	if err := h.m.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	if d, err := h.m.Deliver(a.ID, "resume", true); err != nil || d != subagent.Queued {
		t.Fatalf("resume: %v %v", d, err)
	}
	if err := cfg.refuseWhileAgentsRun(); err == nil {
		t.Fatal("a stopped agent still winding down, with a resume queued, should be refused")
	}
	close(release)
	waitFor(t, "the resumed turn", func() bool { return turns.Load() == 2 && h.m.Active() == 0 })
	if err := cfg.refuseWhileAgentsRun(); err != nil {
		t.Fatalf("nothing runs any more: %v", err)
	}
}

// Steered messages and agents' reports taken partway through a turn go again with the
// next request when the request that carried them fails.
func TestMidTurnTextSurvivesAFailedRequest(t *testing.T) {
	chdirForTest(t, t.TempDir())
	fake := newFakeAnthropic(t,
		newSSE("msg_1").toolUse("toolu_1", "read_dir", `{"path":"."}`).stop("tool_use", ""),
		newSSE("msg_2").text("Done.").stop("end_turn", ""),
	)
	var requests atomic.Int32
	fake.reject = func(map[string]any) (int, string, bool) {
		if requests.Add(1) == 2 {
			return 400, `{"type":"error","error":{"type":"invalid_request_error","message":"flaky"}}`, true
		}
		return 0, "", false
	}
	steering, notices := "STEERED MESSAGE", "AGENT REPORT"
	opts := turnOptions{
		model:        "claude-opus-5-5",
		effort:       provider.EffortMedium,
		takeSteering: func() string { text := steering; steering = ""; return text },
		takeNotices:  func() string { text := notices; notices = ""; return text },
	}
	p := fake.provider(true)
	state, err := sendPromptStream(context.Background(), p, "list files", conversationState{}, opts)
	if err == nil {
		t.Fatal("the second request should fail")
	}
	if !strings.Contains(state.pendingText, "STEERED MESSAGE") || !strings.Contains(state.pendingText, "AGENT REPORT") {
		t.Fatalf("the failed request's text should be kept: %q", state.pendingText)
	}
	if _, err := sendPromptStream(context.Background(), p, "go on", state, opts); err != nil {
		t.Fatal(err)
	}
	third := fake.request(2).raw
	if !strings.Contains(third, "STEERED MESSAGE") || !strings.Contains(third, "AGENT REPORT") || !strings.Contains(third, "go on") {
		t.Fatalf("the next request should carry the text again: %.2000s", third)
	}
}

// A report that went with a prompt between turns is in main's history, so a summary
// of the conversation keeps it.
func TestBetweenTurnReportsAreInTheHistory(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[after]]": {newSSE("n1").text("thanks").stop("end_turn", "")},
	})
	cfg, h := agentTestConfig(t, fake, subagent.Config{}, true)
	activeConversationID = "c-history-test"
	spawnFinished(t, fake, h, "hist", "histreport-7c1f")
	captureTerminal(t, func() {
		if err := runPrompt(context.Background(), cfg, "[[after]] what did it find?"); err != nil {
			t.Fatal(err)
		}
	})
	entries, err := loadConversationHistoryEntries()
	if err != nil {
		t.Fatal(err)
	}
	if source := buildCompactionSource(entries); !strings.Contains(source, "histreport-7c1f") {
		t.Fatalf("the report should be in the conversation's history:\n%s", source)
	}
}

// An agent event while a paste is coming in waits for the paste to end: the rest of it
// would otherwise be typed into the next prompt and its newline would send the line.
func TestIdlePromptDoesNotGiveWayInTheMiddleOfAPaste(t *testing.T) {
	withColor(t, true)
	noCbreak(t)
	input, keys := keyPipe(t)
	changed := make(chan struct{}, 1)
	var checks, fire, footers atomic.Int32
	events := &promptEvents{
		changed: changed,
		interrupt: func(empty bool) bool {
			checks.Add(1)
			return fire.Load() == 1
		},
		footer: func() string { footers.Add(1); return "FOOTER" },
	}
	result := editInBackground(t, context.Background(), input, events)
	waitFor(t, "the prompt", func() bool { return checks.Load() == 1 })
	io.WriteString(keys, pasteStart+"rm -rf build")
	// Wake the prompt until a wake finds it inside the paste: interrupt is only asked
	// outside one, so a redraw with no check means the paste has begun.
	inPaste := false
	for range 50 {
		before, redraws := checks.Load(), footers.Load()
		changed <- struct{}{}
		waitFor(t, "the redraw", func() bool { return footers.Load() > redraws })
		if checks.Load() == before {
			inPaste = true
			break
		}
	}
	if !inPaste {
		t.Fatal("the paste never began")
	}
	// Now an event arrives with the paste still coming in.
	fire.Store(1)
	redraws := footers.Load()
	changed <- struct{}{}
	select {
	case r := <-result:
		t.Fatalf("the prompt gave way mid-paste: %v, partial %q", r.err, input.partial)
	case <-time.After(5 * time.Second):
		t.Fatal("the wake was not handled")
	case <-waitChan(func() bool { return footers.Load() > redraws }):
	}
	fire.Store(0)
	io.WriteString(keys, "\rsecond"+pasteEnd+"\r")
	if r := <-result; r.err != nil || r.text != "rm -rf build\nsecond" {
		t.Fatalf("got %v %q", r.err, r.text)
	}
}

// waitChan is closed once cond holds.
func waitChan(cond func() bool) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for !cond() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if cond() {
			close(done)
		}
	}()
	return done
}

// Lines queued for main before the attach view opened stay main's.
func TestAttachLeavesLinesQueuedForMain(t *testing.T) {
	withColor(t, true)
	resetTurnQueue(t)
	saved := currentKeyCapture()
	setActiveKeyCapture(&keyCapture{})
	t.Cleanup(func() { setActiveKeyCapture(saved) })
	runner := &echoRunner{}
	cfg, h := uiTestHost(t, runner.run)
	a := spawnTest(t, h, "helper", "first")
	waitFor(t, "a1 idle", func() bool { return a.State() == subagent.Idle })

	enqueue("now fix the test")
	done := make(chan error, 1)
	captureTerminal(t, func() {
		go func() { done <- h.attach(context.Background(), cfg, a) }()
		waitFor(t, "the attach view", func() bool {
			turnQueue.mu.Lock()
			defer turnQueue.mu.Unlock()
			return len(turnQueue.items) == 0
		})
		time.Sleep(20 * time.Millisecond)
		enqueue("/back")
		nudgeLineQueued()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	if seen := runner.seen(); len(seen) != 1 {
		t.Fatalf("a line meant for main went to the agent: %q", seen)
	}
	if text, ok := nextQueuedInput(); !ok || text != "now fix the test" {
		t.Fatalf("the line should still go to main: %q %v", text, ok)
	}
}
