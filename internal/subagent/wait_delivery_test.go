package subagent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func delivered(a *Agent) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.delivered
}

func noticeIDs(m *Manager) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for _, n := range m.notices {
		ids = append(ids, n.AgentID)
	}
	return ids
}

// A wait the user interrupts, or one that stops to ask about a command, may never
// reach the model: the reports it found stay pending.
func TestAnInterruptedWaitKeepsTheReportsItFound(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 4)
	m := NewManager(Config{}, fakeRunner(calls, true))
	a1, _ := m.Spawn(SpawnOptions{Label: "one", ParentConv: "c", Background: true, Prompt: "a"})
	c1 := next(t, calls)
	a2, _ := m.Spawn(SpawnOptions{Label: "two", ParentConv: "c", Background: true, Prompt: "b"})
	next(t, calls)
	c1.reply <- reply{text: "report one"}
	waitDone(t, a1)

	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("ctrl-c")
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel(cause)
	}()
	res, err := m.Wait(ctx, nil, true, time.Minute)
	if !errors.Is(err, cause) || len(res.Done) != 1 || res.Done[0].ID != a1.ID {
		t.Fatalf("cancelled wait: %+v %v", res, err)
	}
	if delivered(a1) || strings.Join(noticeIDs(m), ",") != a1.ID {
		t.Fatalf("a cancelled wait consumed the report: delivered %v, notices %v", delivered(a1), noticeIDs(m))
	}

	// The same for a wait that stops for an approval request.
	go func() { _ = m.Broker.Request(context.Background(), &ApprovalRequest{AgentID: a2.ID, Command: "make"}) }()
	eventually(t, "the request", func() bool { return m.Broker.Len() == 1 })
	res, err = m.Wait(context.Background(), nil, true, time.Minute)
	if !errors.Is(err, ErrApprovalPending) || len(res.Done) != 1 {
		t.Fatalf("approval wait: %+v %v", res, err)
	}
	if delivered(a1) || len(noticeIDs(m)) != 1 {
		t.Fatalf("a wait stopped for approval consumed the report: delivered %v, notices %v", delivered(a1), noticeIDs(m))
	}

	// Once the caller hands it on, it is delivered and its notice goes.
	m.MarkDelivered(a1.ID)
	if !delivered(a1) || len(noticeIDs(m)) != 0 {
		t.Fatalf("after MarkDelivered: delivered %v, notices %v", delivered(a1), noticeIDs(m))
	}
	m.Broker.DrainAll(ErrShutdown)
	shutdown(t, m)
}

