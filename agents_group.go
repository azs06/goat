package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/subagent"
	"github.com/azs06/goat/internal/tools"
)

// This file runs the foreground spawn_agent calls of one response side by side. The
// turn loop runs tool calls one after another, and a foreground spawn waits for its
// agent, so when the loop reaches the first of several spawn calls in a row,
// PrestartSpawns starts every agent of the group. Calls before the group have run by
// then, and a call after it runs once the group's calls have, so the order of calls
// still holds for everything outside the group. Each
// call then follows its own agent while main shows what all of them do, answers their
// approvals and starts the ones the running limit held back as slots free. Only main
// writes to the terminal and asks the user; the agents' turns run on their own
// goroutines as ever.

// spawnGroup is the foreground spawn_agent calls of one response.
type spawnGroup struct {
	members []*groupMember

	// mu guards each member's agent and interrupted, which the parent's interrupt reads
	// from another goroutine. Everything else is main's alone.
	mu          sync.Mutex
	interrupted bool
}

// groupMember is one call of a group and the agent it runs.
type groupMember struct {
	group      *spawnGroup
	req        tools.SpawnRequest
	label      string
	settings   childSettings
	parentConv string

	// a is nil while the call waits for a running slot. Guarded by group.mu.
	a *subagent.Agent

	// Main's alone: the turn's end, the ring and how much of it main has shown, and
	// once the turn has ended, its result. startErr is set when the agent could not
	// be started.
	turnDone <-chan struct{}
	ring     *subagent.EventRing
	cursor   uint64
	done     bool
	result   subagent.Result
	startErr error
	// held is set while the manager holds the agent for main (Hold), from its start
	// until its call has taken the result, or the group ends.
	held bool
}

// release ends the member's hold, if it has one.
func (h *agentHost) release(m *groupMember) {
	if m.held {
		m.held = false
		h.m.Release(m.group.agent(m))
	}
}

func (g *spawnGroup) agent(m *groupMember) *subagent.Agent {
	g.mu.Lock()
	defer g.mu.Unlock()
	return m.a
}

// interrupt stops starting members and interrupts the turns of those started; they end
// Idle with ErrParentInterrupted, as a foreground spawn does on the parent's Ctrl-C.
func (g *spawnGroup) interrupt() {
	g.mu.Lock()
	g.interrupted = true
	var started []*subagent.Agent
	for _, m := range g.members {
		if m.a != nil {
			started = append(started, m.a)
		}
	}
	g.mu.Unlock()
	for _, a := range started {
		a.InterruptTurn(subagent.ErrParentInterrupted)
	}
}

// PrestartSpawns starts the agents of the spawn_agent calls in a row that run in the
// foreground, from the call about to run up to the first background one, when there
// are at least two, as many as the running limit allows; the rest start as group
// members finish. A background call ends the group, so it still starts after the calls
// before it and before those after it. When the first call already belongs to a group,
// started by an earlier call, nothing starts. The returned func ends the group once the
// calls have run: a member whose call never ran, because the parent was interrupted,
// has its turn interrupted too.
func (h *agentHost) PrestartSpawns(ctx context.Context, reqs []tools.SpawnRequest) func() {
	g := &spawnGroup{}
	h.mu.Lock()
	settings := h.settings
	_, grouped := h.groups[reqs[0].CallID]
	h.mu.Unlock()
	if grouped {
		return func() {}
	}
	for _, req := range reqs {
		if req.Background && h.backgroundAllowed() {
			break
		}
		if req.CallID == "" {
			continue
		}
		g.members = append(g.members, &groupMember{group: g, req: req, label: spawnLabel(req), settings: settings, parentConv: activeConversationID})
	}
	if len(g.members) < 2 {
		return func() {}
	}
	h.mu.Lock()
	if h.groups == nil {
		h.groups = map[string]*groupMember{}
	}
	for _, m := range g.members {
		h.groups[m.req.CallID] = m
	}
	h.mu.Unlock()
	stop := context.AfterFunc(ctx, g.interrupt)
	h.startQueued(ctx, g)
	return func() {
		stop()
		for _, m := range g.members {
			h.release(m)
		}
		h.mu.Lock()
		var left []*groupMember
		for _, m := range g.members {
			if h.groups[m.req.CallID] == m {
				delete(h.groups, m.req.CallID)
				left = append(left, m)
			}
		}
		h.mu.Unlock()
		if len(left) > 0 {
			g.interrupt()
		}
	}
}

var _ tools.AgentPrestarter = (*agentHost)(nil)

// takeGroupMember returns, and forgets, the group member of the call callID, if any.
func (h *agentHost) takeGroupMember(callID string) *groupMember {
	if callID == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.groups[callID]
	delete(h.groups, callID)
	return m
}

// startQueued starts the group's waiting members in order while slots are free. At the
// limit the rest wait while a member of the group still runs, since its slot frees when
// it ends; when none runs, the slots are held by other agents and the waiting calls get
// the limit error, as a single spawn would.
func (h *agentHost) startQueued(ctx context.Context, g *spawnGroup) {
	for _, m := range g.members {
		if ctx.Err() != nil {
			return
		}
		if m.startErr != nil || g.agent(m) != nil {
			continue
		}
		a, err := h.start(m.req, m.label, m.settings, m.parentConv, false)
		var limit spawnLimitError
		if errors.As(err, &limit) && g.holdsSlots() {
			return
		}
		if err != nil {
			m.startErr = err
			continue
		}
		// Held until showGroup keeps the result: a mid-turn pause may otherwise dismiss
		// the agent or start a turn of the user's that replaces it.
		h.m.Hold(a)
		m.held = true
		m.turnDone, m.ring = a.TurnDone(), a.Ring()
		g.mu.Lock()
		m.a = a
		interrupted := g.interrupted
		g.mu.Unlock()
		if interrupted {
			// The interrupt came between the check above and now.
			a.InterruptTurn(subagent.ErrParentInterrupted)
		}
	}
}

