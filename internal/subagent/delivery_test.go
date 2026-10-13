package subagent

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// userEvents are the texts of the EventUser events in a's ring.
func userEvents(a *Agent) []string {
	var texts []string
	for _, e := range a.Ring().Since(0) {
		if e.Kind == EventUser {
			texts = append(texts, e.Text)
		}
	}
	return texts
}

func TestOnlyTheModelsWorkIsReportedToTheModel(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 8)
	m := NewManager(Config{}, fakeRunner(calls, true))
	a, _ := m.Spawn(SpawnOptions{Label: "kid", ParentConv: "c1", Background: true, Prompt: "task"})
	next(t, calls).reply <- reply{text: "task done"}
	waitDone(t, a)
	if n := m.TakeNotices(""); len(n) != 1 || !n[0].ForModel {
		t.Fatalf("the spawn's report is the model's: %+v", n)
	}

	// A turn the user starts from the attach view, after leaving it, is only shown.
	if _, err := m.Send(a.ID, "user question", true); err != nil {
		t.Fatal(err)
	}
	next(t, calls).reply <- reply{text: "answer for the user"}
	waitDone(t, a)
	if n := m.TakeNotices(""); len(n) != 1 || n[0].ForModel {
		t.Fatalf("a user turn must not reach the model: %+v", n)
	}
	// The model's wait finds nothing it is owed.
	if res, err := m.Wait(context.Background(), nil, true, time.Second); err != nil || len(res.Done) != 0 {
		t.Fatalf("got %+v %v", res, err)
	}

	// While the user looks on, a turn the user starts is not announced at all.
	m.SetAttached(a.ID)
	if _, err := m.Send(a.ID, "another", true); err != nil {
		t.Fatal(err)
	}
	user := next(t, calls)
	// The model sends a message while the user's turn runs: that turn is now owed.
	if d, err := m.Deliver(a.ID, "from the model", false); err != nil || d != Steered {
		t.Fatalf("got %v %v", d, err)
	}
	user.reply <- reply{text: "steered"}
	// The model's message was not taken by the turn, so it starts the next one.
	next(t, calls).reply <- reply{text: "for the model"}
	eventually(t, "idle", func() bool { return a.State() == Idle })
	n := m.TakeNotices("")
	if len(n) != 1 || !n[0].ForModel || a.Report() != "for the model" {
		t.Fatalf("the model's message is reported to it: %+v", n)
	}
	m.SetAttached("")
	shutdown(t, m)
}

func TestUserMessagesShowOnceWhenATurnTakesThem(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	var m *Manager
	turns := 0
	m = NewManager(Config{}, func(ctx context.Context, a *Agent, prompt string) (string, error) {
		turns++
		a.Ring().Append(Event{Kind: EventText, Text: "reply " + prompt})
		if turns == 2 {
			// The user's message lands after the runner's last look at the inbox, so
			// the same goroutine takes it as the next prompt.
			if d, err := m.Deliver(a.ID, "late\x1b[2J", true); err != nil || d != Steered {
				return "", err
			}
		}
		return "ok", nil
	})
	a, _ := m.Spawn(SpawnOptions{Prompt: "start"})
	waitDone(t, a)
	eventually(t, "the goroutine to exit", func() bool { return goroutines(a) == 0 })

	// An idle agent: the message shows before anything the turn writes.
	if d, err := m.Deliver(a.ID, "hello", true); err != nil || d != Started {
		t.Fatalf("got %v %v", d, err)
	}
	eventually(t, "the turns to end", func() bool { return a.State() == Idle && goroutines(a) == 0 })
	if got := userEvents(a); len(got) != 2 || got[0] != "hello" || got[1] != `late\x1b[2J` {
		t.Fatalf("each user message shows once, escaped: %q", got)
	}
	events := a.Ring().Since(0)
	var order []string
	for _, e := range events {
		order = append(order, e.Text)
	}
	want := []string{"reply start", "hello", "reply hello", `late\x1b[2J`, "reply late\x1b[2J"}
	if len(order) != len(want) {
		t.Fatalf("events %q", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("events %q", order)
		}
	}
	shutdown(t, m)
}

func TestAReportReachesTheModelOnce(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	m := NewManager(Config{}, instantRunner)
	a, _ := m.Spawn(SpawnOptions{Label: "one", ParentConv: "c1", Background: true, Prompt: "x"})
	waitDone(t, a)
	// Main injects the notice and marks it delivered.
	if n := m.TakeNotices(""); len(n) != 1 {
		t.Fatalf("notices %+v", n)
	}
	m.MarkDelivered(a.ID)
	res, err := m.Wait(context.Background(), []string{a.ID}, true, time.Second)
	if err != nil || len(res.Done) != 1 || !res.Done[0].Delivered {
		t.Fatalf("a wait after the notice should say it was delivered: %+v %v", res, err)
	}

	// The other way round: wait takes the notice, and a second wait says delivered.
	b, _ := m.Spawn(SpawnOptions{Label: "two", ParentConv: "c1", Background: true, Prompt: "y"})
	waitDone(t, b)
	res, err = m.Wait(context.Background(), []string{b.ID}, true, time.Second)
	if err != nil || len(res.Done) != 1 || res.Done[0].Delivered || res.Done[0].Report != "done: y" {
		t.Fatalf("the first wait gives the report: %+v %v", res, err)
	}
	if n := m.TakeNotices(""); len(n) != 0 {
		t.Fatalf("wait consumes the notice: %+v", n)
	}
	if res, _ := m.Wait(context.Background(), []string{b.ID}, true, time.Second); !res.Done[0].Delivered {
		t.Fatalf("a second wait says delivered: %+v", res)
	}

	// A new turn's report is new.
	if _, err := m.Send(b.ID, "z", false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "b idle", func() bool { return b.State() == Idle && goroutines(b) == 0 })
	if res, _ := m.Wait(context.Background(), []string{b.ID}, true, time.Second); res.Done[0].Delivered || res.Done[0].Report != "done: z" {
		t.Fatalf("a new report is given: %+v", res)
	}
	shutdown(t, m)
}
