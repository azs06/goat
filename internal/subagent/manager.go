// Package subagent runs goat's subagents: in-process conversations that do one task
// for the main agent, each turn on its own goroutine. It knows nothing about models,
// tools or the terminal; package main injects the turn itself as a Runner.
//
// Goroutines and leaks. The only goroutines this package starts are turn goroutines,
// one per running turn, each joined by the manager's WaitGroup, and the short-lived
// helper StopAll uses to wait on that group with a bound. An idle agent has no
// goroutine. Every channel a goroutine sends on is either buffered for the one value
// it carries (approval replies) or a one-slot nudge sent without blocking (Changed,
// EventRing.Notify, ApprovalBroker.Pending), so no send can block a turn forever.
// Waiters never subscribe: turn ends are broadcast by closing a channel and replacing
// it, so a waiter that leaves has nothing to clean up. A subagent's approval request
// waits on its own context too and withdraws itself when that ends. StopAll cancels
// every turn and answers every queued request, so after it returns nothing here
// outlives the runner calls themselves.
//
// Locks are taken manager first, then agent, never the other way round.
package subagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Errors the manager returns, and the causes it cancels turns with.
var (
	ErrLimit             = errors.New("subagent limit reached")
	ErrClosed            = errors.New("subagents are shut down")
	ErrUnknownAgent      = errors.New("no such agent")
	ErrInboxFull         = errors.New("the agent's inbox is full")
	ErrStopped           = errors.New("agent stopped")
	ErrBusy              = errors.New("the agent is running")
	ErrApprovalPending   = errors.New("a subagent is waiting for approval")
	ErrWoken             = errors.New("the wait was woken")
	ErrShutdown          = errors.New("goat is exiting")
	ErrEvicted           = errors.New("agent dismissed")
	ErrParentInterrupted = errors.New("the parent agent was interrupted")
	ErrUserInterrupt     = errors.New("interrupted by the user")
)

// Runner runs one turn of a: it sends prompt to the agent's conversation, keeps the
// conversation with a.SetConv, and returns the final reply. It must return soon after
// ctx ends. It takes the agent as an argument rather than closing over one, so a
// finished conversation is never kept alive by the closure.
type Runner func(ctx context.Context, a *Agent, prompt string) (reply string, err error)

// Config holds the limits. A zero field takes its default.
type Config struct {
	MaxRunning  int           // turns running at once
	MaxLive     int           // agents kept before the oldest finished ones are evicted
	MaxEvicted  int           // summaries kept of evicted agents
	InboxCap    int           // messages waiting for one agent
	ReportCap   int           // bytes of a report kept and passed to the parent
	RingEvents  int           // events kept per agent
	RingBytes   int           // bytes of events kept per agent
	EventText   int           // bytes of each text field of one event
	StopTimeout time.Duration // how long exit waits for turns to end
}

var defaults = Config{
	MaxRunning:  3,
	MaxLive:     8,
	MaxEvicted:  20,
	InboxCap:    16,
	ReportCap:   50 << 10,
	RingEvents:  500,
	RingBytes:   256 << 10,
	EventText:   8 << 10,
	StopTimeout: 3 * time.Second,
}

// DefaultConfig is the default limits, with MaxRunning taken from
// GOAT_MAX_RUNNING_AGENTS when that is a positive number.
func DefaultConfig() Config {
	cfg := defaults
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("GOAT_MAX_RUNNING_AGENTS"))); err == nil && n > 0 {
		cfg.MaxRunning = n
	}
	return cfg
}

func (c Config) withDefaults() Config {
	fill := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	fill(&c.MaxRunning, defaults.MaxRunning)
	fill(&c.MaxLive, defaults.MaxLive)
	fill(&c.MaxEvicted, defaults.MaxEvicted)
	fill(&c.InboxCap, defaults.InboxCap)
	fill(&c.ReportCap, defaults.ReportCap)
	fill(&c.RingEvents, defaults.RingEvents)
	fill(&c.RingBytes, defaults.RingBytes)
	fill(&c.EventText, defaults.EventText)
	if c.StopTimeout <= 0 {
		c.StopTimeout = defaults.StopTimeout
	}
	return c
}

