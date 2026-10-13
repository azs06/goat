package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/subagent"
)

// uiTestHost is a host whose agents run run instead of real turns, with cfg's agents
// set to it and the footer reading it.
func uiTestHost(t *testing.T, run subagent.Runner) (*config, *agentHost) {
	t.Helper()
	cfg := &config{approver: &bashApprover{}}
	h := &agentHost{cfg: cfg, pending: map[string]childSettings{}}
	h.m = subagent.NewManager(subagent.Config{}, run)
	cfg.agents = h
	previous := runningAgents.Swap(h)
	t.Cleanup(func() {
		h.m.StopAll(3 * time.Second)
		runningAgents.Store(previous)
	})
	return cfg, h
}

// blockingRunner holds each turn until it is cancelled; asking makes it wait for
// approval first.
func blockingRunner(asking bool) subagent.Runner {
	return countingBlockingRunner(asking, nil)
}

// countingBlockingRunner is blockingRunner counting the turns that returned.
func countingBlockingRunner(asking bool, returned *atomic.Int32) subagent.Runner {
	return func(ctx context.Context, a *subagent.Agent, prompt string) (string, error) {
		if returned != nil {
			defer returned.Add(1)
		}
		if asking {
			a.SetState(subagent.WaitingApproval)
		}
		<-ctx.Done()
		return "", context.Cause(ctx)
	}
}