// running reports whether a member's turn may still be running, as far as main has
// seen.
func (g *spawnGroup) running() bool {
	for _, m := range g.members {
		if m.turnDone != nil && !m.done {
			return true
		}
	}
	return false
}

// holdsSlots reports whether a slot the group's waiting members can have is still in
// use by the group: a member's turn runs, or a turn the user started on a member whose
// call already has its result.
func (g *spawnGroup) holdsSlots() bool {
	if g.running() {
		return true
	}
	for _, m := range g.members {
		if a := g.agent(m); a != nil && a.State().Busy() {
			return true
		}
	}
	return false
}

// queued is how many members wait for a slot.
func (g *spawnGroup) queued() int {
	n := 0
	for _, m := range g.members {
		if m.turnDone == nil && m.startErr == nil {
			n++
		}
	}
	return n
}

// followMember is follow for a call of a group: it waits for the call's own agent while
// it shows the progress of every agent of the group, answers their approvals, and starts
// the members waiting for a slot. Events already shown, during an earlier call of the
// group, are not shown again, and a member that already finished returns at once.
func (h *agentHost) followMember(ctx context.Context, target *groupMember) (string, error) {
	g := target.group
	parentDone := ctx.Done()
	interrupted := false
	var giveUp <-chan time.Time
	for {
		// A pause for the commands typed meanwhile, unless the group is winding down.
		if !interrupted && h.serveMidTurn(ctx) {
			for _, m := range g.members {
				if m.ring != nil && !m.done {
					h.catchUp(g.agent(m).ID, m.ring, &m.cursor, true)
				}
			}
		}
		// Taken before looking at the agents, so a turn that ends meanwhile wakes the
		// select below.
		ended := h.m.TurnEnded()
		// Shown first, so startQueued knows which members have ended.
		h.showGroup(g)
		if !interrupted {
			h.startQueued(ctx, g)
		}
		if interrupted {
			if !g.running() {
				break
			}
		} else if target.done || target.startErr != nil || target.turnDone == nil && !g.holdsSlots() {
			break
		}

		cases := []reflect.SelectCase{
			{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ended)},
			{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(h.m.Broker.Pending())},
		}
		const endedCase, approvalCase = 0, 1
		parentCase, giveUpCase, typedCase := -1, -1, -1
		if !interrupted {
			typedCase = len(cases)
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(lineQueued)})
		}
		if parentDone != nil && !interrupted {
			parentCase = len(cases)
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(parentDone)})
		}
		if giveUp != nil {
			giveUpCase = len(cases)
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(giveUp)})
		}
		for _, m := range g.members {
			if m.ring != nil && !m.done {
				cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(m.ring.Notify())})
			}
		}
		switch chosen, _, _ := reflect.Select(cases); chosen {
		case endedCase, typedCase:
		case approvalCase:
			h.serveApprovals(ctx, h.cfg)
		case parentCase:
			// Every turn of the group was interrupted; they end soon. After StopTimeout
			// one that ignores the cancellation must not hold main.
			interrupted = true
			g.interrupt()
			timer := time.NewTimer(h.m.Config().StopTimeout)
			defer timer.Stop()
			giveUp = timer.C
		case giveUpCase:
			for _, m := range g.members {
				if m.turnDone != nil && !m.done {
					id := g.agent(m).ID
					showAgentActivity(formatNotice(id+" is still winding down; it goes on in the background (/agents "+id+").") + "\n")
				}
			}
			return "", ctx.Err()
		default:
			// A ring has new events.
		}
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if target.startErr != nil {
		return "", target.startErr
	}
	if !target.done {
		return "", errors.New("the agent could not be followed")
	}
	h.m.MarkDelivered(target.result.ID)
	h.release(target)
	return h.wrapResult(target.result), nil
}

// showGroup prints the new events of every started member of g, each line labeled with
// its agent's id, keeps the result of each turn that ended, and names what the agents
// are doing on the spinner.
func (h *agentHost) showGroup(g *spawnGroup) {
	var b strings.Builder
	var activities []string
	for _, m := range g.members {
		if m.turnDone == nil || m.done {
			continue
		}
		a := g.agent(m)
		// The turn's end is seen before its last events are read, so none is missed;
		// turnEvents leaves out those of a later turn.
		finished := false
		select {
		case <-m.turnDone:
			finished = true
		default:
		}
		for _, e := range turnEvents(a, m.ring, m.cursor, m.turnDone) { // ring is set with turnDone
			m.cursor = e.Seq
			b.WriteString(renderAgentEventAs(e, a.ID))
		}
		if finished {
			// Kept as the turn ended. The agent stays held until its call takes the
			// result, so it is not dismissed meanwhile, by the user at a pause or to
			// make room for the next spawn.
			m.done = true
			m.result = a.LastResult()
			continue
		}
		activity := a.Info().Activity
		if activity == "" {
			activity = "working"
		}
		activities = append(activities, a.ID+" "+activity)
	}
	if b.Len() > 0 {
		showAgentActivity(b.String())
	}
	relabelSpinner(groupSpinnerLabel(len(g.members), activities, g.queued()))
}

// groupSpinnerLabel is what the spinner says while a group runs: how many agents, what
// each running one does, and how many wait for a slot.
func groupSpinnerLabel(members int, activities []string, queued int) string {
	dot := " " + style.Glyphs().Dot + " "
	label := plural(members, "agent")
	if len(activities) > 0 {
		label += dot + strings.Join(activities, dot)
	}
	if queued > 0 {
		label += dot + fmt.Sprintf("%d waiting", queued)
	}
	return label
}