// maxNotices is the least bound on the finished-agent notices nobody has taken yet;
// noticeCap raises it with the limits.
const maxNotices = 32

// noticeCap bounds the notices nobody has taken yet. Main takes them after every tool
// round and between turns, and in between an agent adds a few (a turn ending, one a
// message that raced its end continues, and one its queued message starts when a slot
// frees), while at most MaxLive agents plus
// the MaxRunning just spawned are live. The cap stays above that, so a report is never
// dropped, which would also leave its agent free to be evicted unseen; it only guards
// against a bug piling notices up.
func (c Config) noticeCap() int {
	return max(maxNotices, 4*(c.MaxLive+c.MaxRunning))
}

// Notice is a finished background agent's report, waiting to be given to the
// conversation that spawned it. ForModel is false for a turn only the user asked for,
// from the attach view: that one is shown to the user and never given to the model.
type Notice struct {
	AgentID, ParentConv, Text string
	ForModel                  bool
	// turn is the agent's turn count when the report was made, so delivering the
	// agent's latest report drops its notice and leaves an earlier one still owed.
	turn int
}

// Manager owns every subagent and the goroutines their turns run on.
type Manager struct {
	cfg Config
	run Runner

	// Broker queues the agents' approval requests for main to serve.
	Broker *ApprovalBroker

	root context.Context
	stop context.CancelCauseFunc
	sem  chan struct{}
	wg   sync.WaitGroup

	changed chan struct{}

	mu        sync.Mutex
	agents    map[string]*Agent
	order     []string
	next      int
	closed    bool
	notices   []Notice
	evicted   []Info
	turnEnded chan struct{}
	// attached is the agent the user is looking at in the attach view, if any.
	attached string
}

// NewManager returns a manager that runs turns with run, within cfg's limits.
func NewManager(cfg Config, run Runner) *Manager {
	cfg = cfg.withDefaults()
	root, stop := context.WithCancelCause(context.Background())
	return &Manager{
		cfg:       cfg,
		run:       run,
		Broker:    NewApprovalBroker(),
		root:      root,
		stop:      stop,
		sem:       make(chan struct{}, cfg.MaxRunning),
		changed:   make(chan struct{}, 1),
		agents:    map[string]*Agent{},
		turnEnded: make(chan struct{}),
	}
}

// Config is the limits the manager runs with.
func (m *Manager) Config() Config {
	return m.cfg
}

// Changed receives a value when any agent changes state or activity, or a notice
// arrives. It holds at most one, for redrawing a footer.
func (m *Manager) Changed() <-chan struct{} {
	return m.changed
}

func (m *Manager) nudge() {
	select {
	case m.changed <- struct{}{}:
	default:
	}
}

// TurnEnded is closed when any agent's turn ends or an agent is stopped. Take it again
// after each wake, before looking at the agents, so no end is missed.
func (m *Manager) TurnEnded() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.turnEnded
}

// broadcastTurnEnd wakes every Wait. Callers hold m.mu.
func (m *Manager) broadcastTurnEnd() {
	close(m.turnEnded)
	m.turnEnded = make(chan struct{})
}

// SpawnOptions describes a new agent and its first turn.
type SpawnOptions struct {
	Label      string
	Mode       string
	ParentConv string
	ConvID     string
	Background bool
	Prompt     string
}

// Spawn creates an agent and starts its first turn. It fails fast with ErrLimit when
// MaxRunning turns are already running, and never queues: a parent waiting on a slot
// its own children hold would deadlock. A foreground caller links its tool call to
// the turn with InterruptTurn.
func (m *Manager) Spawn(opts SpawnOptions) (*Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	m.evictLocked(m.cfg.MaxLive - 1)
	now := time.Now()
	ctx, cancel := context.WithCancelCause(m.root)
	a := &Agent{
		ID:         fmt.Sprintf("a%d", m.next+1),
		Label:      opts.Label,
		Mode:       opts.Mode,
		ParentConv: opts.ParentConv,
		ConvID:     opts.ConvID,
		Background: opts.Background,
		m:          m,
		ctx:        ctx,
		cancel:     cancel,
		state:      Idle,
		owed:       true,
		started:    now,
		updated:    now,
		ring:       NewEventRing(m.cfg.RingEvents, m.cfg.RingBytes, m.cfg.EventText),
	}
	a.mu.Lock()
	err := m.startTurnLocked(a, opts.Prompt)
	a.mu.Unlock()
	if err != nil {
		cancel(err)
		return nil, err
	}
	m.next++
	m.agents[a.ID] = a
	m.order = append(m.order, a.ID)
	m.nudge()
	return a, nil
}

