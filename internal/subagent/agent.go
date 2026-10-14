package subagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// State is where an agent is in its life.
type State int

// An agent is Running or WaitingApproval while a turn is in flight. Idle and Failed
// agents take new turns from Send. A Stopped agent refuses the model's messages; the
// user can still resume it.
const (
	Running State = iota
	WaitingApproval
	Idle
	Failed
	Stopped
)

func (s State) String() string {
	switch s {
	case Running:
		return "running"
	case WaitingApproval:
		return "waiting approval"
	case Idle:
		return "idle"
	case Failed:
		return "failed"
	case Stopped:
		return "stopped"
	}
	return "unknown"
}

// Busy reports whether a turn is in flight.
func (s State) Busy() bool {
	return s == Running || s == WaitingApproval
}

// Agent is one subagent: a conversation the manager runs turns on, one goroutine per
// turn. Between turns it holds its conversation and event ring but no goroutine.
//
// The exported fields are set at spawn and never change. Everything else is read and
// written through methods, under the agent's lock.
type Agent struct {
	ID, Label  string
	Mode       string
	ParentConv string // the conversation that spawned it
	ConvID     string // its own conversation id in history
	Background bool

	// AutoApprove is set when the user answers "always" to this agent's request. It is
	// never inherited from main.
	AutoApprove atomic.Bool
	// InputWait counts the nanoseconds this agent's turns spent waiting for the user.
	InputWait atomic.Int64

	m *Manager

	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelCauseFunc
	gen        uint64
	goroutines int
	state      State
	activity   string
	started    time.Time
	updated    time.Time
	turns      int
	inbox      []message
	// owed is set while the parent model has asked for work it has not been told the
	// outcome of: a spawn, or a message from the model. A turn only the user asked for
	// from the attach view is not reported to the model.
	owed bool
	// delivered is set once the latest report reached the model, as a notice or from
	// wait, so it is never given twice.
	delivered bool
	// kicked is set for a turn that started from the inbox once a slot freed, or that
	// the model steered: nobody follows it, so its end is announced like a background
	// agent's.
	kicked     bool
	turnCancel context.CancelCauseFunc
	turnDone   chan struct{}
	report     string
	err        error
	ring       *EventRing
	conv       any
	// last is how the latest turn ended, kept as it was when TurnDone closed: a stop
	// does not change it. While the agent is held it is the turn main follows, kept
	// (kept) until Release, so a turn the user starts meanwhile does not replace it.
	last Result
	kept bool
	// held counts the follows of main that have yet to take the agent's result (Hold).
	// A held agent is never evicted.
	held int
}

// State is the agent's current state.
func (a *Agent) State() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// SetState moves a turn between Running and WaitingApproval, around an approval
// request. Any other change is ignored: the manager owns the rest.
func (a *Agent) SetState(s State) {
	if !s.Busy() {
		return
	}
	a.mu.Lock()
	changed := a.state.Busy() && a.state != s
	if changed {
		a.state = s
		a.updated = time.Now()
	}
	a.mu.Unlock()
	if changed {
		a.m.nudge()
	}
}

// SetActivity records what the agent is doing now, such as the tool it is running.
func (a *Agent) SetActivity(activity string) {
	a.mu.Lock()
	a.activity = activity
	a.updated = time.Now()
	a.mu.Unlock()
	a.m.nudge()
}

// Report is the reply of the agent's last finished turn.
func (a *Agent) Report() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.report
}

// Err is the error of the agent's last finished turn, if it had one.
func (a *Agent) Err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}

// LastResult is how the agent's latest turn ended, as it was when that turn's TurnDone
// closed. Report and Err may have changed since, by a stop or a turn of the user's.
func (a *Agent) LastResult() Result {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

// recordTurnEndLocked keeps how the turn ending now ended, for LastResult. Callers
// hold a.mu.
func (a *Agent) recordTurnEndLocked() {
	a.recordResultLocked(a.state)
}

// recordResultLocked keeps a turn that ended in state for LastResult, unless the
// result of the turn main follows is kept already. Callers hold a.mu.
func (a *Agent) recordResultLocked(state State) {
	if a.kept {
		return
	}
	a.last = Result{ID: a.ID, Label: a.Label, State: state, Report: a.report, Err: a.err, turn: a.turns}
	if a.ring != nil {
		a.last.EndSeq = a.ring.LastSeq()
	}
	a.kept = a.held > 0
}

// Conv is the conversation package main stored with SetConv, or nil.
func (a *Agent) Conv() any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conv
}

