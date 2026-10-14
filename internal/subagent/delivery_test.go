package subagent

import (
	"context"
	"testing"
	"time"

	"strings"

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

// A foreground agent's turn the user started from the attach view is followed by
// nobody once the user goes back to main, so a message the model steers it with gets
// its report as a notice, whether the turn takes the message or leaves it for the next.
func TestModelSteeringAForegroundUserTurnIsReported(t *testing.T) {
	for _, takes := range []bool{true, false} {
		calls := make(chan call, 8)
		m := NewManager(Config{}, fakeRunner(calls, true))
		a, _ := m.Spawn(SpawnOptions{Label: "kid", ParentConv: "c1", Prompt: "task"})
		next(t, calls).reply <- reply{text: "task done"}
		waitDone(t, a)
		eventually(t, "the goroutine to exit", func() bool { return goroutines(a) == 0 })
		m.MarkDelivered(a.ID) // follow gave the spawn's report
		if d, err := m.Deliver(a.ID, "user question", true); err != nil || d != Started {
			t.Fatalf("got %v %v", d, err)
		}
		user := next(t, calls)
		if d, err := m.Deliver(a.ID, "from the model", false); err != nil || d != Steered {
			t.Fatalf("got %v %v", d, err)
		}
		if takes {
			if got := user.a.TakeInbox(); len(got) != 1 {
				t.Fatalf("inbox %q", got)
			}
			user.reply <- reply{text: "steered answer"}
		} else {
			user.reply <- reply{text: "user answer"}
			next(t, calls).reply <- reply{text: "steered answer"}
		}
		eventually(t, "idle", func() bool { return a.State() == Idle && goroutines(a) == 0 })
		n := m.TakeNotices("")
		if len(n) != 1 || !n[0].ForModel {
			t.Fatalf("takes=%v: the model's steer must be reported to it: %+v", takes, n)
		}
		shutdown(t, m)
	}
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

// noticeTexts lists the notices' texts, marking the ones for the model.
func noticeTexts(notices []Notice) []string {
	var texts []string
	for _, n := range notices {
		mark := "user: "
		if n.ForModel {
			mark = "model: "
		}
		text := strings.TrimPrefix(n.Text, "<agent-report")
		_, text, _ = strings.Cut(text, "\n")
		text, _, _ = strings.Cut(text, "\n</agent-report>")
		texts = append(texts, mark+n.AgentID+" "+text)
	}
	return texts
}

// Delivering an agent's latest report leaves the notice of an earlier turn the model
// has not seen: here a queued message's turn, started once a slot freed, then a second
// message in the same round whose turn is followed.
func TestDeliveringTheLatestReportKeepsAnEarlierNotice(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 8)
	m := NewManager(Config{MaxRunning: 1}, fakeRunner(calls, true))
	f, _ := m.Spawn(SpawnOptions{Label: "fore", ParentConv: "c1", Prompt: "task"})
	next(t, calls).reply <- reply{text: "first"}
	settle(t, f) // the only slot is free once f's goroutine exits, not when its turn ends
	m.MarkDelivered(f.ID)

	b, err := m.Spawn(SpawnOptions{Label: "back", ParentConv: "c1", Background: true, Prompt: "other"})
	if err != nil {
		t.Fatal(err)
	}
	bCall := next(t, calls)
	if d, err := m.Deliver(f.ID, "queued", false); err != nil || d != Queued {
		t.Fatalf("got %v %v", d, err)
	}
	bCall.reply <- reply{text: "b done"}
	waitDone(t, b)
	kicked := next(t, calls)
	if kicked.prompt != "queued" {
		t.Fatalf("the queued message should start f: %q", kicked.prompt)
	}
	kicked.reply <- reply{text: "R1"}
	eventually(t, "f's kicked turn", func() bool { return f.State() == Idle && f.Report() == "R1" })
	eventually(t, "f's goroutine to exit", func() bool { return goroutines(f) == 0 })

	if d, err := m.Deliver(f.ID, "next", false); err != nil || d != Started {
		t.Fatalf("got %v %v", d, err)
	}
	next(t, calls).reply <- reply{text: "R2"}
	eventually(t, "f's second turn", func() bool { return f.State() == Idle && f.Report() == "R2" })
	m.MarkDelivered(f.ID)

	got := strings.Join(noticeTexts(m.TakeNotices("")), " | ")
	if !strings.Contains(got, "model: a1 R1") || strings.Contains(got, "R2") {
		t.Fatalf("R1 never reached the model, R2 did: %s", got)
	}
	shutdown(t, m)
}

// A user message that lands as an owed turn ends is answered in a turn of its own: the
// model gets the report of its work, not the reply to the user.
func TestOwedReportIsGivenWhenAUserMessageContinuesTheTurn(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 8)
	m := NewManager(Config{}, fakeRunner(calls, true))
	a, _ := m.Spawn(SpawnOptions{Label: "kid", ParentConv: "c1", Background: true, Prompt: "task"})
	m.SetAttached(a.ID)
	first := next(t, calls)
	if d, err := m.Deliver(a.ID, "user late", true); err != nil || d != Steered {
		t.Fatalf("got %v %v", d, err)
	}
	first.reply <- reply{text: "model work report"}
	second := next(t, calls)
	if second.prompt != "user late" {
		t.Fatalf("the user's message should be the next prompt: %q", second.prompt)
	}
	second.reply <- reply{text: "reply to user"}
	eventually(t, "idle", func() bool { return a.State() == Idle })
	got := noticeTexts(m.TakeNotices(""))
	if len(got) != 1 || got[0] != "model: a1 model work report" {
		t.Fatalf("got %q", got)
	}
	m.SetAttached("")
	shutdown(t, m)
}

// A message from the model that lands as the agent writes its final answer starts a
// follow-up turn; the answer to the original task still reaches the model.
func TestRacedMessageKeepsTheOriginalReport(t *testing.T) {
	for _, background := range []bool{true, false} {
		t.Run(map[bool]string{true: "background", false: "foreground"}[background], func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			calls := make(chan call, 8)
			m := NewManager(Config{}, fakeRunner(calls, true))
			a, _ := m.Spawn(SpawnOptions{Label: "kid", ParentConv: "c1", Background: background, Prompt: "check X"})
			first := next(t, calls)
			if d, err := m.Deliver(a.ID, "also check Y", false); err != nil || d != Steered {
				t.Fatalf("got %v %v", d, err)
			}
			first.reply <- reply{text: "X findings"}
			second := next(t, calls)
			second.reply <- reply{text: "Y findings"}
			eventually(t, "idle", func() bool { return a.State() == Idle })
			if !background {
				// follow returns the latest report and marks it delivered.
				m.MarkDelivered(a.ID)
			}
			got := strings.Join(noticeTexts(m.TakeNotices("")), " | ")
			if !strings.Contains(got, "model: a1 X findings") {
				t.Fatalf("the original task's report was lost: %s", got)
			}
			if background && !strings.Contains(got, "model: a1 Y findings") {
				t.Fatalf("the follow-up's report should be given too: %s", got)
			}
			shutdown(t, m)
		})
	}
}