// startTurnLocked starts a turn of a on a new goroutine. Callers hold m.mu and a.mu.
func (m *Manager) startTurnLocked(a *Agent, prompt string) error {
	if m.closed {
		return ErrClosed
	}
	if a.goroutines > 0 {
		// A stopped turn whose runner has not returned yet: two runners must never
		// share one conversation. kick starts the agent once it has gone.
		return ErrBusy
	}
	select {
	case m.sem <- struct{}{}:
	default:
		return ErrLimit
	}
	a.gen++
	gen := a.gen
	a.delivered = false
	a.kicked = false
	a.state = Running
	a.activity = ""
	a.updated = time.Now()
	a.turnDone = make(chan struct{})
	turnCtx, cancel := context.WithCancelCause(a.ctx)
	a.turnCancel = cancel
	a.goroutines++
	m.wg.Add(1)
	go m.runTurns(a, gen, turnCtx, cancel, prompt)
	return nil
}

// testHookTurnReturned, when set by a test, runs on a turn goroutine once its last turn
// has ended, before the goroutine winds down.
var testHookTurnReturned func()

// runTurns is a turn goroutine. It keeps going while messages arrive in the inbox
// just as a turn ends, so a send that races the end is never lost.
func (m *Manager) runTurns(a *Agent, gen uint64, ctx context.Context, cancel context.CancelCauseFunc, prompt string) {
	defer m.wg.Done()
	// The turn gives its slot back when it ends (finishTurn), before it is announced, so
	// a spawn that follows the announcement finds the slot free. A turn the agent moved
	// on from (it was stopped) gives it back here instead, as its runner returns.
	released := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				_, _, released = m.finishTurn(a, gen, ctx, "", fmt.Errorf("subagent panic: %v", r), false)
			}
		}()
		for {
			reply, err := m.run(ctx, a, prompt)
			next, more, freed := m.finishTurn(a, gen, ctx, reply, err, true)
			if !more {
				released = freed
				return
			}
			prompt = next
		}
	}()
	if testHookTurnReturned != nil {
		testHookTurnReturned()
	}
	if !released {
		<-m.sem
	}
	cancel(nil)
	m.mu.Lock()
	a.mu.Lock()
	a.goroutines--
	a.mu.Unlock()
	if !released {
		// The slot of a stopped turn frees only now: whoever waits for one, such as
		// a spawn group's waiting member, looks again.
		m.broadcastTurnEnd()
	}
	m.kickLocked()
	m.mu.Unlock()
}

