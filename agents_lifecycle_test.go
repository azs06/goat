package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/subagent"
	"github.com/azs06/goat/internal/tools"
	"github.com/azs06/goat/internal/workspace"
)

// ringNotices are the notice events in a's ring.
func ringNotices(a *subagent.Agent) []string {
	var notices []string
	for _, e := range a.Ring().Since(0) {
		if e.Kind == subagent.EventNotice {
			notices = append(notices, e.Text)
		}
	}
	return notices
}

// A subagent's turn that cannot save its history warns in the agent's events, never on
// the terminal, which only main writes to.
func TestSubagentHistoryWarningsStayOffTheTerminal(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[warn]]": {newSSE("w1").text("warned").stop("end_turn", "")},
	})
	_, h := agentTestConfig(t, fake, subagent.Config{}, true)
	previous := historyEntryAppender
	historyEntryAppender = func(historyEntry) error { return errors.New("disk full") }
	t.Cleanup(func() { historyEntryAppender = previous })

	var a *subagent.Agent
	out := captureTerminal(t, func() {
		if _, err := h.Spawn(context.Background(), tools.SpawnRequest{Description: "warn", Prompt: "[[warn]] go", Mode: tools.AgentModeExplore, Background: true}); err != nil {
			t.Fatal(err)
		}
		var err error
		if a, err = h.m.Get("a1"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "a1 to finish", func() bool { return a.State() == subagent.Idle && h.m.Active() == 0 })
	})
	if strings.Contains(out, "failed to save history") {
		t.Fatalf("a subagent's warning reached the terminal: %q", out)
	}
	if notices := ringNotices(a); len(notices) == 0 || !strings.Contains(notices[0], "failed to save history: disk full") {
		t.Fatalf("expected the warning in a1's events, got %q", notices)
	}
}

// A subagent's read_output that finds unreadable history lines warns in its events.
func TestSubagentReadOutputWarningStaysOffTheTerminal(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[skip]]": {
			newSSE("s1").toolUse("t1", "read_output", `{"output":1}`).stop("tool_use", ""),
			newSSE("s2").text("read it").stop("end_turn", ""),
		},
	})
	_, h := agentTestConfig(t, fake, subagent.Config{}, true)
	path, _, err := workspace.ResolveStatePath(conversationHistoryPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var a *subagent.Agent
	out := captureTerminal(t, func() {
		if _, err := h.Spawn(context.Background(), tools.SpawnRequest{Description: "skip", Prompt: "[[skip]] go", Mode: tools.AgentModeExplore, Background: true}); err != nil {
			t.Fatal(err)
		}
		if a, err = h.m.Get("a1"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "a1 to finish", func() bool { return a.State() == subagent.Idle && h.m.Active() == 0 })
	})
	if strings.Contains(out, "unreadable line") {
		t.Fatalf("a subagent's warning reached the terminal: %q", out)
	}
	if notices := strings.Join(ringNotices(a), "\n"); !strings.Contains(notices, "skipped 1 unreadable line(s)") {
		t.Fatalf("expected the warning in a1's events, got %q", notices)
	}
}

// A first turn that fails before it saves the conversation keeps the agent's model, so
// a later message runs.
func TestSubagentFirstTurnFailureKeepsItsModel(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[kid]]": {newSSE("k1").text("kid answered").stop("end_turn", "")},
	})
	cfg, h := agentTestConfig(t, fake, subagent.Config{}, true)
	// A provider that cannot be set up yet.
	h.mu.Lock()
	h.settings.model = provider.Model{Provider: "not-set-up", ID: "claude-opus-5-5"}
	h.mu.Unlock()
	if _, err := h.Spawn(context.Background(), tools.SpawnRequest{Description: "kid", Prompt: "[[kid]] go", Mode: tools.AgentModeExplore, Background: true}); err != nil {
		t.Fatal(err)
	}
	a, err := h.m.Get("a1")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first turn to fail", func() bool { return a.State() == subagent.Failed && h.m.Active() == 0 })

	cfg.providersMu.Lock()
	cfg.providers["not-set-up"] = fake.provider()
	cfg.providersMu.Unlock()
	if _, err := h.m.Deliver("a1", "[[kid]] again", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second turn to end", func() bool { return a.State() != subagent.Running && h.m.Active() == 0 })
	if a.Err() != nil || a.Report() != "kid answered" {
		t.Fatalf("the second turn should run: %v, report %q", a.Err(), a.Report())
	}
}

// goat -c without a session file continues main's conversation, never a subagent's.
func TestResumeFromHistorySkipsSubagentConversations(t *testing.T) {
	chdirForTest(t, t.TempDir())
	for _, e := range []historyEntry{
		{Type: "user", Content: "hi", ConversationID: "c-main", Provider: provider.OpenAI},
		{Type: "assistant", Content: "hello", ResponseID: "resp_main", ConversationID: "c-main", Provider: provider.OpenAI},
		{Type: "user", Content: "task", ConversationID: "c-main-sub1", ParentID: "c-main", Provider: provider.OpenAI},
		{Type: "assistant", Content: "done", ResponseID: "resp_sub", ConversationID: "c-main-sub1", ParentID: "c-main", Provider: provider.OpenAI},
	} {
		recordHistory(e)
	}
	response, conversation, err := latestResumableConversation()
	if err != nil || response != "resp_main" || conversation != "c-main" {
		t.Fatalf("got %q %q %v, want main's response", response, conversation, err)
	}
}

