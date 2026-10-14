package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/subagent"
	"github.com/azs06/goat/internal/tools"
)

// spawnFinished spawns a background explore agent whose script answers report, and
// waits for it to finish.
func spawnFinished(t *testing.T, fake *routedAnthropic, h *agentHost, key, report string) {
	t.Helper()
	fake.mu.Lock()
	fake.scripts["[["+key+"]]"] = []string{newSSE(key).text(report).stop("end_turn", "")}
	fake.mu.Unlock()
	if _, err := h.Spawn(context.Background(), tools.SpawnRequest{Description: key, Prompt: "[[" + key + "]] go", Mode: tools.AgentModeExplore, Background: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, key+" to finish", func() bool { running, _, _ := h.m.Counts(); return running == 0 && h.m.NoticeCount() > 0 })
}

func TestNoticesFollowMainThroughCompaction(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{})
	cfg, h := agentTestConfig(t, fake, subagent.Config{}, true)

	// Compaction carries the conversation on under a new id; the agent spawned before
	// it still reports to it.
	spawnFinished(t, fake, h, "before", "before report")
	before := activeConversationID
	captureTerminal(t, func() {
		applyCompactedConversation(cfg, provider.Conversation{Provider: provider.Anthropic}, "summary", "auto")
	})
	if activeConversationID == before {
		t.Fatal("compaction should rotate the conversation id")
	}
	var notices string
	out := captureTerminal(t, func() { notices = h.turnNotices() })
	if !strings.Contains(notices, "before report") || !strings.Contains(out, "a1 before finished") {
		t.Fatalf("the report should reach the compacted conversation: %q\n%s", notices, out)
	}

	// Twice over, and through a manual /compact too.
	spawnFinished(t, fake, h, "twice", "twice report")
	captureTerminal(t, func() {
		applyCompactedConversation(cfg, provider.Conversation{Provider: provider.Anthropic}, "summary", "auto")
		applyCompactedConversation(cfg, provider.Conversation{Provider: provider.Anthropic}, "summary", "manual")
	})
	captureTerminal(t, func() { notices = h.turnNotices() })
	if !strings.Contains(notices, "twice report") {
		t.Fatalf("the report should survive two compactions: %q", notices)
	}

	// /clear starts another conversation: an earlier agent's report is only shown.
	spawnFinished(t, fake, h, "cleared", "cleared report")
	captureTerminal(t, func() {
		if err := commandClear(context.Background(), cfg, nil); err != nil {
			t.Fatal(err)
		}
	})
	out = captureTerminal(t, func() { notices = h.turnNotices() })
	if notices != "" || !strings.Contains(out, "a3 cleared finished") {
		t.Fatalf("after /clear the report is only shown: %q\n%s", notices, out)
	}
}

func TestAUserTurnIsNotReportedToMain(t *testing.T) {
	withColor(t, false)
	runner := &echoRunner{}
	_, h := uiTestHost(t, runner.run)
	previous := activeConversationID
	activeConversationID = "c-main"
	t.Cleanup(func() { activeConversationID = previous })
	a := spawnTest(t, h, "helper", "task")
	waitFor(t, "a1 idle", func() bool { return a.State() == subagent.Idle && h.m.NoticeCount() == 1 })
	var notices string
	captureTerminal(t, func() { notices = h.turnNotices() })
	if !strings.Contains(notices, "reply to task") {
		t.Fatalf("the spawn's report goes to main: %q", notices)
	}

	// The user talks to the agent from the attach view, then goes back to main.
	if note, err := h.sendFromUser(a, "just for me"); err != nil || note != "" {
		t.Fatalf("got %q %v", note, err)
	}
	waitFor(t, "the user's turn", func() bool { return a.State() == subagent.Idle && h.m.NoticeCount() == 1 })
	out := captureTerminal(t, func() { notices = h.turnNotices() })
	if notices != "" {
		t.Fatalf("a turn the user started must not reach the model: %q", notices)
	}
	if !strings.Contains(out, "a1 helper finished") {
		t.Fatalf("expected a one-line notice, got %q", out)
	}

	// While the user is attached nothing is announced.
	h.m.SetAttached(a.ID)
	if _, err := h.sendFromUser(a, "again"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second turn", func() bool { return a.Report() == "reply to again" && a.State() == subagent.Idle })
	if n := h.m.NoticeCount(); n != 0 {
		t.Fatalf("the attached agent's user turn is announced: %d", n)
	}
}

func TestWaitAfterANoticeDoesNotRepeatTheReport(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{})
	_, h := agentTestConfig(t, fake, subagent.Config{}, true)
	spawnFinished(t, fake, h, "once", "the long report")
	var notices string
	captureTerminal(t, func() { notices = h.turnNotices() })
	if !strings.Contains(notices, "the long report") {
		t.Fatalf("notices %q", notices)
	}
	waited, err := h.Wait(context.Background(), []string{"a1"}, true, time.Second)
	if err != nil || strings.Contains(waited, "the long report") || !strings.Contains(waited, "delivered above") {
		t.Fatalf("a wait after the notice repeated the report: %q %v", waited, err)
	}
}