// finishTurn records the end of turn gen and returns the next prompt when the inbox
// has messages for the same goroutine to carry on with. A turn the agent has moved on
// from (it was stopped) is ignored. released reports whether the turn gave its running
// slot back: a turn that ends does, before TurnDone is closed.
func (m *Manager) finishTurn(a *Agent, gen uint64, ctx context.Context, reply string, err error, mayContinue bool) (next string, more, released bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if gen != a.gen || !a.state.Busy() {
		return "", false, false
	}
	a.report = CapReport(a.ID, reply, m.cfg.ReportCap)
	a.delivered = false
	a.err = err
	a.turns++
	a.updated = time.Now()
	if mayContinue && len(a.inbox) > 0 && ctx.Err() == nil && !m.closed {
		if a.held > 0 {
			// Main follows this turn: it ends here for main, which takes this result,
			// and the messages go on in a turn of their own.
			state := Idle
			if err != nil {
				state = Failed
			}
			a.recordResultLocked(state)
			close(a.turnDone)
			a.turnDone = make(chan struct{})
		} else if a.owed {
			// The report the model is owed is given now: the turn the inbox starts
			// makes a new one, and only a message from the model makes that one owed.
			m.addNoticeLocked(a, true)
		}
		a.owed = hasModelMessage(a.inbox)
		next := joinMessages(a.inbox)
		a.echoUserLocked(a.inbox)
		a.inbox = nil
		a.state = Running
		a.activity = ""
		m.nudge()
		return next, true, false
	}
	switch {
	case a.ctx.Err() != nil:
		a.state = Stopped
	case ctx.Err() != nil:
		a.state = Idle
		if errors.Is(a.err, context.Canceled) {
			a.err = context.Cause(ctx)
		}
	case err != nil:
		a.state = Failed
	default:
		a.state = Idle
	}
	a.activity = ""
	a.turnCancel = nil
	// The slot is free before anyone hears the turn ended. The goroutine still counts in
	// a.goroutines until it returns, so the agent's own next turn waits for it (ErrBusy)
	// and is kicked then.
	<-m.sem
	a.recordTurnEndLocked()
	close(a.turnDone)
	// A background turn is announced; so is a turn the model is owed that started from
	// the inbox once a slot freed, since nobody follows it the way a foreground spawn or
	// send is followed.
	if a.state != Stopped && (a.Background && (a.owed || a.ID != m.attached) || a.kicked && a.owed) {
		m.addNoticeLocked(a, a.owed)
	}
	// A message from the model still waiting starts a turn it is owed the report of.
	a.owed = hasModelMessage(a.inbox)
	m.broadcastTurnEnd()
	m.nudge()
	return "", false, true
}

func (m *Manager) addNoticeLocked(a *Agent, forModel bool) {
	text := a.report
	if a.err != nil {
		text = fmt.Sprintf("The agent's turn ended with an error: %v\n\n%s", a.err, a.report)
	}
	m.notices = append(m.notices, Notice{
		AgentID:    a.ID,
		ParentConv: a.ParentConv,
		Text:       WrapReport(a.ID, a.Label, text, 0, true),
		ForModel:   forModel,
		turn:       a.turns,
	})
	if limit := m.cfg.noticeCap(); len(m.notices) > limit {
		m.notices = append([]Notice(nil), m.notices[len(m.notices)-limit:]...)
	}
}

// kickLocked starts the oldest agents that are waiting with messages in their inbox,
// while there are free slots. Callers hold m.mu.
func (m *Manager) kickLocked() {
	for _, id := range m.order {
		a := m.agents[id]
		a.mu.Lock()
		var err error
		if !a.state.Busy() && a.state != Stopped && len(a.inbox) > 0 {
			inbox := a.inbox
			a.inbox = nil
			if err = m.startTurnLocked(a, joinMessages(inbox)); err != nil {
				a.inbox = inbox
			} else {
				a.kicked = true
				a.echoUserLocked(inbox)
				m.nudge()
			}
		}
		a.mu.Unlock()
		if errors.Is(err, ErrLimit) || errors.Is(err, ErrClosed) {
			return
		}
	}
}

// Get returns a live agent. An unknown id's error lists the live agents.
func (m *Manager) Get(id string) (*Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getLocked(id)
}

func (m *Manager) getLocked(id string) (*Agent, error) {
	if a, ok := m.agents[id]; ok {
		return a, nil
	}
	if len(m.order) == 0 {
		return nil, fmt.Errorf("%w %q; there are no agents", ErrUnknownAgent, id)
	}
	live := make([]string, 0, len(m.order))
	for _, other := range m.order {
		live = append(live, fmt.Sprintf("%s (%s)", other, m.agents[other].Label))
	}
	return nil, fmt.Errorf("%w %q; the agents are: %s", ErrUnknownAgent, id, strings.Join(live, ", "))
}

// List describes every live agent in spawn order, then the evicted ones still
// remembered.
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	infos := make([]Info, 0, len(m.order)+len(m.evicted))
	for _, id := range m.order {
		infos = append(infos, m.agents[id].Info())
	}
	return append(infos, m.evicted...)
}

// Counts is how many agents are running, how many of those wait for approval, and
// how many are live.
func (m *Manager) Counts() (running, waitingApproval, total int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.agents {
		switch a.State() {
		case Running:
			running++
		case WaitingApproval:
			running++
			waitingApproval++
		}
	}
	return running, waitingApproval, len(m.agents)
}