// A model's message to a foreground agent that has to wait for a slot starts a turn
// nobody follows: wait sees the agent as still running, and its report arrives as a
// notice that keeps it from being evicted.
func TestAQueuedModelMessageIsWaitedForAndAnnounced(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 4)
	m := NewManager(Config{MaxRunning: 1, MaxLive: 2}, fakeRunner(calls, true))
	a1, _ := m.Spawn(SpawnOptions{Label: "fg", ParentConv: "c", Prompt: "first"})
	next(t, calls).reply <- reply{text: "report of first"}
	settle(t, a1)
	m.MarkDelivered(a1.ID) // as follow does
	a2, _ := m.Spawn(SpawnOptions{Label: "bg", ParentConv: "c", Background: true, Prompt: "block"})
	c2 := next(t, calls)

	if d, err := m.Deliver(a1.ID, "second", false); err != nil || d != Queued {
		t.Fatalf("deliver: %v %v", d, err)
	}
	res, err := m.Wait(context.Background(), []string{a1.ID}, true, 20*time.Millisecond)
	if err != nil || !res.TimedOut || len(res.Done) != 0 || len(res.Running) != 1 {
		t.Fatalf("a queued agent should still be running: %+v %v", res, err)
	}
	res, err = m.Wait(context.Background(), nil, true, 20*time.Millisecond)
	if err != nil || strings.Join(res.Running, ",") != a1.ID+","+a2.ID {
		t.Fatalf("a bare wait should wait for the queued agent too: %+v %v", res, err)
	}

	c2.reply <- reply{text: "report of block"}
	c1 := next(t, calls)
	if c1.a != a1 || c1.prompt != "second" {
		t.Fatalf("kicked %s with %q", c1.a.ID, c1.prompt)
	}
	c1.reply <- reply{text: "report of second"}
	settle(t, a1)
	eventually(t, "both notices", func() bool { return len(noticeIDs(m)) == 2 })

	// The undelivered report keeps a1 live through the next spawn.
	a3, err := m.Spawn(SpawnOptions{Label: "third", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	next(t, calls).reply <- reply{text: "x"}
	waitDone(t, a3)
	if _, err := m.Get(a1.ID); err != nil {
		t.Fatalf("a1 was evicted with its report undelivered: %v", err)
	}
	var got *Notice
	for _, n := range m.TakeNotices("c") {
		if n.AgentID == a1.ID {
			got = &n
		}
	}
	if got == nil || !got.ForModel || !strings.Contains(got.Text, "report of second") {
		t.Fatalf("a1's report should reach the model: %+v", got)
	}
	shutdown(t, m)
}

// A foreground turn the model is followed through never leaves a notice behind.
func TestAFollowedForegroundTurnLeavesNoNotice(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	m := NewManager(Config{}, instantRunner)
	a, _ := m.Spawn(SpawnOptions{Label: "fg", ParentConv: "c", Prompt: "x"})
	settle(t, a)
	if d, err := m.Deliver(a.ID, "again", false); err != nil || d != Started {
		t.Fatalf("deliver: %v %v", d, err)
	}
	waitDone(t, a)
	if ids := noticeIDs(m); len(ids) != 0 {
		t.Fatalf("notices %v", ids)
	}
	shutdown(t, m)
}

// A stopped agent whose runner has not returned, and a resume queued behind it, still
// count as active: either may run a turn later.
func TestActiveCountsStoppedStragglersAndQueuedResumes(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 4)
	m := NewManager(Config{}, fakeRunner(calls, false))
	a, _ := m.Spawn(SpawnOptions{Prompt: "go"})
	c := next(t, calls)
	if err := m.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	if d, err := m.Deliver(a.ID, "resume", true); err != nil || d != Queued {
		t.Fatalf("resume: %v %v", d, err)
	}
	if running, _, _ := m.Counts(); running != 0 {
		t.Fatalf("running %d", running)
	}
	if n := m.Active(); n != 1 {
		t.Fatalf("active %d, want 1", n)
	}
	c.reply <- reply{text: "late"}
	r := next(t, calls)
	if r.prompt != "resume" {
		t.Fatalf("resumed with %q", r.prompt)
	}
	r.reply <- reply{text: "ok"}
	settle(t, a)
	if n := m.Active(); n != 0 {
		t.Fatalf("active %d after the turn, want 0", n)
	}
	shutdown(t, m)
}

// The user's "always" for an agent's command does not carry over to what the model
// sends the agent afterwards.
func TestAModelMessageEndsTheAgentsAlways(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	m := NewManager(Config{}, instantRunner)
	a, _ := m.Spawn(SpawnOptions{Prompt: "x"})
	settle(t, a)
	a.AutoApprove.Store(true)
	if _, err := m.Deliver(a.ID, "from the user", true); err != nil {
		t.Fatal(err)
	}
	settle(t, a)
	if !a.AutoApprove.Load() {
		t.Fatal("a user's message should keep the agent's always")
	}
	if _, err := m.Deliver(a.ID, "from the model", false); err != nil {
		t.Fatal(err)
	}
	if a.AutoApprove.Load() {
		t.Fatal("a model's message should end the agent's always")
	}
	settle(t, a)
	shutdown(t, m)
}

// A report cut to ReportCap says so, in the notice the model gets.
func TestACutReportIsMarkedInItsNotice(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	m := NewManager(Config{ReportCap: 1024}, func(ctx context.Context, a *Agent, prompt string) (string, error) {
		return strings.Repeat("x", 5000), nil
	})
	a, _ := m.Spawn(SpawnOptions{Prompt: "x", Background: true})
	waitDone(t, a)
	notices := m.TakeNotices("")
	if len(notices) != 1 {
		t.Fatalf("notices %+v", notices)
	}
	if n := strings.Count(notices[0].Text, "[report truncated at 1 KB; the user can view the full transcript with /agents log a1]"); n != 1 {
		t.Fatalf("truncation marker %d times in %q", n, notices[0].Text[len(notices[0].Text)-300:])
	}
	shutdown(t, m)
}