// /agents stop all stops an idle agent whose messages wait for a slot, too; it does not
// start once the running ones are stopped.
func TestStopAllStopsAgentsWaitingForASlot(t *testing.T) {
	withColor(t, false)
	var quick atomic.Int32
	runner := func(ctx context.Context, a *subagent.Agent, prompt string) (string, error) {
		if a.Label == "quick" {
			quick.Add(1)
			return "done", nil
		}
		<-ctx.Done()
		return "", context.Cause(ctx)
	}
	cfg := &config{approver: &bashApprover{}}
	h := &agentHost{cfg: cfg, pending: map[string]childSettings{}}
	h.m = subagent.NewManager(subagent.Config{MaxRunning: 1}, runner)
	cfg.agents = h
	previous := runningAgents.Swap(h)
	t.Cleanup(func() {
		h.m.StopAll(3 * time.Second)
		runningAgents.Store(previous)
	})
	a1 := spawnTest(t, h, "quick", "p")
	waitFor(t, "a1 idle", func() bool { return a1.State() == subagent.Idle && h.m.Active() == 0 })
	spawnTest(t, h, "slow", "p")
	waitFor(t, "a2 running", func() bool { r, _, _ := h.m.Counts(); return r == 1 })
	if d, err := h.m.Deliver("a1", "again", false); err != nil || d != subagent.Queued {
		t.Fatalf("a1's message should wait for a slot: %v %v", d, err)
	}

	out := captureTerminal(t, func() {
		if err := commandAgents(context.Background(), cfg, []string{"stop", "all"}); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, "a1") {
		t.Errorf("a1 should be named as stopped: %q", out)
	}
	waitFor(t, "every turn to end", func() bool { return h.m.Active() == 0 })
	if n := quick.Load(); n != 1 || a1.State() != subagent.Stopped {
		t.Fatalf("a1 ran %d turns and is %s; its queued message should not run", n, a1.State())
	}
}

// send_message to an agent an earlier conversation spawned delivers its report to the
// current conversation, as it promises.
func TestSendToAnEarlierConversationsAgentDeliversItsReport(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{})
	_, h := agentTestConfig(t, fake, subagent.Config{}, true)
	spawnFinished(t, fake, h, "old", "first report")
	captureTerminal(t, func() { h.turnNotices() })

	// /clear starts another conversation.
	activeConversationID = "c-after-clear"
	h.leaveLineage()
	fake.mu.Lock()
	fake.scripts["[[old]]"] = []string{newSSE("o2").text("second report").stop("end_turn", "")}
	fake.mu.Unlock()
	reply, err := h.Send(context.Background(), "a1", "one more thing")
	if err != nil || !strings.Contains(reply, "delivered to you automatically") {
		t.Fatalf("got %q %v", reply, err)
	}
	waitFor(t, "a1 to finish", func() bool { return h.m.Active() == 0 && h.m.NoticeCount() > 0 })
	var notices string
	captureTerminal(t, func() { notices = h.turnNotices() })
	if !strings.Contains(notices, "second report") {
		t.Fatalf("the report promised to the current conversation was not delivered: %q", notices)
	}

	// Another switch drops the adoption: the next report is only shown.
	activeConversationID = "c-later"
	h.leaveLineage()
	if h.isAdopted("a1") {
		t.Fatal("a conversation switch should end the adoption")
	}
}

// A run_code script in a general subagent stops writing once the user turns plan mode
// on: each of its calls asks whether plan mode is on now.
func TestRunCodeCallsFollowPlanModeTurnedOnMidScript(t *testing.T) {
	chdirForTest(t, t.TempDir())
	withToolTestColor(t, false)
	code := `async () => {
		await goat.write_file({ path: "first.txt", content: "1" });
		try { await goat.write_file({ path: "second.txt", content: "2" }); } catch (e) { return e.message; }
		return "wrote both";
	}`
	args, _ := json.Marshal(map[string]string{"code": code})
	resp := &provider.Response{ID: "r1", ToolCalls: []provider.ToolCall{{ID: "c1", Name: tools.RunCode, Arguments: string(args)}}}
	// The user turns plan mode on as soon as the first file is written.
	planMode := func() bool { _, err := os.Stat("first.txt"); return err == nil }
	opts := turnOptions{codeMode: true, livePlanMode: planMode, sink: &recordingSink{}, subagent: true}
	results := runToolCalls(context.Background(), provider.Anthropic, opts, resp)
	if len(results) != 1 || !strings.Contains(results[0].Output, tools.ErrPlanMode.Error()) {
		t.Fatalf("the second write should be refused: %+v", results)
	}
	if _, err := os.Stat("second.txt"); err == nil {
		t.Fatal("the script wrote after plan mode was turned on")
	}
}
