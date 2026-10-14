package subagent

import (
	"strings"
	"testing"

	"go.uber.org/goleak"
)

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