func TestVerifyWaitsForGeneralAgents(t *testing.T) {
	withColor(t, false)
	_, h := uiTestHost(t, blockingRunner(false))
	ran := 0
	verify := func(context.Context) (string, error) { ran++; return "", nil }
	pending := false
	gate := h.deferVerify(verify, &pending)

	// An explore agent changes nothing, so verify runs.
	explore := spawnTest(t, h, "look", "x")
	if _, err := gate(context.Background()); err != nil || ran != 1 || pending {
		t.Fatalf("ran %d pending %v %v", ran, pending, err)
	}
	// A general agent still running puts verify off to the next turn.
	general, err := h.m.Spawn(subagent.SpawnOptions{Label: "edit", Mode: tools.AgentModeGeneral, Background: true, Prompt: "y"})
	if err != nil {
		t.Fatal(err)
	}
	out := captureTerminal(t, func() {
		if failure, err := gate(context.Background()); err != nil || failure != "" {
			t.Errorf("got %q %v", failure, err)
		}
	})
	if ran != 1 || !pending || !strings.Contains(out, "Not verifying yet: a2") {
		t.Fatalf("verify ran while a general agent worked: ran %d pending %v %q", ran, pending, out)
	}
	// Once it is done, verify runs and nothing is pending.
	h.m.Stop(general.ID)
	h.m.Stop(explore.ID)
	if _, err := gate(context.Background()); err != nil || ran != 2 || pending {
		t.Fatalf("ran %d pending %v %v", ran, pending, err)
	}
}

func TestForegroundFollowGivesUpOnATurnThatIgnoresTheInterrupt(t *testing.T) {
	withColor(t, false)
	release := make(chan struct{})
	cfg := &config{approver: &bashApprover{}}
	h := &agentHost{cfg: cfg, pending: map[string]childSettings{}, lineage: map[string]bool{}}
	// The runner ignores its context, like a provider that never notices the cancel.
	h.m = subagent.NewManager(subagent.Config{StopTimeout: 100 * time.Millisecond}, func(ctx context.Context, a *subagent.Agent, prompt string) (string, error) {
		<-release
		return "late", nil
	})
	cfg.agents = h
	t.Cleanup(func() {
		close(release)
		h.m.StopAll(3 * time.Second)
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	done := make(chan error, 1)
	var err error
	timedOut := false
	out := captureTerminal(t, func() {
		go func() {
			_, err := h.Spawn(ctx, tools.SpawnRequest{Description: "stuck", Prompt: "hang", Mode: tools.AgentModeExplore})
			done <- err
		}()
		select {
		case err = <-done:
		case <-time.After(3 * time.Second):
			timedOut = true
		}
	})
	if timedOut {
		t.Fatal("the parent's interrupt waited on a turn that ignores it")
	}
	if !errors.Is(err, context.Canceled) || !strings.Contains(out, "a1 is still winding down") {
		t.Fatalf("got %v %q", err, out)
	}
}

// A report collected between turns but dropped by /clear never reached a model, so a
// later wait gives it instead of saying it was delivered above.
func TestClearedReportIsGivenByWait(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{})
	cfg, h := agentTestConfig(t, fake, subagent.Config{}, true)
	spawnFinished(t, fake, h, "idle", "idle report")
	captureTerminal(t, func() { collectAgentNotices(cfg) })
	if len(cfg.agentNotices) != 1 {
		t.Fatalf("the report should be held for the next prompt: %q", cfg.agentNotices)
	}
	captureTerminal(t, func() {
		if err := commandClear(context.Background(), cfg, nil); err != nil {
			t.Fatal(err)
		}
	})
	got, err := h.Wait(context.Background(), []string{"a1"}, false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "idle report") || strings.Contains(got, "delivered above") {
		t.Fatalf("wait should give the report no model saw: %q", got)
	}
}