// Active is how many agents are running a turn, have a runner that has not returned
// yet (a stopped one winding down), or hold messages that start a turn once a slot
// frees. Any of them may still start or run a turn.
func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, a := range m.agents {
		a.mu.Lock()
		if a.goroutines > 0 || a.state.Busy() || len(a.inbox) > 0 {
			n++
		}
		a.mu.Unlock()
	}
	return n
}

// Delivery is what became of a message Send accepted.
type Delivery int

const (
	// Started: the message began a new turn.
	Started Delivery = iota
	// Steered: the agent is running and reads the message at its next tool round.
	Steered
	// Queued: no slot is free; the message starts a turn once one frees.
	Queued
	// AfterTask: the agent works on the task main is waiting on; the user's message
	// starts a turn of its own once that ends.
	AfterTask
)

// Send gives an agent a message. A running agent finds it in its inbox at its next
// tool round. An idle or failed agent starts a new turn with it, or, when no slot is
// free, keeps it in the inbox and starts when one frees. A stopped agent refuses the
// model (fromUser false) with ErrStopped; the user resumes it. started reports
// whether a new turn began.
func (m *Manager) Send(id, text string, fromUser bool) (started bool, err error) {
	d, err := m.Deliver(id, text, fromUser)
	return err == nil && d == Started, err
}

// Deliver is Send, saying what became of the message. A message from the user that a
// turn takes as its prompt is shown in the agent's events as that turn starts, so the
// attach view shows each message once whatever the agent was doing. A message from
// the model makes the agent's next report owed to the model.
func (m *Manager) Deliver(id, text string, fromUser bool) (Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, ErrClosed
	}
	a, err := m.getLocked(id)
	if err != nil {
		return 0, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state == Stopped && !fromUser {
		return 0, fmt.Errorf("%s was stopped; only the user can resume it: %w", id, ErrStopped)
	}
	if len(a.inbox) >= m.cfg.InboxCap {
		return 0, fmt.Errorf("%s has %d messages waiting: %w", id, len(a.inbox), ErrInboxFull)
	}
	a.inbox = append(a.inbox, message{text: text, fromUser: fromUser})
	if !fromUser {
		// A running turn is owed to the model once it takes the message (TakeInbox);
		// one it leaves in the inbox starts the next turn, which is owed instead.
		if !a.state.Busy() {
			a.owed = true
		}
		// The user's "always" covered the work they saw this agent asked to do, not
		// whatever the model sends it next.
		a.AutoApprove.Store(false)
	}
	a.updated = time.Now()
	defer m.nudge()
	if a.state.Busy() {
		if !fromUser {
			// Main is single-threaded, so a turn the model steers is one nothing
			// follows (a user's turn from the attach view, or one a follow gave up
			// on): its report is announced like a kicked turn's.
			a.kicked = true
		}
		if fromUser && a.held > 0 {
			return AfterTask, nil
		}
		return Steered, nil
	}
	if a.state == Stopped {
		a.ctx, a.cancel = context.WithCancelCause(m.root)
		a.state = Idle
	}
	inbox := a.inbox
	a.inbox = nil
	switch err := m.startTurnLocked(a, joinMessages(inbox)); {
	case err == nil:
		a.echoUserLocked(inbox)
		return Started, nil
	case errors.Is(err, ErrLimit) || errors.Is(err, ErrBusy):
		a.inbox = inbox
		return Queued, nil
	default:
		a.inbox = inbox
		return 0, err
	}
}

// Attached is the agent SetAttached last recorded, "" for none.
func (m *Manager) Attached() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attached
}

// SetAttached records the agent the user is looking at in the attach view, "" for
// none. A turn of that agent the user started is not announced when it ends: the user
// saw it end.
func (m *Manager) SetAttached(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attached = id
}

// Hold marks a as followed by main until Release: it is not evicted, and the result
// of the turn main follows is kept for LastResult, whatever turn the user starts
// meanwhile.
func (m *Manager) Hold(a *Agent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a.mu.Lock()
	if a.held == 0 {
		// The turn main follows is the running one, or the one that just ended.
		a.kept = !a.state.Busy()
	}
	a.held++
	a.mu.Unlock()
}