// SetConv stores the agent's conversation. The manager never looks inside it; it only
// drops it when the agent is evicted.
func (a *Agent) SetConv(conv any) {
	a.mu.Lock()
	a.conv = conv
	a.mu.Unlock()
}

// Ring is the agent's recent events, or nil once it has been evicted.
func (a *Agent) Ring() *EventRing {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ring
}

// TurnDone is closed when the current turn ends. Take it again after each wake: a new
// turn gets a new channel.
func (a *Agent) TurnDone() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turnDone
}

// TakeInbox removes and returns the messages sent to the agent while its turn runs.
// The runner calls it between tool rounds to steer the turn. While main follows the
// turn (Hold) the user's messages stay: the turn's report is main's, and they go on in
// a turn of their own once it ends.
func (a *Agent) TakeInbox() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	texts := make([]string, 0, len(a.inbox))
	var left []message
	for _, msg := range a.inbox {
		if msg.fromUser && a.held > 0 {
			left = append(left, msg)
			continue
		}
		texts = append(texts, msg.text)
	}
	// The model's message steers this turn, so its report is owed to the model.
	if hasModelMessage(a.inbox) {
		a.owed = true
	}
	a.inbox = left
	return texts
}

// message is one message in an agent's inbox, and who sent it.
type message struct {
	text     string
	fromUser bool
}

// joinMessages is the prompt a turn takes from inbox messages.
func joinMessages(msgs []message) string {
	texts := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		texts = append(texts, msg.text)
	}
	return strings.Join(texts, "\n\n")
}

// hasModelMessage reports whether msgs hold a message from the parent model.
func hasModelMessage(msgs []message) bool {
	for _, msg := range msgs {
		if !msg.fromUser {
			return true
		}
	}
	return false
}

// echoUserLocked shows the user's messages among msgs in the event ring, as a turn
// takes them for its prompt. Messages steered into a running turn are shown by the
// turn itself. Callers hold a.mu, which also keeps the turn from writing to the ring
// first.
func (a *Agent) echoUserLocked(msgs []message) {
	if a.ring == nil {
		return
	}
	for _, msg := range msgs {
		if msg.fromUser {
			a.ring.Append(Event{Kind: EventUser, Text: escapeControl(msg.text)})
		}
	}
}

// escapeControl writes control characters other than newline and tab as \xNN, so an
// event never carries terminal escapes.
func escapeControl(text string) string {
	var b strings.Builder
	for _, r := range text {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			b.WriteRune(r)
			continue
		}
		fmt.Fprintf(&b, "\\x%02x", r)
	}
	return b.String()
}

// InterruptTurn cancels the turn in flight, if any, with cause. The agent becomes Idle
// and keeps its conversation. A foreground spawn links it to the parent's tool call:
//
//	stop := context.AfterFunc(callCtx, func() { a.InterruptTurn(ErrParentInterrupted) })
//	defer stop()
func (a *Agent) InterruptTurn(cause error) {
	a.mu.Lock()
	cancel := a.turnCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel(cause)
	}
}

// Info is a snapshot of the agent for listings.
func (a *Agent) Info() Info {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.infoLocked()
}

func (a *Agent) infoLocked() Info {
	info := Info{
		ID:         a.ID,
		Label:      a.Label,
		Mode:       a.Mode,
		ParentConv: a.ParentConv,
		ConvID:     a.ConvID,
		Background: a.Background,
		State:      a.state,
		Activity:   a.activity,
		ReportHead: head(a.report, 200),
		Turns:      a.turns,
		Inbox:      len(a.inbox),
		Active:     a.goroutines > 0 || a.state.Busy() || len(a.inbox) > 0,
		Started:    a.started,
		Updated:    a.updated,
	}
	if a.err != nil {
		info.Err = a.err.Error()
	}
	return info
}

// Info describes an agent, live or evicted.
type Info struct {
	ID, Label, Mode    string
	ParentConv, ConvID string
	Background         bool
	State              State
	Activity           string
	ReportHead         string // the first 200 bytes of the last report
	Err                string
	Turns              int
	Inbox              int
	Active             bool // a turn runs, a stopped one is still winding down, or messages wait to start one
	Started, Updated   time.Time
	Evicted            bool // only this summary is left; the transcript is in history
}

// head cuts s to at most n bytes on a rune boundary.
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
