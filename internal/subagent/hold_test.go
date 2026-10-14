package subagent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// A held agent keeps the result of the turn main follows until Release: it is not
// dismissed, and neither a stop nor a turn the user's message starts changes the
// recorded result.
func TestHoldKeepsTheFollowedTurnsResultUntilRelease(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	m := NewManager(Config{}, instantRunner)
	a, err := m.Spawn(SpawnOptions{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	m.Hold(a)
	settle(t, a)
	if err := m.Dismiss(a.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("dismiss of a held agent: %v", err)
	}
	if ids := m.DismissFinished(); len(ids) != 0 {
		t.Fatalf("dismissed held agents %v", ids)
	}
	d, err := m.Deliver(a.ID, "chat", true)
	if err != nil || d != Started {
		t.Fatalf("deliver to a held agent: %v %v", d, err)
	}
	eventually(t, "the user's turn", func() bool { return a.State() == Idle && a.Report() == "done: chat" })
	settle(t, a)
	if got := a.LastResult(); got.State != Idle || got.Report != "done: x" || got.Err != nil {
		t.Fatalf("the user's turn replaced the kept result: %+v", got)
	}

	m.Release(a)

	// A stop after the turn ended leaves the recorded result as it was.
	m.Hold(a)
	if err := m.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	if got := a.LastResult(); got.State != Idle || got.Err != nil {
		t.Fatalf("a stop changed the last result: %+v", got)
	}
	// The user's message resumes the stopped agent.
	if d, err := m.Deliver(a.ID, "again", true); err != nil || d != Started {
		t.Fatalf("deliver to a held stopped agent: %v %v", d, err)
	}
	eventually(t, "the resumed turn", func() bool { return a.State() == Idle && a.Report() == "done: again" })
	if got := a.LastResult(); got.Report != "done: chat" {
		t.Fatalf("the resumed turn replaced the kept result: %+v", got)
	}
	m.Release(a)
	settle(t, a)
	if err := m.Dismiss(a.ID); err != nil {
		t.Fatalf("dismiss once released: %v", err)
	}
	shutdown(t, m)
}

// A message that lands as a held agent writes its final answer goes on in a turn of its
// own: the followed turn ends there, and main gets the task's report, not the reply.
func TestHeldAgentEndsTheTurnBeforeARacedMessage(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 8)
	m := NewManager(Config{}, fakeRunner(calls, true))
	a, _ := m.Spawn(SpawnOptions{Label: "kid", ParentConv: "c1", Prompt: "check X"})
	m.Hold(a)
	done := a.TurnDone()
	first := next(t, calls)
	if d, err := m.Deliver(a.ID, "how many files?", true); err != nil || d != AfterTask {
		t.Fatalf("got %v %v", d, err)
	}
	first.reply <- reply{text: "X findings"}
	second := next(t, calls)
	select {
	case <-done:
	default:
		t.Fatal("the followed turn should end before the message's turn")
	}
	if !strings.Contains(second.prompt, "how many files?") {
		t.Fatalf("the held message's turn got %q", second.prompt)
	}
	second.reply <- reply{text: "3 files"}
	eventually(t, "idle", func() bool { return a.State() == Idle && a.Report() == "3 files" })
	if got := a.LastResult(); got.Report != "X findings" || got.Err != nil {
		t.Fatalf("main should get the task's report, got %+v", got)
	}
	if n := m.NoticeCount(); n != 0 {
		t.Fatalf("main takes the report itself; got %d notices", n)
	}
	m.Release(a)
	shutdown(t, m)
}

// While main follows a turn, the user's message is not steered into it: the runner's
// TakeInbox leaves it, and it goes on in a turn of its own once the followed one ends.
func TestHeldTurnIsNotSteeredByTheUser(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 8)
	steered := make(chan []string, 1)
	inner := fakeRunner(calls, true)
	runner := func(ctx context.Context, a *Agent, prompt string) (string, error) {
		if prompt == "check X" {
			c := call{a: a, prompt: prompt, reply: make(chan reply, 1)}
			calls <- c
			<-c.reply // the tool round
			steered <- a.TakeInbox()
			return "X findings", nil
		}
		return inner(ctx, a, prompt)
	}
	m := NewManager(Config{}, runner)
	a, _ := m.Spawn(SpawnOptions{Label: "kid", ParentConv: "c1", Prompt: "check X"})
	m.Hold(a)
	first := next(t, calls)
	if d, err := m.Deliver(a.ID, "how many files?", true); err != nil || d != AfterTask {
		t.Fatalf("got %v %v", d, err)
	}
	first.reply <- reply{}
	if got := <-steered; len(got) != 0 {
		t.Fatalf("the user's message steered the followed turn: %q", got)
	}
	second := next(t, calls)
	if got := a.LastResult(); got.Report != "X findings" {
		t.Fatalf("main should get the task's report, got %+v", got)
	}
	if !strings.Contains(second.prompt, "how many files?") {
		t.Fatalf("the message's turn got %q", second.prompt)
	}
	second.reply <- reply{text: "3 files"}
	eventually(t, "idle", func() bool { return a.State() == Idle && a.Report() == "3 files" })
	m.Release(a)
	shutdown(t, m)
}

// Main marks delivered the turn it followed, not a turn of the user's that ended since:
// a later wait still gives the user's turn's report.
func TestMarkDeliveredLeavesTheUsersLaterTurn(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	m := NewManager(Config{}, instantRunner)
	a, _ := m.Spawn(SpawnOptions{Prompt: "x"})
	m.Hold(a)
	settle(t, a)
	if d, err := m.Deliver(a.ID, "chat", true); err != nil || d != Started {
		t.Fatalf("got %v %v", d, err)
	}
	eventually(t, "the user's turn", func() bool { return a.State() == Idle && a.Report() == "done: chat" })
	settle(t, a)
	m.MarkDelivered(a.ID)
	m.Release(a)
	res, err := m.Wait(context.Background(), []string{a.ID}, true, time.Second)
	if err != nil || len(res.Done) != 1 || res.Done[0].Delivered || res.Done[0].Report != "done: chat" {
		t.Fatalf("the user's turn was marked delivered: %+v %v", res, err)
	}
	shutdown(t, m)
}