// Release ends a Hold.
func (m *Manager) Release(a *Agent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a.mu.Lock()
	if a.held > 0 {
		a.held--
	}
	if a.held == 0 && a.kept {
		// Turns the user ran meanwhile were not recorded; the latest one is now.
		a.kept = false
		if !a.state.Busy() {
			a.recordTurnEndLocked()
		}
	}
	a.mu.Unlock()
}

// MarkDelivered records that the agent's latest report reached the model, so a wait
// on the agent says so instead of giving it again, and its notice is not given either.
func (m *Manager) MarkDelivered(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	if !ok {
		return
	}
	a.mu.Lock()
	// A turn the user started since the turn main followed ended is not main's.
	current := !a.state.Busy() && (!a.kept || a.last.turn == a.turns)
	a.mu.Unlock()
	if current {
		m.consumeNoticesLocked([]Result{{ID: id}})
	}
}

// MarkNoticeDelivered records that n reached the model. Only the report n carries
// counts: when the agent has finished a newer turn since n was taken, that report and
// its notice are still owed.
func (m *Manager) MarkNoticeDelivered(n Notice) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[n.AgentID]
	if !ok {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.turns == n.turn {
		a.delivered = true
	}
}

// ForgetDelivered clears every agent's delivered mark, for a conversation compacted
// into a summary that may no longer hold the reports: a wait gives them again.
func (m *Manager) ForgetDelivered() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.agents {
		a.mu.Lock()
		a.delivered = false
		a.mu.Unlock()
	}
}

// Stop ends an agent: its turn is cancelled with ErrStopped and it is Stopped at
// once, without waiting for the runner to return; whatever that turn still reports
// is ignored. Its inbox is dropped.
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.getLocked(id)
	if err != nil {
		return err
	}
	a.mu.Lock()
	m.stopLocked(a, ErrStopped)
	a.mu.Unlock()
	m.broadcastTurnEnd()
	m.nudge()
	return nil
}

// stopLocked marks a stopped and cancels its context. Callers hold m.mu and a.mu.
func (m *Manager) stopLocked(a *Agent, cause error) {
	a.cancel(cause)
	a.inbox = nil
	// Whatever the model asked for ended here; a turn the user resumes is the user's.
	a.owed = false
	if a.state == Stopped {
		return
	}
	if a.state.Busy() {
		a.gen++
		a.turnCancel = nil
		a.err = cause
		a.state = Stopped
		a.recordTurnEndLocked()
		close(a.turnDone)
	}
	a.state = Stopped
	a.activity = ""
	a.updated = time.Now()
}

// StopAll shuts the manager down for exit: no new turns, every agent stopped, every
// approval request answered, then up to bound for the runners to return. It returns
// the ids of agents whose runner was still going when the bound passed.
func (m *Manager) StopAll(bound time.Duration) []string {
	m.mu.Lock()
	m.closed = true
	for _, id := range m.order {
		a := m.agents[id]
		a.mu.Lock()
		m.stopLocked(a, ErrShutdown)
		a.mu.Unlock()
	}
	m.stop(ErrShutdown)
	m.broadcastTurnEnd()
	m.mu.Unlock()
	m.nudge()
	m.Broker.DrainAll(ErrShutdown)

	waited := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(waited)
	}()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-waited:
		return nil
	case <-timer.C:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var stragglers []string
	for _, id := range m.order {
		a := m.agents[id]
		a.mu.Lock()
		if a.goroutines > 0 {
			stragglers = append(stragglers, id)
		}
		a.mu.Unlock()
	}
	return stragglers
}

// Dismiss evicts a finished agent, keeping only its summary. A running agent, or one
// with messages or a report not yet delivered, is refused with ErrBusy.
func (m *Manager) Dismiss(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, err := m.getLocked(id)
	if err != nil {
		return err
	}
	a.mu.Lock()
	ok := m.evictableLocked(a)
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s is still in use: %w", id, ErrBusy)
	}
	m.evictAgentLocked(a)
	m.nudge()
	return nil
}

// DismissFinished evicts every finished agent and returns their ids.
func (m *Manager) DismissFinished() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for _, id := range append([]string(nil), m.order...) {
		a := m.agents[id]
		a.mu.Lock()
		ok := m.evictableLocked(a)
		a.mu.Unlock()
		if ok {
			m.evictAgentLocked(a)
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		m.nudge()
	}
	return ids
}

