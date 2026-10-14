package subagent

import (
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"go.uber.org/goleak"
)

func TestRingKeepsBothCapsUnderRandomAppends(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	const maxEvents, maxBytes, eventText = 50, 4 << 10, 512
	r := NewEventRing(maxEvents, maxBytes, eventText)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 10000 {
		e := Event{Kind: EventKind(rng.IntN(5)), Text: strings.Repeat("x", rng.IntN(1200)), Output: strings.Repeat("é", rng.IntN(300))}
		r.Append(e)
		if r.n > maxEvents || (r.n > 1 && r.bytes > maxBytes) {
			t.Fatalf("after %d appends: %d events, %d bytes", i+1, r.n, r.bytes)
		}
	}
	events := r.Since(0)
	if len(events) != r.n || r.Dropped() != 10000-len(events) {
		t.Fatalf("%d events, %d dropped", len(events), r.Dropped())
	}
	total := 0
	for i, e := range events {
		if len(e.Text) > eventText || len(e.Output) > eventText || !utf8.ValidString(e.Output) {
			t.Fatalf("event %d not truncated: %d %d", i, len(e.Text), len(e.Output))
		}
		if i > 0 && e.Seq != events[i-1].Seq+1 {
			t.Fatalf("seq %d after %d", e.Seq, events[i-1].Seq)
		}
		total += e.size()
	}
	if total != r.bytes || events[len(events)-1].Seq != 10000 {
		t.Fatalf("bytes %d vs %d, last seq %d", total, r.bytes, events[len(events)-1].Seq)
	}
}

func TestRingSinceReturnsACopy(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	r := NewEventRing(10, 1<<10, 100)
	r.Append(Event{Text: "a"})
	r.Append(Event{Text: "b"})
	r.Append(Event{Text: "c"})
	got := r.Since(1)
	if len(got) != 2 || got[0].Text != "b" || got[1].Seq != 3 || got[0].At.IsZero() {
		t.Fatalf("got %+v", got)
	}
	got[0].Text = "changed"
	if again := r.Since(1); again[0].Text != "b" {
		t.Fatal("Since shares the ring's memory")
	}
	if len(r.Since(3)) != 0 {
		t.Fatal("events after the last")
	}
	select {
	case <-r.Notify():
	default:
		t.Fatal("no nudge")
	}
}

func TestRingKeepsAnOversizedNewestEvent(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	r := NewEventRing(10, 10, 0)
	r.Append(Event{Text: "small"})
	r.Append(Event{Text: strings.Repeat("x", 100)})
	if got := r.Since(0); len(got) != 1 || len(got[0].Text) != 100 || r.Dropped() != 1 {
		t.Fatalf("got %d events, %d dropped", len(got), r.Dropped())
	}
}

func TestRingConcurrentAppendAndSince(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	r := NewEventRing(100, 8<<10, 256)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 5000 {
			r.Append(Event{Text: "streamed text"})
		}
	}()
	go func() {
		defer wg.Done()
		var cursor uint64
		for cursor < 5000 {
			for _, e := range r.Since(cursor) {
				if e.Seq <= cursor {
					t.Errorf("seq %d not after %d", e.Seq, cursor)
					return
				}
				cursor = e.Seq
			}
			_ = r.Dropped()
		}
	}()
	wg.Wait()
}

func TestTruncateTextKeepsRunesWhole(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	got := truncateText(strings.Repeat("日本", 100), 40)
	if len(got) > 40 || !utf8.ValidString(got) || !strings.HasSuffix(got, "(truncated)") {
		t.Fatalf("got %q", got)
	}
	if truncateText("short", 40) != "short" {
		t.Fatal("short text changed")
	}
}
