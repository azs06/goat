package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/azs06/goat/internal/subagent"
	"github.com/azs06/goat/internal/tools"
)

// threeSpawns is a main response with three foreground spawn_agent calls, for the
// children [[c1]], [[c2]] and [[c3]].
func threeSpawns() string {
	sse := newSSE("m1").text("Asking three agents.")
	for i := 1; i <= 3; i++ {
		sse = sse.toolUse(fmt.Sprintf("tm%d", i), tools.SpawnAgent, fmt.Sprintf(`{"description":"look %d","prompt":"[[c%d]] read f%d.txt"}`, i, i, i))
	}
	return sse.stop("tool_use", "")
}

// readingChild is a child that reads its file, then reports what it says.
func readingChild(i int) []string {
	return []string{
		newSSE(fmt.Sprintf("c%d-1", i)).toolUse(fmt.Sprintf("tc%d", i), "read_file", fmt.Sprintf(`{"path":"f%d.txt"}`, i)).stop("tool_use", ""),
		newSSE(fmt.Sprintf("c%d-2", i)).text(fmt.Sprintf("report %d: f%d.txt says %d", i, i, i)).stop("end_turn", ""),
	}
}

// TestSpawnsOfOneResponseRunAtTheSameTime: three foreground spawns in one response run
// side by side. Each child's first request waits until all three have arrived, which
// they never would one after another.
func TestSpawnsOfOneResponseRunAtTheSameTime(t *testing.T) {
	withToolTestColor(t, false)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[main]]": {threeSpawns(), newSSE("m2").text("All three reported.").stop("end_turn", "")},
		"[[c1]]":   readingChild(1),
		"[[c2]]":   readingChild(2),
		"[[c3]]":   readingChild(3),
	})
	fake.barrier = newRequestBarrier("[[c1]]", "[[c2]]", "[[c3]]")
	cfg, h := agentTestConfig(t, fake, subagent.Config{}, false)
	for i := 1; i <= 3; i++ {
		writeTestFile(t, fmt.Sprintf("f%d.txt", i), fmt.Sprint(i))
	}

	out := captureTerminal(t, func() {
		if err := runPrompt(context.Background(), cfg, "[[main]] look at three files"); err != nil {
			t.Error(err)
		}
	})

	// The results come back in the calls' order, each with its own agent's report.
	reqs := fake.requests("[[main]]")
	if len(reqs) != 2 {
		t.Fatalf("expected two requests from main, got %d", len(reqs))
	}
	results := toolResultsOf(t, reqs[1])
	if len(results) != 3 {
		t.Fatalf("expected three tool results, got %v", results)
	}
	for i, result := range results {
		n := i + 1
		content := fmt.Sprint(result["content"])
		if result["tool_use_id"] != fmt.Sprintf("tm%d", n) || result["is_error"] == true {
			t.Errorf("result %d: %v", n, result)
		}
		if !strings.Contains(content, fmt.Sprintf(`<agent-report id="a%d" label="look %d">`, n, n)) || !strings.Contains(content, fmt.Sprintf("report %d: f%d.txt says %d", n, n, n)) {
			t.Errorf("result %d does not hold a%d's report: %s", n, n, content)
		}
		for other := 1; other <= 3; other++ {
			if other != n && strings.Contains(content, fmt.Sprintf("report %d:", other)) {
				t.Errorf("result %d holds report %d too", n, other)
			}
		}
	}

	// Main's history has each call and its output, in order.
	entries, err := loadConversationHistoryEntries()
	if err != nil {
		t.Fatal(err)
	}
	var calls, outputs []historyEntry
	for _, e := range entries {
		if e.ConversationID != "c-main-test" || e.ToolName != tools.SpawnAgent {
			continue
		}
		switch e.Type {
		case "tool_call":
			calls = append(calls, e)
		case "tool_output":
			outputs = append(outputs, e)
		}
	}
	if len(calls) != 3 || len(outputs) != 3 {
		t.Fatalf("expected three spawn calls and outputs in history, got %d and %d", len(calls), len(outputs))
	}
	for i := range 3 {
		n := i + 1
		if !strings.Contains(calls[i].Arguments, fmt.Sprintf("[[c%d]]", n)) || outputs[i].Status != "ok" || !strings.Contains(outputs[i].Content, fmt.Sprintf("report %d:", n)) {
			t.Errorf("history entry %d: %+v / %+v", n, calls[i], outputs[i])
		}
	}

	// The terminal showed every agent's work, each line labeled with its agent, once.
	for n := 1; n <= 3; n++ {
		read := fmt.Sprintf("f%d.txt", n)
		var labeled int
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, fmt.Sprintf("%sa%d ", nestedIndent, n)) && strings.Contains(line, read) {
				labeled++
			}
		}
		// The call's header and its result line.
		if labeled != 2 {
			t.Errorf("expected a%d's read of %s shown labeled, header and result once each; got %d lines:\n%s", n, read, labeled, out)
		}
		if strings.Count(out, fmt.Sprintf("%sa%d report %d:", nestedIndent, n, n)) != 1 {
			t.Errorf("expected a%d's report text shown once:\n%s", n, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("plain output has escape codes: %q", out)
	}
	for _, info := range h.m.List() {
		if info.State != subagent.Idle || info.Background {
			t.Errorf("%s ended %s (background %v)", info.ID, info.State, info.Background)
		}
	}
	if h.m.NoticeCount() != 0 {
		t.Errorf("foreground agents left %d notices", h.m.NoticeCount())
	}
}