// evictLocked evicts the oldest finished agents until at most keep are live, as far
// as it can. Callers hold m.mu.
func (m *Manager) evictLocked(keep int) {
	for i := 0; len(m.agents) > keep && i < len(m.order); {
		a := m.agents[m.order[i]]
		a.mu.Lock()
		ok := m.evictableLocked(a)
		a.mu.Unlock()
		if ok {
			m.evictAgentLocked(a)
			continue
		}
		i++
	}
}

// evictableLocked reports whether a has finished and holds nothing the parent has yet
// to see: no messages, no notice, and no result a follow of main has yet to take
// (Hold). Callers hold m.mu and a.mu.
func (m *Manager) evictableLocked(a *Agent) bool {
	if a.state.Busy() || a.goroutines > 0 || len(a.inbox) > 0 || a.held > 0 {
		return false
	}
	for _, n := range m.notices {
		if n.AgentID == a.ID {
			return false
		}
	}
	return true
}

// evictAgentLocked drops a, keeping a summary. Everything large is set to nil so a
// stray *Agent pointer keeps nothing alive. Callers hold m.mu.
func (m *Manager) evictAgentLocked(a *Agent) {
	a.mu.Lock()
	info := a.infoLocked()
	a.cancel(ErrEvicted)
	a.conv = nil
	a.ring = nil
	a.report = ""
	a.last.Report = ""
	a.inbox = nil
	a.turnCancel = nil
	a.mu.Unlock()
	info.Evicted = true
	delete(m.agents, a.ID)
	for i, id := range m.order {
		if id == a.ID {
			m.order = append(m.order[:i:i], m.order[i+1:]...)
			break
		}
	}
	m.evicted = append(m.evicted, info)
	if len(m.evicted) > m.cfg.MaxEvicted {
		m.evicted = append([]Info(nil), m.evicted[len(m.evicted)-m.cfg.MaxEvicted:]...)
	}
}

// NoticeCount is how many finished background agents' reports wait to be taken.
func (m *Manager) NoticeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.notices)
}

// TakeNotices removes and returns the notices for parentConv, or every notice when it
// is empty.
func (m *Manager) TakeNotices(parentConv string) []Notice {
	m.mu.Lock()
	defer m.mu.Unlock()
	var taken, kept []Notice
	for _, n := range m.notices {
		if parentConv == "" || n.ParentConv == parentConv {
			taken = append(taken, n)
		} else {
			kept = append(kept, n)
		}
	}
	m.notices = kept
	return taken
}

// Result is how an agent's turn ended, as Wait returns it. Delivered is set when the
// report already reached the model before this wait.
type Result struct {
	ID, Label string
	State     State
	Report    string
	Err       error
	Delivered bool
	// EndSeq is the agent's last event of the turn LastResult describes; the events
	// after it belong to later turns.
	EndSeq uint64
	// turn is the number of that turn (Agent.turns).
	turn int
}

// WaitResult is what Wait found: the agents that finished, the ones still running,
// and whether the timeout passed first.
type WaitResult struct {
	Done     []Result
	Running  []string
	TimedOut bool
}

// Wait waits for the agents ids (when empty: the running ones and those with an
// undelivered report) to finish their turns: any one of them, or all of them when
// all is set. It returns when that holds, when timeout passes (TimedOut), when ctx
// ends (with its cause), or with ErrApprovalPending as soon as an agent waits for
// approval. On ErrApprovalPending the caller serves the broker and calls Wait again
// with the time left. The reports it returns are taken from the pending notices, so
// none is delivered twice, except on ErrApprovalPending and ctx's end: the caller
// may never pass those on, so they stay pending until the caller hands them to the
// model and calls MarkDelivered.
func (m *Manager) Wait(ctx context.Context, ids []string, all bool, timeout time.Duration) (WaitResult, error) {
	return m.WaitOrWake(ctx, ids, all, timeout, nil)
}

