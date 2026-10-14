package subagent

import (
	"errors"
	"testing"

	"go.uber.org/goleak"
)

// A held agent keeps the result of the turn main follows until Release: it is not
// dismissed, a stop does not change the recorded result, and the user's message waits
// for Release instead of starting a turn that would replace it.
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
	if err != nil || d != Held {
		t.Fatalf("deliver to a held agent: %v %v", d, err)
	}
	if a.State() != Idle || a.Report() != "done: x" {
		t.Fatalf("the message started a turn: %s %q", a.State(), a.Report())
	}
	if got := a.LastResult(); got.State != Idle || got.Report != "done: x" || got.Err != nil {
		t.Fatalf("last result %+v", got)
	}

	m.Release(a)
	eventually(t, "the held message's turn", func() bool { return a.State() == Idle && a.Report() == "done: chat" })
	settle(t, a)
	if got := a.LastResult(); got.Report != "done: chat" {
		t.Fatalf("last result after the user's turn %+v", got)
	}

	// A stop after the turn ended leaves the recorded result as it was.
	m.Hold(a)
	if err := m.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	if got := a.LastResult(); got.State != Idle || got.Err != nil {
		t.Fatalf("a stop changed the last result: %+v", got)
	}
	// The user's message resumes the stopped agent once main has the result.
	if d, err := m.Deliver(a.ID, "again", true); err != nil || d != Held {
		t.Fatalf("deliver to a held stopped agent: %v %v", d, err)
	}
	if got := a.LastResult(); got.Report != "done: chat" || a.State().Busy() {
		t.Fatalf("the message started a turn while held: %s %+v", a.State(), got)
	}
	m.Release(a)
	eventually(t, "the resumed turn", func() bool { return a.State() == Idle && a.Report() == "done: again" })
	settle(t, a)
	if err := m.Dismiss(a.ID); err != nil {
		t.Fatalf("dismiss once released: %v", err)
	}
	shutdown(t, m)
}