// TestSpawnGroupLargerThanTheLimitWaitsForSlots: with one slot, the agents of three
// spawns run one at a time, without the limit error.
func TestSpawnGroupLargerThanTheLimitWaitsForSlots(t *testing.T) {
	withToolTestColor(t, false)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[main]]": {threeSpawns(), newSSE("m2").text("Done.").stop("end_turn", "")},
		"[[c1]]":   readingChild(1),
		"[[c2]]":   readingChild(2),
		"[[c3]]":   readingChild(3),
	})
	cfg, h := agentTestConfig(t, fake, subagent.Config{MaxRunning: 1}, false)
	for i := 1; i <= 3; i++ {
		writeTestFile(t, fmt.Sprintf("f%d.txt", i), fmt.Sprint(i))
	}
	captureTerminal(t, func() {
		if err := runPrompt(context.Background(), cfg, "[[main]] look at three files"); err != nil {
			t.Error(err)
		}
	})
	results := toolResultsOf(t, fake.requests("[[main]]")[1])
	if len(results) != 3 {
		t.Fatalf("expected three tool results, got %v", results)
	}
	for i, result := range results {
		content := fmt.Sprint(result["content"])
		if result["is_error"] == true || strings.Contains(content, "limit") || !strings.Contains(content, fmt.Sprintf("report %d:", i+1)) {
			t.Errorf("result %d: %s", i+1, content)
		}
	}
	if len(h.m.List()) != 3 {
		t.Errorf("expected three agents, got %v", h.m.List())
	}
}

// TestSpawnGroupGetsTheLimitErrorWhenOthersHoldTheSlots: when other agents hold every slot, the
// group's calls get the limit error, as a single spawn does, instead of waiting.
func TestSpawnGroupGetsTheLimitErrorWhenOthersHoldTheSlots(t *testing.T) {
	withToolTestColor(t, false)
	fake := newRoutedAnthropic(t, map[string][]string{"[[busy]]": {blockStream}})
	_, h := agentTestConfig(t, fake, subagent.Config{MaxRunning: 1}, false)
	busy, err := h.start(tools.SpawnRequest{Prompt: "[[busy]] wait", Mode: tools.AgentModeExplore}, "busy", h.settings, activeConversationID, false)
	if err != nil {
		t.Fatal(err)
	}
	done := h.PrestartSpawns(context.Background(), []tools.SpawnRequest{
		{Prompt: "one", Mode: tools.AgentModeExplore, CallID: "x1"},
		{Prompt: "two", Mode: tools.AgentModeExplore, CallID: "x2"},
	})
	defer done()
	for _, id := range []string{"x1", "x2"} {
		_, err := h.Spawn(context.Background(), tools.SpawnRequest{Prompt: "ignored", Mode: tools.AgentModeExplore, CallID: id})
		if err == nil || !strings.Contains(err.Error(), "Subagent limit reached (1 running). Do not retry") {
			t.Errorf("%s: expected the limit error, got %v", id, err)
		}
	}
	busy.InterruptTurn(subagent.ErrParentInterrupted)
}

