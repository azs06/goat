package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/subagent"
)

// This file runs commands typed while main's turn works. Main's turn often waits on
// subagents for minutes, so /agents (with its attach view, log, stop and dismiss),
// Ctrl-G and the read-only commands of midTurnCommands do not wait for the turn to
// end: the key capture queues them like any line and wakes main through lineQueued,
// and main runs them at its next pause: while it follows a foreground agent or a group
// of them, while the wait tool waits, and after each tool round. All of that is on
// main's goroutine, which stays the only one writing to the terminal, asking about
// approvals and reading stdin; while main's model streams, a command waits for the
// next pause, and the footer says so. Main's turn is paused while such a command runs,
// the attach view included; the agents keep running.

// midTurnCommands run at main's pauses during its turn. They change nothing main's
// turn depends on (the conversation, the model and provider, the session, the theme or
// the keys) and read stdin only through the selector, the pager or the attach view,
// which pause the key capture. /plan is read live by main's turn and by general
// agents. Every other command waits for the turn to end.
var midTurnCommands = map[string]bool{"agents": true, "help": true, "ps": true, "tools": true, "output": true, "status": true, "history": true, "diff": true, "plan": true}

// isMidTurnCommand reports whether line is a slash command of midTurnCommands. A bare
// word such as "help" stays text for the model.
func isMidTurnCommand(line string) bool {
	if !strings.HasPrefix(line, "/") {
		return false
	}
	cmd, _, err := lookupCommand(line)
	return err == nil && cmd != nil && midTurnCommands[cmd.name]
}

// midTurnKey marks the context of a command run at main's pause.
type midTurnKey struct{}

// runsMidTurn reports whether ctx is that of a command run during main's turn.
func runsMidTurn(ctx context.Context) bool {
	on, _ := ctx.Value(midTurnKey{}).(bool)
	return on
}

// midTurnRuns counts the commands run at main's pauses, for tests to wait on.
var midTurnRuns atomic.Int64

// serveMidTurn runs the commands queued for main's next pause, one after another,
// and reports whether it ran any. Called on main, at its pauses.
func (h *agentHost) serveMidTurn(ctx context.Context) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	h.shownAway = nil
	h.mu.Unlock()
	ran := false
	for ctx.Err() == nil {
		line, ok := takeMidTurnCommand()
		if !ok {
			break
		}
		ran = true
		h.runMidTurn(ctx, line)
	}
	return ran
}

// runMidTurn runs one command at main's pause, under main's spinner, which comes back
// once it is done. Ctrl-C ends main's turn as ever, except in the attach view, where it
// only returns to the turn.
func (h *agentHost) runMidTurn(ctx context.Context, line string) {
	defer midTurnRuns.Add(1)
	resume := pauseSpinner()
	defer resume()
	fmt.Println("\n" + renderUserMessage(line, terminalWidth()))
	cmd, args, err := lookupCommand(line)
	if err == nil && cmd == nil {
		err = errors.New("not a command")
	}
	if err == nil {
		err = cmd.callback(context.WithValue(ctx, midTurnKey{}, true), h.cfg, args)
	}
	if err != nil && ctx.Err() == nil {
		fmt.Println(formatError(err.Error()))
	}
	// /plan changes what general agents follow; the footer shows the modes.
	h.sync(h.cfg)
	if h.cfg.interactive && style.Enabled() {
		setTurnFooter(turnFooterParts(h.cfg, true))
	}
}

// noteShown records that the attach view showed agent id's events up to seq, so main's
// follow, back from a pause, does not show them again.
func (h *agentHost) noteShown(id string, seq uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shownAway == nil {
		h.shownAway = map[string]uint64{}
	}
	h.shownAway[id] = max(h.shownAway[id], seq)
}

// awaySummaryAfter is how many of an agent's events main shows on coming back from a
// pause; more are summed up in one line.
const awaySummaryAfter = 8

// catchUp moves a follow's cursor for agent id past what happened during main's
// pause: past the events the attach view showed, and past the rest too when there are
// many, with a line saying how many. labeled labels the line with the agent, as a
// group's lines are. The few events left the follow shows as ever.
func (h *agentHost) catchUp(id string, ring *subagent.EventRing, cursor *uint64, labeled bool) {
	h.mu.Lock()
	shown := h.shownAway[id]
	h.mu.Unlock()
	*cursor = max(*cursor, shown)
	if ring == nil {
		return
	}
	events := ring.Since(*cursor)
	if len(events) <= awaySummaryAfter {
		return
	}
	*cursor = events[len(events)-1].Seq
	indent := nestedIndent
	if labeled {
		indent += id + " "
	}
	text := fmt.Sprintf("%s %s while you were away (/agents log %s)", style.Glyphs().Ellipsis, plural(len(events), "event"), id)
	showAgentActivity(indent + style.Subtle.Render(text) + "\n")
}

// interruptRedirect, when set, gets the Ctrl-C the running command or turn would get:
// the attach view opened during main's turn returns to the turn instead of ending it.
var interruptRedirect atomic.Pointer[func()]

// redirectInterrupt sends Ctrl-C to cancel until the returned func is called.
func redirectInterrupt(cancel func()) func() {
	interruptRedirect.Store(&cancel)
	return func() { interruptRedirect.CompareAndSwap(&cancel, nil) }
}

// deliverInterrupt is what one Ctrl-C does: it goes to the redirect, if any, and
// otherwise cancels what runs.
func deliverInterrupt(cancel func()) {
	if redirect := interruptRedirect.Load(); redirect != nil {
		(*redirect)()
		return
	}
	cancel()
}

// notifyInterrupt is signal.NotifyContext for Ctrl-C (os.Interrupt), except that a
// redirect set meanwhile gets each Ctrl-C instead of ctx.
func notifyInterrupt(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		for {
			select {
			case <-signals:
				deliverInterrupt(cancel)
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			signal.Stop(signals)
			close(done)
			<-exited
			cancel()
		})
	}
}