// WaitOrWake is Wait that also returns, with ErrWoken, when wake receives a value,
// such as for the caller to run something on its own goroutine. Like
// ErrApprovalPending, ErrWoken leaves the reports found so far pending; the caller
// calls it again with the time left.
func (m *Manager) WaitOrWake(ctx context.Context, ids []string, all bool, timeout time.Duration, wake <-chan struct{}) (WaitResult, error) {
	m.mu.Lock()
	targets, err := m.waitTargetsLocked(ids)
	m.mu.Unlock()
	if err != nil || len(targets) == 0 {
		return WaitResult{}, err
	}
	timer := time.NewTimer(max(timeout, 0))
	defer timer.Stop()
	for {
		m.mu.Lock()
		res := m.collectLocked(targets)
		ended := m.turnEnded
		if len(res.Running) == 0 || (!all && len(res.Done) > 0) {
			m.consumeNoticesLocked(res.Done)
			m.mu.Unlock()
			return res, nil
		}
		m.mu.Unlock()
		if m.Broker.Len() > 0 {
			return m.partial(targets, false), ErrApprovalPending
		}
		select {
		case <-ended:
		case <-m.Broker.Pending():
		case <-wake:
			return m.partial(targets, false), ErrWoken
		case <-ctx.Done():
			return m.partial(targets, false), context.Cause(ctx)
		case <-timer.C:
			res := m.partial(targets, true)
			res.TimedOut = true
			return res, nil
		}
	}
}

// partial is what Wait returns when it stops early: the finished agents' reports,
// taken as delivered when consume is set.
func (m *Manager) partial(targets []string, consume bool) WaitResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := m.collectLocked(targets)
	if consume {
		m.consumeNoticesLocked(res.Done)
	}
	return res
}

func (m *Manager) waitTargetsLocked(ids []string) ([]string, error) {
	if len(ids) > 0 {
		for _, id := range ids {
			if _, err := m.getLocked(id); err != nil {
				return nil, err
			}
		}
		return ids, nil
	}
	var targets []string
	for _, id := range m.order {
		a := m.agents[id]
		a.mu.Lock()
		running := a.runningLocked()
		a.mu.Unlock()
		if running || m.hasModelNoticeLocked(id) {
			targets = append(targets, id)
		}
	}
	return targets, nil
}

func (m *Manager) hasModelNoticeLocked(id string) bool {
	for _, n := range m.notices {
		if n.AgentID == id && n.ForModel {
			return true
		}
	}
	return false
}

func (m *Manager) collectLocked(targets []string) WaitResult {
	var res WaitResult
	for _, id := range targets {
		a, ok := m.agents[id]
		if !ok {
			res.Done = append(res.Done, Result{ID: id, State: Stopped, Err: ErrEvicted})
			continue
		}
		a.mu.Lock()
		if a.runningLocked() {
			res.Running = append(res.Running, id)
		} else {
			res.Done = append(res.Done, Result{ID: id, Label: a.Label, State: a.state, Report: a.report, Err: a.err, Delivered: a.delivered})
		}
		a.mu.Unlock()
	}
	return res
}

// consumeNoticesLocked takes the reports in done as delivered: their notices are
// dropped and a later wait says they were given already. Only the notice of an agent's
// latest report goes; one of an earlier turn, which the model has not seen, stays.
func (m *Manager) consumeNoticesLocked(done []Result) {
	if len(done) == 0 {
		return
	}
	// latest is the turn of each live agent's report; -1 for a dismissed agent, all of
	// whose notices go.
	latest := make(map[string]int, len(done))
	for _, r := range done {
		latest[r.ID] = -1
		if a, ok := m.agents[r.ID]; ok {
			a.mu.Lock()
			a.delivered = true
			latest[r.ID] = a.turns
			a.mu.Unlock()
		}
	}
	kept := m.notices[:0:0]
	for _, n := range m.notices {
		turn, ok := latest[n.AgentID]
		if !ok || turn >= 0 && n.turn != turn {
			kept = append(kept, n)
		}
	}
	m.notices = kept
}

// runningLocked reports whether a has a turn in flight, or messages waiting for a slot
// that start one. Callers hold a.mu.
func (a *Agent) runningLocked() bool {
	return a.state.Busy() || a.state != Stopped && len(a.inbox) > 0
}