// TestInterruptStopsTheWholeSpawnGroup: Ctrl-C while main follows the first agent of a
// group interrupts every agent started, leaves them idle, never starts the one waiting
// for a slot, and leaves no goroutine behind.
func TestInterruptStopsTheWholeSpawnGroup(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	baseline := runtime.NumGoroutine()
	withToolTestColor(t, false)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[main]]": {threeSpawns()},
		"[[c1]]":   {blockStream},
		"[[c2]]":   {blockStream},
		"[[c3]]":   {blockStream},
	})
	cfg, h := agentTestConfig(t, fake, subagent.Config{MaxRunning: 2}, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for fake.started.Load() < 2 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	var err error
	out := captureTerminal(t, func() { err = runPrompt(ctx, cfg, "[[main]] look at three files") })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the turn interrupted, got %v", err)
	}
	if strings.Contains(out, "winding down") {
		t.Errorf("an agent did not end on the interrupt:\n%s", out)
	}
	infos := h.m.List()
	if len(infos) != 2 {
		t.Fatalf("expected the two agents that fit to have started, got %v", infos)
	}
	for _, info := range infos {
		a, _ := h.m.Get(info.ID)
		if a.State() != subagent.Idle || !errors.Is(a.Err(), subagent.ErrParentInterrupted) {
			t.Errorf("%s: expected idle after the parent's interrupt, got %s %v", info.ID, a.State(), a.Err())
		}
	}
	if h.m.NoticeCount() != 0 {
		t.Errorf("interrupted foreground agents left %d notices", h.m.NoticeCount())
	}
	// The calls after the first say they were interrupted.
	entries, err := loadConversationHistoryEntries()
	if err != nil {
		t.Fatal(err)
	}
	var skipped int
	for _, e := range entries {
		if e.Type == "tool_output" && e.ToolName == tools.SpawnAgent && e.Status == "skipped" && strings.Contains(e.Content, "interrupted") {
			skipped++
		}
	}
	if skipped != 2 {
		t.Errorf("expected the two later calls skipped as interrupted, got %d", skipped)
	}
	h.mu.Lock()
	left := len(h.groups)
	h.mu.Unlock()
	if left != 0 {
		t.Errorf("%d group members left behind", left)
	}

	fake.server.CloseClientConnections()
	fake.server.Close()
	http.DefaultClient.CloseIdleConnections()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Errorf("%d goroutines after the interrupt, %d before", got, baseline)
	}
	goleak.VerifyNone(t, ignore)
}

// TestSingleSpawnIsNotGrouped: one spawn_agent call in a response runs as before.
func TestSingleSpawnIsNotGrouped(t *testing.T) {
	withToolTestColor(t, false)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[main]]": {
			newSSE("m1").toolUse("tm1", tools.SpawnAgent, `{"description":"only","prompt":"[[c1]] read f1.txt"}`).stop("tool_use", ""),
			newSSE("m2").text("Done.").stop("end_turn", ""),
		},
		"[[c1]]": readingChild(1),
	})
	cfg, h := agentTestConfig(t, fake, subagent.Config{}, false)
	writeTestFile(t, "f1.txt", "1")
	out := captureTerminal(t, func() {
		if err := runPrompt(context.Background(), cfg, "[[main]] look"); err != nil {
			t.Error(err)
		}
	})
	if strings.Contains(out, nestedIndent+"a1 ") {
		t.Errorf("a single agent's lines are labeled:\n%s", out)
	}
	if content := fmt.Sprint(toolResultsOf(t, fake.requests("[[main]]")[1])[0]["content"]); !strings.Contains(content, "report 1:") {
		t.Errorf("unexpected result %s", content)
	}
	if h.groups != nil {
		t.Error("a single spawn made a group")
	}
}

func writeTestFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestQueuedGroupMemberNeverGetsAnEmptyReport: when a group member ends, the slot it
// frees may go to another agent's waiting message instead of the next member. That
// member then either runs or gets the limit error, never an empty report.
func TestQueuedGroupMemberNeverGetsAnEmptyReport(t *testing.T) {
	withToolTestColor(t, false)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[a0]]": {newSSE("o1").text("a0 first").stop("end_turn", ""), newSSE("o2").text("a0 second").stop("end_turn", "")},
		"[[x1]]": {newSSE("x1").text("x1 report").stop("end_turn", "")},
		"[[x2]]": {newSSE("x2").text("x2 report").stop("end_turn", "")},
	})
	release := make(chan struct{})
	fake.hold = map[string]chan struct{}{"[[x1]]": release}
	_, h := agentTestConfig(t, fake, subagent.Config{MaxRunning: 1}, false)
	ctx := context.Background()
	captureTerminal(t, func() {
		if _, err := h.Spawn(ctx, tools.SpawnRequest{Description: "earlier", Prompt: "[[a0]] go", Mode: tools.AgentModeExplore}); err != nil {
			t.Fatal(err)
		}
		done := h.PrestartSpawns(ctx, []tools.SpawnRequest{
			{Description: "x1", Prompt: "[[x1]] go", Mode: tools.AgentModeExplore, CallID: "x1"},
			{Description: "x2", Prompt: "[[x2]] go", Mode: tools.AgentModeExplore, CallID: "x2"},
		})
		defer done()
		waitFor(t, "x1's request", func() bool { return len(fake.requests("[[x1]]")) == 1 })
		// a1 waits for the slot x1 holds, beside x2.
		if d, err := h.m.Deliver("a1", "[[a0]] again", false); err != nil || d != subagent.Queued {
			t.Fatalf("expected the message queued, got %v %v", d, err)
		}
		// x2 is followed while x1 runs (the host is called directly, out of the calls'
		// order), so its follow sees x1 end and the slot go to a1 or to x2.
		go func() {
			defer close(release)
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				h.mu.Lock()
				following := h.groups["x2"] == nil
				h.mu.Unlock()
				if following {
					return
				}
			}
			t.Error("x2 was never followed")
		}()
		report, err := h.Spawn(ctx, tools.SpawnRequest{CallID: "x2"})
		if x1, err := h.Spawn(ctx, tools.SpawnRequest{CallID: "x1"}); err != nil || !strings.Contains(x1, "x1 report") {
			t.Fatalf("x1: %q %v", x1, err)
		}
		switch {
		case err == nil && strings.Contains(report, `<agent-report id="a3" label="x2">`) && strings.Contains(report, "x2 report"):
		case err != nil && strings.Contains(err.Error(), "Subagent limit reached"):
		default:
			t.Fatalf("x2: expected its report or the limit error, got %q %v", report, err)
		}
	})
	// a1's second turn runs from its message in either case.
	waitFor(t, "every turn to end", func() bool { return h.m.Active() == 0 })
}

func TestGroupSpinnerLabel(t *testing.T) {
	withToolTestColor(t, false)
	if got, want := groupSpinnerLabel(3, []string{"a1 Reading", "a2 working"}, 1), "3 agents - a1 Reading - a2 working - 1 waiting"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := groupSpinnerLabel(2, nil, 0), "2 agents"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
