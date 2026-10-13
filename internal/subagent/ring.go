package subagent

import (
	"sync"
	"time"
	"unicode/utf8"
)

// EventKind says what an Event records.
type EventKind int

// The kinds of Event a ring holds.
const (
	EventText EventKind = iota
	EventToolStart
	EventToolResult
	EventUser
	EventNotice
)

// Event is one typed record of an agent's turn: streamed text, a tool call and its
// result, a message from the user, or a notice. Replay renders it at the terminal's
// width at the time, so it holds no ANSI codes.
type Event struct {
	Kind    EventKind
	Text    string
	Tool    string
	Args    string
	Output  string
	Err     string
	Elapsed time.Duration
	At      time.Time
	Seq     uint64
}

func (e *Event) size() int {
	return len(e.Text) + len(e.Tool) + len(e.Args) + len(e.Output) + len(e.Err)
}

// EventRing keeps an agent's most recent events within a count and a byte cap. Old
// events are pushed out and counted as dropped; the full transcript stays in history.
// It is safe for one writer and any number of readers.
type EventRing struct {
	mu        sync.Mutex
	buf       []Event
	head, n   int
	bytes     int
	maxBytes  int
	eventText int
	seq       uint64
	dropped   int
	notify    chan struct{}
}

// NewEventRing returns a ring that holds at most maxEvents events and maxBytes bytes of
// text, cutting each text field of an event to eventText bytes.
func NewEventRing(maxEvents, maxBytes, eventText int) *EventRing {
	return &EventRing{
		buf:       make([]Event, max(maxEvents, 1)),
		maxBytes:  maxBytes,
		eventText: eventText,
		notify:    make(chan struct{}, 1),
	}
}

// Append stores e with the next sequence number, pushing out the oldest events until
// both caps hold, and nudges Notify without blocking. The newest event is always kept.
func (r *EventRing) Append(e Event) {
	e.Text = truncateText(e.Text, r.eventText)
	e.Args = truncateText(e.Args, r.eventText)
	e.Output = truncateText(e.Output, r.eventText)
	e.Err = truncateText(e.Err, r.eventText)
	if e.At.IsZero() {
		e.At = time.Now()
	}
	r.mu.Lock()
	r.seq++
	e.Seq = r.seq
	if r.n == len(r.buf) {
		r.dropOldest()
	}
	r.buf[(r.head+r.n)%len(r.buf)] = e
	r.n++
	r.bytes += e.size()
	for r.n > 1 && r.maxBytes > 0 && r.bytes > r.maxBytes {
		r.dropOldest()
	}
	r.mu.Unlock()
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *EventRing) dropOldest() {
	r.bytes -= r.buf[r.head].size()
	r.buf[r.head] = Event{}
	r.head = (r.head + 1) % len(r.buf)
	r.n--
	r.dropped++
}

// Since returns a copy of the events after seq, oldest first. Since(0) is everything
// the ring still holds.
func (r *EventRing) Since(seq uint64) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for i := range r.n {
		e := r.buf[(r.head+i)%len(r.buf)]
		if e.Seq > seq {
			out = append(out, e)
		}
	}
	return out
}

// Dropped is how many events have been pushed out to keep the caps.
func (r *EventRing) Dropped() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// Notify receives a value after an Append. It holds at most one, so a reader that
// falls behind wakes once and reads everything new with Since.
func (r *EventRing) Notify() <-chan struct{} {
	return r.notify
}

// truncateText cuts s to at most limit bytes on a rune boundary, with a note.
func truncateText(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	const note = "\n… (truncated)"
	cut := max(limit-len(note), 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + note
}