func spawnTest(t *testing.T, h *agentHost, label, prompt string) *subagent.Agent {
	t.Helper()
	a, err := h.m.Spawn(subagent.SpawnOptions{Label: label, Mode: "explore", ParentConv: "c-main", ConvID: "c-" + label, Background: true, Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAgentsFooterPartCountsWithoutGit(t *testing.T) {
	withColor(t, true)
	runningAgents.Store(nil)
	if part := agentsFooterPart(); part != "" {
		t.Fatalf("without a host: %q", part)
	}
	_, h := uiTestHost(t, blockingRunner(false))
	if part := agentsFooterPart(); part != "" {
		t.Fatalf("with no agents: %q", part)
	}
	spawnTest(t, h, "one", "p")
	waitFor(t, "a1 running", func() bool { r, _, _ := h.m.Counts(); return r == 1 })
	if part := agentsFooterPart(); part != "1 agent running (/agents)" {
		t.Fatalf("got %q", part)
	}

	_, h2 := uiTestHost(t, blockingRunner(true))
	spawnTest(t, h2, "two", "p")
	spawnTest(t, h2, "three", "p")
	waitFor(t, "both waiting", func() bool { _, w, _ := h2.m.Counts(); return w == 2 })
	if part := agentsFooterPart(); part != "2 agents running · 2 needs approval (/agents)" {
		t.Fatalf("got %q", part)
	}

	// After the model and the modes, replacing one already there.
	parts := withAgentsPart([]string{"model", "effort high", "plan mode", "code mode", "1 command running (/ps)", "old badge (/agents)", "main ↑"}, "NEW (/agents)")
	if strings.Join(parts, "|") != "model|effort high|plan mode|code mode|NEW (/agents)|1 command running (/ps)|main ↑" {
		t.Fatalf("got %q", parts)
	}
	if parts := withAgentsPart([]string{"model", "x (/agents)"}, ""); strings.Join(parts, "|") != "model" {
		t.Fatalf("got %q", parts)
	}
}

func TestTurnFrameShowsTheLiveAgentsBadge(t *testing.T) {
	resetTurnQueue(t)
	withColor(t, true)
	_, h := uiTestHost(t, blockingRunner(false))
	turnQueue.capturing = true
	setTurnFooter([]string{"anthropic:claude-opus-5-5", "effort high"})
	frame := func() string {
		lines, _, _ := turnFrameLines(120, 20)
		return style.StripANSI(strings.Join(lines, "\n"))
	}
	if strings.Contains(frame(), "/agents") {
		t.Fatalf("no badge without agents:\n%s", frame())
	}
	// The snapshot the turn took stays; the badge comes from the manager at each redraw.
	spawnTest(t, h, "one", "p")
	waitFor(t, "a1 running", func() bool { r, _, _ := h.m.Counts(); return r == 1 })
	if !strings.Contains(frame(), "effort high · 1 agent running (/agents)") {
		t.Fatalf("expected the badge in the frame:\n%s", frame())
	}
}

func TestAgentPickerRows(t *testing.T) {
	withColor(t, true)
	now := time.Now()
	long := strings.Repeat("r", 100)
	infos := []subagent.Info{
		{ID: "a1", Label: "done one", Mode: "explore", State: subagent.Idle, ReportHead: long, Started: now.Add(-90 * time.Second), Updated: now.Add(-30 * time.Second)},
		{ID: "a2", Label: "fix flaky test", Mode: "general", State: subagent.Running, Activity: "Read", Started: now.Add(-12 * time.Second)},
		{ID: "a3", Label: "asks", Mode: "general", State: subagent.WaitingApproval, Started: now.Add(-5 * time.Second)},
		{ID: "a4", Label: "broke", Mode: "explore", State: subagent.Failed, Err: "boom", Started: now, Updated: now},
		{ID: "a5", Label: "halted\x1b[2J", Mode: "explore", State: subagent.Stopped, Started: now, Updated: now},
	}
	options, ids := agentPickerOptions(infos, now)
	if strings.Join(ids, ",") != ",a2,a3,a1,a4,a5" {
		t.Fatalf("rows in order %q", ids)
	}
	want := []struct{ label, hint, group string }{
		{"main", "the main conversation", ""},
		{"a2  fix flaky test", "▶ running · 12s · Read", "Running"},
		{"a3  asks", "◌ needs approval · 5s · working", "Needs approval"},
		{"a1  done one", "· idle · 1m 0s · " + truncateForLog(long, 60), "Finished"},
		{"a4  broke", "✗ failed · 0s · boom", "Finished"},
		{"a5  halted\\x1b[2J", "■ stopped · 0s", "Finished"},
	}
	for i, w := range want {
		o := options[i]
		if o.label != w.label || o.group != w.group || style.StripANSI(o.hint) != w.hint {
			t.Errorf("row %d: got %q / %q / %q, want %q / %q / %q", i, o.label, o.hint, o.group, w.label, w.hint, w.group)
		}
	}
	if !options[0].current {
		t.Error("main should be the current row")
	}
}

func TestAgentsTableIsPlainText(t *testing.T) {
	withColor(t, false)
	now := time.Now()
	table := renderAgentsTable([]subagent.Info{
		{ID: "a1", Label: "look around", Mode: "explore", State: subagent.Running, Activity: "Read", Started: now.Add(-3 * time.Second)},
		{ID: "a2", Label: "gone", Mode: "general", State: subagent.Idle, ReportHead: "all good", Evicted: true, Started: now, Updated: now},
	}, now)
	if strings.Contains(table, "\x1b") {
		t.Fatalf("plain output has escapes: %q", table)
	}
	for _, want := range []string{"Subagents:", "a1  > running", "3s", "explore", "look around", "Read", "a2  - idle, dismissed", "all good"} {
		if !strings.Contains(table, want) {
			t.Errorf("expected %q in:\n%s", want, table)
		}
	}
	if renderAgentsTable(nil, now) != "No subagents yet; the model starts them with spawn_agent." {
		t.Error("an empty table should say so")
	}
	if label := renderApprovalLabel("a2", "fix\x1b[2J it"); label != "o a2 - fix\\x1b[2J it wants to run:" {
		t.Fatalf("plain approval label: %q", label)
	}
}

// echoRunner answers each turn with "reply to <prompt>" as an event and a report,
// recording the prompts.
type echoRunner struct {
	mu      sync.Mutex
	prompts []string
	causes  []error
	block   chan struct{}
}

func (r *echoRunner) run(ctx context.Context, a *subagent.Agent, prompt string) (string, error) {
	r.mu.Lock()
	r.prompts = append(r.prompts, prompt)
	block := r.block
	r.mu.Unlock()
	if block != nil && strings.Contains(prompt, "slow") {
		select {
		case <-block:
		case <-ctx.Done():
			r.mu.Lock()
			r.causes = append(r.causes, context.Cause(ctx))
			r.mu.Unlock()
			return "", context.Cause(ctx)
		}
	}
	reply := "reply to " + prompt
	a.Ring().Append(subagent.Event{Kind: subagent.EventText, Text: reply})
	return reply, nil
}

func (r *echoRunner) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.prompts...)
}

func TestAttachRoutesWhatIsTyped(t *testing.T) {
	withColor(t, false)
	runner := &echoRunner{block: make(chan struct{})}
	cfg, h := uiTestHost(t, runner.run)
	a := spawnTest(t, h, "helper", "first")
	waitFor(t, "a1 idle", func() bool { return a.State() == subagent.Idle })
	b := spawnTest(t, h, "other", "first")
	waitFor(t, "a2 idle", func() bool { return b.State() == subagent.Idle })
	ctx := context.Background()

	var action attachAction
	var target string
	out := captureTerminal(t, func() {
		// Text starts a new turn of an idle agent, shown as the user's message.
		action, _ = h.attachLine(ctx, cfg, a, "  hello there ")
		waitFor(t, "the reply", func() bool { return a.State() == subagent.Idle && a.Report() == "reply to hello there" })
	})
	if action != attachStay || strings.Join(runner.seen(), "|") != "first|first|hello there" {
		t.Fatalf("text should go to the agent: %v %q", action, runner.seen())
	}
	events := a.Ring().Since(0)
	if len(events) < 2 || events[len(events)-2].Kind != subagent.EventUser || events[len(events)-2].Text != "hello there" {
		t.Fatalf("the message should be echoed before the reply: %+v", events)
	}
	if out != "" {
		t.Errorf("sending prints nothing more: %q", out)
	}

	out = captureTerminal(t, func() {
		for _, line := range []string{"/clear", "/model x", "!ls", "/theme"} {
			if action, _ := h.attachLine(ctx, cfg, a, line); action != attachStay {
				t.Errorf("%s: %v", line, action)
			}
		}
	})
	if strings.Count(out, "commands act on the main conversation; /back first") != 4 {
		t.Fatalf("main's commands are refused here:\n%s", out)
	}
	if len(runner.seen()) != 3 {
		t.Fatalf("refused commands must not reach the agent: %q", runner.seen())
	}

	out = captureTerminal(t, func() {
		action, _ = h.attachLine(ctx, cfg, a, "/help")
	})
	if action != attachStay || !strings.Contains(out, "/agents") {
		t.Fatalf("/help runs: %v\n%s", action, out)
	}
	out = captureTerminal(t, func() {
		action, _ = h.attachLine(ctx, cfg, a, "/agents")
	})
	if !strings.Contains(out, "Subagents:") || !strings.Contains(out, "a2") {
		t.Fatalf("/agents lists them:\n%s", out)
	}
	captureTerminal(t, func() { action, target = h.attachLine(ctx, cfg, a, "/agents a2") })
	if action != attachSwitch || target != "a2" {
		t.Fatalf("/agents a2 switches: %v %q", action, target)
	}
	if action, _ := h.attachLine(ctx, cfg, a, "/BACK"); action != attachBack {
		t.Fatalf("/back returns: %v", action)
	}

	// /stop interrupts a running turn; the agent keeps its conversation.
	captureTerminal(t, func() {
		h.attachLine(ctx, cfg, a, "slow one")
		waitFor(t, "a1 running", func() bool { return a.State().Busy() })
		// A message to a running agent waits in its inbox; it shows when a turn takes it.
		h.attachLine(ctx, cfg, a, "and more")
		h.attachLine(ctx, cfg, a, "/stop")
		// The interrupted turn ends; the message in the inbox then starts the next.
		waitFor(t, "the next turn", func() bool { return a.Report() == "reply to and more" && a.State() == subagent.Idle })
	})
	runner.mu.Lock()
	causes := runner.causes
	runner.mu.Unlock()
	if len(causes) != 1 || !errors.Is(causes[0], subagent.ErrUserInterrupt) {
		t.Fatalf("/stop should interrupt the turn: %v", causes)
	}
	// The message waited in the inbox, so it shows once, as the turn that took it
	// started.
	echoes := 0
	for _, e := range a.Ring().Since(0) {
		if e.Kind == subagent.EventUser && e.Text == "and more" {
			echoes++
		}
	}
	if echoes != 1 {
		t.Fatalf("a message should show once, got %d", echoes)
	}
}

func TestStopAndDismissFromTheUser(t *testing.T) {
	withColor(t, false)
	var returned atomic.Int32
	cfg, h := uiTestHost(t, countingBlockingRunner(false, &returned))
	spawnTest(t, h, "one", "p")
	spawnTest(t, h, "two", "p")
	waitFor(t, "two running", func() bool { r, _, _ := h.m.Counts(); return r == 2 })
	out := captureTerminal(t, func() {
		if err := commandAgents(context.Background(), cfg, []string{"dismiss", "a1"}); err == nil || !strings.Contains(err.Error(), "running") {
			t.Errorf("a running agent cannot be dismissed: %v", err)
		}
		if err := commandAgents(context.Background(), cfg, []string{"stop", "all"}); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, "Stopped a1, a2.") {
		t.Fatalf("got %q", out)
	}
	waitFor(t, "the turns to return", func() bool { return returned.Load() == 2 })
	// The runners returned, but their goroutines let go of the agents just after; until
	// then an agent cannot be dismissed.
	waitFor(t, "the turn goroutines to end", func() bool { return h.m.Active() == 0 })
	out = captureTerminal(t, func() {
		if err := commandAgents(context.Background(), cfg, []string{"dismiss", "finished"}); err != nil {
			t.Error(err)
		}
		if err := commandAgents(context.Background(), cfg, nil); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, "Dismissed a1, a2") || !strings.Contains(out, "a1  # stopped, dismissed") {
		t.Fatalf("got %q", out)
	}
	if err := commandAgents(context.Background(), cfg, []string{"a9"}); err == nil || !strings.Contains(err.Error(), "no such agent") {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestAgentsLogReadsHistoryEvenAfterDismissal(t *testing.T) {
	withColor(t, false)
	chdirForTest(t, t.TempDir())
	runner := &echoRunner{}
	cfg, h := uiTestHost(t, runner.run)
	a := spawnTest(t, h, "reader", "go")
	waitFor(t, "a1 idle", func() bool { return a.State() == subagent.Idle })
	for _, e := range []historyEntry{
		{Type: "user", Content: "[[x]] read a.txt", ConversationID: a.ConvID},
		{Type: "tool_call", ToolName: "read_file", Arguments: `{"path":"a.txt"}`, ConversationID: a.ConvID},
		{Type: "tool_output", ToolName: "read_file", Content: "A\x1b[2J", Status: "ok", ConversationID: a.ConvID},
		{Type: "assistant", Content: "a.txt says A", ConversationID: a.ConvID},
		{Type: "assistant", Content: "main's own reply", ConversationID: "c-main"},
	} {
		recordHistory(e)
	}
	h.m.TakeNotices("")
	if err := h.m.Dismiss("a1"); err != nil {
		t.Fatal(err)
	}
	out := captureTerminal(t, func() {
		if err := commandAgents(context.Background(), cfg, []string{"log", "a1"}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"a1 - reader - explore", "> [[x]] read a.txt", "Read(a.txt)", "a.txt says A"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in the log:\n%s", want, out)
		}
	}
	if strings.Contains(out, "main's own reply") || strings.Contains(out, "\x1b[2J") {
		t.Fatalf("the log shows only the agent's entries, escaped:\n%s", out)
	}
	// Picking a dismissed agent shows its log too.
	out = captureTerminal(t, func() {
		if err := commandAgents(context.Background(), cfg, []string{"a1"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "a.txt says A") {
		t.Fatalf("got %q", out)
	}
}

func TestPlainAttachAsksForMessagesAndShowsTheReply(t *testing.T) {
	withColor(t, false)
	runner := &echoRunner{}
	cfg, h := uiTestHost(t, runner.run)
	a := spawnTest(t, h, "helper", "first")
	waitFor(t, "a1 idle", func() bool { return a.State() == subagent.Idle })
	cfg.input = newLineReader(strings.NewReader("ping\n/clear\n\n"))
	out := captureTerminal(t, func() {
		if err := commandAgents(context.Background(), cfg, []string{"a1"}); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(out, "\x1b") {
		t.Fatalf("plain output has escapes: %q", out)
	}
	for _, want := range []string{"-- a1 - helper - explore --", "reply to first", "message for a1 (empty returns): ", "> ping", "reply to ping", "commands act on the main conversation"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in:\n%s", want, out)
		}
	}
}

func TestReplayShowsTheLatestEventsUnderACount(t *testing.T) {
	withColor(t, false)
	_, h := uiTestHost(t, (&echoRunner{}).run)
	a := spawnTest(t, h, "chatty", "go")
	waitFor(t, "a1 idle", func() bool { return a.State() == subagent.Idle })
	for i := range 70 {
		a.Ring().Append(subagent.Event{Kind: subagent.EventNotice, Text: "event " + string(rune('a'+i%26))})
	}
	text, cursor := replayAgent(a, "what")
	if !strings.Contains(text, "... 11 earlier events (/agents log a1)") || strings.Count(text, "event ") != 60 {
		t.Fatalf("got:\n%s", text)
	}
	if events := a.Ring().Since(0); cursor != events[len(events)-1].Seq {
		t.Fatalf("cursor %d", cursor)
	}
}

func TestCtrlGOpensTheAgentsPicker(t *testing.T) {
	e := &promptEditor{}
	if done, err := typeKeys(t, e, "h", "i", "\x07"); !done || !errors.Is(err, errOpenAgents) || string(e.buf) != "hi" {
		t.Fatalf("got done=%v err=%v buf=%q", done, err, string(e.buf))
	}

	// During a turn it leaves a marker: /agents runs first once the turn is over.
	resetTurnQueue(t)
	typeQueueKeys("later\r")
	queueKey("\x07")
	typeQueueKeys("typing")
	if len(lineQueued) == 0 {
		t.Error("Ctrl-G should wake the attach view")
	}
	turnQueue.capturing = true
	lines, _ := queuedInputLines(80)
	if !strings.Contains(style.StripANSI(strings.Join(lines, "\n")), "queued  /agents") {
		t.Errorf("the marker should show: %q", lines)
	}
	if text, ok := nextQueuedInput(); !ok || text != "/agents" {
		t.Fatalf("got %q %v", text, ok)
	}
	if text, _ := nextQueuedInput(); text != "later" || turnQueue.editorText() != "typing" {
		t.Fatalf("got %q, editor %q", text, turnQueue.editorText())
	}
	// After an interrupted turn the marker is dropped, not put in the prompt.
	queueKey("\x07")
	input := newLineReader(strings.NewReader(""))
	returnQueueToPrompt(input)
	if input.partial != "" || turnQueue.openAgents {
		t.Fatalf("got %q", input.partial)
	}
}

func TestQueuedLinesGoBackToTheEditorOnDetach(t *testing.T) {
	resetTurnQueue(t)
	typeQueueKeys("one\rtwo\rthr")
	if line, ok := takeQueuedLine(); !ok || line != "one" {
		t.Fatalf("got %q", line)
	}
	returnQueuedLinesToEditor()
	if turnQueue.editorText() != "two\nthr" || len(turnQueue.items) != 0 {
		t.Fatalf("got %q %q", turnQueue.editorText(), turnQueue.items)
	}
}

// keyPipe is typed input for the prompt editor.
func keyPipe(t *testing.T) (*lineReader, *io.PipeWriter) {
	t.Helper()
	r, w := io.Pipe()
	t.Cleanup(func() { w.Close() })
	return newLineReader(r), w
}

// editInBackground runs editPrompt on its own goroutine, with stdout captured.
func editInBackground(t *testing.T, ctx context.Context, input *lineReader, events *promptEvents) <-chan editResult {
	t.Helper()
	result := make(chan editResult, 1)
	go func() {
		var r editResult
		captureStdout(t, func() {
			r.text, _, r.err = editPrompt(ctx, input, nil, "", 80, nil, events)
		})
		result <- r
	}()
	return result
}

type editResult struct {
	text string
	err  error
}

func noCbreak(t *testing.T) {
	t.Helper()
	previous := enterCbreak
	enterCbreak = func() (func(), error) { return func() {}, nil }
	t.Cleanup(func() { enterCbreak = previous })
}

func TestEditPromptGivesWayToAgentEventsAndKeepsTheText(t *testing.T) {
	withColor(t, true)
	noCbreak(t)
	input, keys := keyPipe(t)
	changed := make(chan struct{}, 1)
	var checks atomic.Int32
	var fire, footers atomic.Int32
	events := &promptEvents{
		changed: changed,
		interrupt: func(empty bool) bool {
			checks.Add(1)
			return fire.Load() == 1
		},
		footer: func() string { footers.Add(1); return "FOOTER" },
	}
	result := editInBackground(t, context.Background(), input, events)
	io.WriteString(keys, "ab")
	waitFor(t, "two keys", func() bool { return checks.Load() == 3 })
	// A nudge that changes nothing only redraws the footer.
	changed <- struct{}{}
	waitFor(t, "the footer redrawn", func() bool { return footers.Load() == 2 })
	fire.Store(1)
	changed <- struct{}{}
	if r := <-result; !errors.Is(r.err, errUIEvent) || input.partial != "ab" || r.text != "" {
		t.Fatalf("got %v, partial %q", r.err, input.partial)
	}

	// The prompt comes back with the text, and goes on from there.
	fire.Store(0)
	result = editInBackground(t, context.Background(), input, events)
	io.WriteString(keys, "c\r")
	if r := <-result; r.err != nil || r.text != "abc" || input.partial != "" {
		t.Fatalf("got %v %q, partial %q", r.err, r.text, input.partial)
	}

	// A context ended to make the prompt give way keeps the text too.
	ctx, cancel := context.WithCancelCause(context.Background())
	checks.Store(0)
	result = editInBackground(t, ctx, input, events)
	io.WriteString(keys, "xy")
	waitFor(t, "two keys", func() bool { return checks.Load() == 3 })
	cancel(fmt.Errorf("an agent event: %w", errPromptYield))
	if r := <-result; !errors.Is(r.err, context.Canceled) || input.partial != "xy" {
		t.Fatalf("got %v, partial %q", r.err, input.partial)
	}
}

func TestCtrlCAtTheIdlePromptClearsTheLine(t *testing.T) {
	withColor(t, true)
	noCbreak(t)
	input, keys := keyPipe(t)
	var checks atomic.Int32
	events := &promptEvents{interrupt: func(bool) bool { checks.Add(1); return false }}
	// The prompt's context ends the way readPrompt's does on Ctrl-C: a plain cancel.
	ctx, cancel := context.WithCancel(context.Background())
	result := editInBackground(t, ctx, input, events)
	io.WriteString(keys, "typed")
	waitFor(t, "the keys", func() bool { return checks.Load() == 6 })
	cancel()
	if r := <-result; !errors.Is(r.err, context.Canceled) || input.partial != "" {
		t.Fatalf("Ctrl-C should clear the line as before: %v, partial %q", r.err, input.partial)
	}

	// A toolchain whose signal.NotifyContext cancels with a cause naming the signal is
	// still the user's Ctrl-C: the line is cleared.
	ctx, cancelCause := context.WithCancelCause(context.Background())
	checks.Store(0)
	result = editInBackground(t, ctx, input, events)
	io.WriteString(keys, "typed")
	waitFor(t, "the keys", func() bool { return checks.Load() == 6 })
	cancelCause(errors.New("interrupt signal received"))
	if r := <-result; !errors.Is(r.err, context.Canceled) || input.partial != "" {
		t.Fatalf("Ctrl-C with a cause should clear the line: %v, partial %q", r.err, input.partial)
	}

	// Ctrl-G gives way to the picker and keeps the text for the prompt after it.
	result = editInBackground(t, context.Background(), input, events)
	io.WriteString(keys, "hi\x07")
	if r := <-result; !errors.Is(r.err, errOpenAgents) || input.partial != "hi" {
		t.Fatalf("got %v, partial %q", r.err, input.partial)
	}
}

func TestEditPromptWaitsForAnEmptyLineBeforeAnApproval(t *testing.T) {
	withColor(t, true)
	noCbreak(t)
	_, h := uiTestHost(t, blockingRunner(false))
	input, keys := keyPipe(t)
	// A request waits in the broker for the whole test.
	reqCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go h.m.Broker.Request(reqCtx, &subagent.ApprovalRequest{AgentID: "a1", Command: "ls"})
	waitFor(t, "the request", func() bool { return h.m.Broker.Len() > 0 })

	// With nothing typed the prompt gives way at once.
	var err error
	captureStdout(t, func() {
		_, _, err = editPrompt(context.Background(), input, nil, "", 80, nil, h.promptEvents(func() string { return "F" }))
	})
	if !errors.Is(err, errUIEvent) {
		t.Fatalf("got %v", err)
	}
	// With text typed it waits until the line is empty again.
	input.partial = "keep"
	result := editInBackground(t, context.Background(), input, h.promptEvents(func() string { return "F" }))
	io.WriteString(keys, "!")
	time.Sleep(50 * time.Millisecond)
	select {
	case r := <-result:
		t.Fatalf("gave way with text typed: %v", r.err)
	default:
	}
	io.WriteString(keys, "\x15") // Ctrl-U empties the line
	if r := <-result; !errors.Is(r.err, errUIEvent) || input.partial != "" {
		t.Fatalf("got %v, partial %q", r.err, input.partial)
	}
}
