package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/subagent"
	"github.com/azs06/goat/internal/tools"
)

// This file joins internal/subagent to the turn loop. A subagent's turn is an ordinary
// sendPromptStream on its own goroutine, with options that keep it off the terminal:
// its work goes to the agent's event ring through ringSink, its run_bash approvals go
// to the broker for main to ask about, and it never reads stdin. Only main, on its own
// goroutine, prints what agents do and asks the user about their commands.

// childSettings is what a subagent's turns run with, taken from main when it is spawned.
type childSettings struct {
	model    provider.Model
	effort   provider.Effort
	codeMode bool
	vision   bool
}

// childBox is the conversation package main keeps in a subagent (Agent.SetConv). Each
// save stores a new box, so a reader never sees one half written.
type childBox struct {
	settings childSettings
	conv     conversationState
}

// agentHost runs the subagent tools for the main agent on top of a subagent.Manager.
// Its tool methods run on main, inside a tool call; runChild runs on turn goroutines.
type agentHost struct {
	m   *subagent.Manager
	cfg *config

	// interactive is set when main can ask the user about a subagent's command.
	interactive bool
	// mainPlanMode is main's plan mode, which general subagents follow call by call.
	mainPlanMode atomic.Bool
	// convSeq numbers the subagents' conversation ids.
	convSeq atomic.Uint64

	mu sync.Mutex
	// settings is main's current model and modes, refreshed by sync.
	settings childSettings
	// pending holds the settings of agents whose first turn has not taken them yet,
	// by conversation id.
	pending map[string]childSettings
	// lineage holds the earlier ids of main's current conversation: compaction and a
	// model switch carry it on under a new id, and the agents it spawned before still
	// report to it. /clear and /resume start another one and empty it.
	lineage map[string]bool
	// adopted holds the agents of an earlier conversation that main's current one sent
	// a message to: their reports go to it too. Emptied with lineage.
	adopted map[string]bool
}

// backgroundReportNote tells the model when a background agent's report reaches it:
// nothing wakes it when the agent finishes while its turn is over.
const backgroundReportNote = "Once it finishes, its report comes with your next tool result, or with the user's next message if your turn has ended; do not guess its results. Call wait if you need them before you end your turn."

// runningAgents is the session's host, for stopping its agents on every way out.
var runningAgents atomic.Pointer[agentHost]

// heldWarnings are workspace warnings given while agents may be running: a subagent's
// file change runs on its own goroutine, which must never write to the terminal over
// the prompt or the attach view. Main shows them where it shows agents' notices.
var heldWarnings struct {
	sync.Mutex
	lines []string
}

// workspaceWarnf is workspace.Warnf: a warning, held for main while any agent may still
// be changing files. Main's own warnings in that window wait until its next tool round.
func workspaceWarnf(format string, args ...any) {
	if h := runningAgents.Load(); h != nil && h.m.Active() > 0 {
		heldWarnings.Lock()
		heldWarnings.lines = append(heldWarnings.lines, fmt.Sprintf(format, args...))
		heldWarnings.Unlock()
		return
	}
	warnf(format, args...)
}

// showHeldWarnings shows the warnings held for main. Called on main.
func showHeldWarnings() {
	heldWarnings.Lock()
	lines := heldWarnings.lines
	heldWarnings.lines = nil
	heldWarnings.Unlock()
	for _, line := range lines {
		warnf("%s", line)
	}
}

// newAgentHost returns a host whose agents run within limits; zero limits take the
// defaults.
func newAgentHost(cfg *config, limits subagent.Config, interactive bool) *agentHost {
	h := &agentHost{cfg: cfg, interactive: interactive, pending: map[string]childSettings{}, lineage: map[string]bool{}}
	h.m = subagent.NewManager(limits, h.runChild)
	h.sync(cfg)
	return h
}

// startAgents creates the session's subagent host. Called on main once cfg is set up.
func startAgents(cfg *config, oneShot bool) {
	cfg.agents = newAgentHost(cfg, subagent.DefaultConfig(), cfg.interactive && !oneShot)
	runningAgents.Store(cfg.agents)
}

// sync takes main's current model and modes, for the agents spawned next and for
// general agents' plan mode. Called on main, before each turn and after each command.
func (h *agentHost) sync(cfg *config) {
	if h == nil {
		return
	}
	h.mainPlanMode.Store(cfg.planMode)
	h.mu.Lock()
	h.settings = childSettings{model: cfg.model, effort: cfg.reasoningEffort, codeMode: cfg.codeMode, vision: provider.AcceptsImages(cfg.model)}
	h.mu.Unlock()
}

// backgroundAllowed reports whether agents may run in the background: only on an
// interactive color terminal, where main can show their notices and ask about their
// commands between turns. Elsewhere, such as under -p, nothing may be left behind.
func (h *agentHost) backgroundAllowed() bool {
	return h.interactive && !h.cfg.oneShot && h.cfg.interactive && style.Enabled()
}

// runChild is the subagent.Runner: one turn of a's conversation.
func (h *agentHost) runChild(ctx context.Context, a *subagent.Agent, prompt string) (string, error) {
	var settings childSettings
	var conv conversationState
	box, ok := a.Conv().(*childBox)
	if ok && box != nil {
		settings, conv = box.settings, box.conv
	} else {
		h.mu.Lock()
		settings, ok = h.pending[a.ConvID]
		delete(h.pending, a.ConvID)
		h.mu.Unlock()
		if ok {
			// Kept at once: a first turn that fails before it saves the conversation,
			// such as on a provider that cannot be set up, must not leave every later
			// turn without a model.
			a.SetConv(&childBox{settings: settings})
		}
	}
	if settings.model.IsZero() {
		return "", errors.New("no model is selected for the subagent")
	}
	p, err := h.cfg.provider(settings.model.Provider)
	if err != nil {
		return "", err
	}

	sink := &ringSink{a: a}
	explore := a.Mode != tools.AgentModeGeneral
	planMode := func() bool { return explore || h.mainPlanMode.Load() }
	opts := turnOptions{
		model:          settings.model.ID,
		effort:         settings.effort,
		codeMode:       settings.codeMode,
		vision:         settings.vision,
		planMode:       planMode(),
		livePlanMode:   planMode,
		conversationID: a.ConvID,
		parentID:       a.ParentConv,
		sink:           sink,
		inputWait:      &a.InputWait,
		subagent:       true,
		approveBash:    h.childApprover(a),
		takeSteering: func() string {
			return strings.Join(a.TakeInbox(), "\n\n")
		},
		checkpoint: func(state conversationState) {
			a.SetConv(&childBox{settings: settings, conv: state})
		},
	}
	next, err := sendPromptStream(ctx, p, prompt, conv, opts)
	a.SetConv(&childBox{settings: settings, conv: next})
	return sink.report(), err
}

// childApprover asks about a subagent's run_bash commands. An agent the user answered
// "always" for runs its commands, except ones touching goat's state or keys, and so do
// all agents while the user has auto-approval on (--yes, GOAT_AUTO_APPROVE_BASH or
// /approve on, but not an "always" answered to main). Without a terminal the answer is
// main's approver's: it declines, unless auto-approval is on. Otherwise the request
// waits for main to ask the user.
func (h *agentHost) childApprover(a *subagent.Agent) func(ctx context.Context, command, workdir string) error {
	return func(ctx context.Context, command, workdir string) error {
		guarded := mentionsStateDir(command, workdir) || mentionsConfigDir(command, workdir)
		if !guarded && (a.AutoApprove.Load() || h.sessionAutoApprove()) {
			return nil
		}
		if !h.interactive {
			if h.cfg.approver == nil {
				return fmt.Errorf("%w: approval needs an interactive terminal", errBashCommandDeclined)
			}
			// Not interactive, so it never reads stdin.
			return h.cfg.approver.approve(ctx, command, workdir)
		}
		a.SetState(subagent.WaitingApproval)
		defer a.SetState(subagent.Running)
		return h.m.Broker.Request(ctx, &subagent.ApprovalRequest{AgentID: a.ID, Label: a.Label, Command: command, Workdir: workdir})
	}
}

// sessionAutoApprove reports whether the user has auto-approval on for the session.
func (h *agentHost) sessionAutoApprove() bool {
	return h.cfg.approver != nil && h.cfg.approver.sessionAutoApprove.Load()
}

// forgetAlways drops every agent's "always", for /approve off.
func (h *agentHost) forgetAlways() {
	if h == nil {
		return
	}
	for _, info := range h.m.List() {
		if a, err := h.m.Get(info.ID); err == nil {
			a.AutoApprove.Store(false)
		}
	}
}

// serveApprovals asks the user about the commands subagents are waiting to run, one at
// a time, on main. "always" applies to the asking agent only.
func (h *agentHost) serveApprovals(ctx context.Context, cfg *config) {
	if h == nil {
		return
	}
	for ctx.Err() == nil {
		req := h.m.Broker.Next()
		if req == nil {
			return
		}
		a, err := h.m.Get(req.AgentID)
		if err != nil {
			req.Answer(err)
			continue
		}
		guarded := mentionsStateDir(req.Command, req.Workdir) || mentionsConfigDir(req.Command, req.Workdir)
		if !guarded && a.AutoApprove.Load() {
			// "always" was answered to an earlier request of the same agent.
			req.Answer(nil)
			continue
		}
		if cfg.input == nil {
			req.Answer(fmt.Errorf("%w: approval needs an interactive terminal", errBashCommandDeclined))
			continue
		}
		// The question ends when the agent stops waiting, too.
		askCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(req.Ctx, cancel)
		asker := &bashApprover{input: cfg.input, interactive: true, label: renderApprovalLabel(a.ID, a.Label)}
		err = asker.approve(askCtx, req.Command, req.Workdir)
		stop()
		cancel()
		if err == nil && asker.autoApprove.Load() {
			a.AutoApprove.Store(true)
		}
		if err != nil && ctx.Err() != nil {
			err = fmt.Errorf("%w: the user interrupted the question", errBashCommandDeclined)
		}
		req.Answer(err)
	}
}

// limitText is the spawn_agent error when MaxRunning turns already run.
func (h *agentHost) limitText() string {
	return fmt.Sprintf("Subagent limit reached (%d running). Do not retry; do the work yourself or wait for one to finish.", h.m.Config().MaxRunning)
}

// Spawn starts an agent. In the foreground it shows the agent's work and returns its
// report; in the background it returns at once.
func (h *agentHost) Spawn(ctx context.Context, req tools.SpawnRequest) (string, error) {
	label := req.Description
	if label == "" {
		label = truncateForLog(oneLine(req.Prompt), 40)
	}
	background := req.Background && h.backgroundAllowed()
	convID := fmt.Sprintf("%s-sub%d", newConversationID(), h.convSeq.Add(1))
	h.mu.Lock()
	h.pending[convID] = h.settings
	h.mu.Unlock()

	a, err := h.m.Spawn(subagent.SpawnOptions{Label: label, Mode: req.Mode, ParentConv: activeConversationID, ConvID: convID, Background: background, Prompt: req.Prompt})
	if err != nil {
		h.mu.Lock()
		delete(h.pending, convID)
		h.mu.Unlock()
		if errors.Is(err, subagent.ErrLimit) {
			return "", errors.New(h.limitText())
		}
		return "", err
	}
	if background {
		return fmt.Sprintf("Agent %s (%s) started in the background. %s", a.ID, label, backgroundReportNote), nil
	}
	report, err := h.follow(ctx, a)
	if err == nil && req.Background {
		report = "[background is not available here, so the agent ran in the foreground]\n" + report
	}
	return report, err
}

// follow shows a foreground agent's turn as it runs, answering approvals meanwhile,
// and returns its wrapped report. The parent's Ctrl-C interrupts the turn, which leaves
// the agent idle with its conversation.
func (h *agentHost) follow(ctx context.Context, a *subagent.Agent) (string, error) {
	stop := context.AfterFunc(ctx, func() { a.InterruptTurn(subagent.ErrParentInterrupted) })
	defer stop()
	done, ring := a.TurnDone(), a.Ring()
	var notify <-chan struct{}
	if ring != nil {
		notify = ring.Notify()
	}
	var cursor uint64
	parentDone := ctx.Done()
	// After the parent's interrupt the turn gets StopTimeout to end; a provider that
	// ignores the cancellation must not hold main.
	var giveUp <-chan time.Time
	for finished := false; !finished; {
		h.showEvents(a, ring, &cursor)
		select {
		case <-done:
			finished = true
		case <-notify:
		case <-h.m.Broker.Pending():
			h.serveApprovals(ctx, h.cfg)
		case <-parentDone:
			// The turn was interrupted; it ends soon.
			parentDone = nil
			timer := time.NewTimer(h.m.Config().StopTimeout)
			defer timer.Stop()
			giveUp = timer.C
		case <-giveUp:
			showAgentActivity(formatNotice(a.ID+" is still winding down; it goes on in the background (/agents "+a.ID+").") + "\n")
			finished = true
		}
	}
	stop()
	h.showEvents(a, ring, &cursor)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	h.m.MarkDelivered(a.ID)
	return h.wrapResult(subagent.Result{ID: a.ID, Label: a.Label, State: a.State(), Report: a.Report(), Err: a.Err()}), nil
}

// showEvents prints the agent's events after cursor, indented, and names its activity
// on the spinner.
func (h *agentHost) showEvents(a *subagent.Agent, ring *subagent.EventRing, cursor *uint64) {
	if ring == nil {
		return
	}
	events := ring.Since(*cursor)
	if len(events) > 0 {
		*cursor = events[len(events)-1].Seq
		var b strings.Builder
		for _, e := range events {
			b.WriteString(renderAgentEvent(e))
		}
		showAgentActivity(b.String())
	}
	activity := a.Info().Activity
	if activity == "" {
		activity = "working"
	}
	relabelSpinner(a.ID + " " + style.Glyphs().Dot + " " + activity)
}

// showAgentActivity prints text where tool calls are shown, above the spinner.
func showAgentActivity(text string) {
	if activityOut != os.Stdout {
		fmt.Fprint(activityOut, text)
		return
	}
	printAboveSpinner(text)
}

// renderAgentEvent draws one event of a subagent's turn, indented under the tool call
// that runs it. Event text is already free of control characters.
func renderAgentEvent(e subagent.Event) string {
	width := toolRenderWidth() - len(nestedIndent)
	call := provider.ToolCall{Name: e.Tool, Arguments: e.Args}
	switch e.Kind {
	case subagent.EventText:
		return indentBlock(strings.TrimRight(e.Text, "\n"), nestedIndent) + "\n"
	case subagent.EventToolStart:
		return indentBlock(renderToolHeader(call, width), nestedIndent) + "\n"
	case subagent.EventToolResult:
		var err error
		if e.Err != "" {
			err = errors.New(e.Err)
		}
		first, _, _ := strings.Cut(renderToolResult(call, e.Output, err, e.Elapsed, width), "\n")
		return nestedIndent + first + "\n"
	case subagent.EventUser:
		return indentBlock(style.Glyphs().Prompt+" "+e.Text, nestedIndent) + "\n"
	case subagent.EventNotice:
		return indentBlock(formatNotice(e.Text), nestedIndent) + "\n"
	}
	return ""
}

// wrapResult is how an agent's finished turn reaches the parent model.
func (h *agentHost) wrapResult(r subagent.Result) string {
	text := r.Report
	switch {
	case r.Delivered:
		return fmt.Sprintf("Agent %s (%s) has not run since its report was delivered above; it is not repeated here. send_message continues it.", r.ID, escapeControlChars(oneLine(r.Label)))
	case errors.Is(r.Err, subagent.ErrEvicted):
		text = "The agent was dismissed; its report is no longer available."
	case r.State == subagent.Stopped:
		text = fmt.Sprintf("The agent was stopped (%v).\n\n%s", r.Err, r.Report)
	case r.Err != nil:
		text = fmt.Sprintf("The agent's turn ended with an error: %v\n\n%s", r.Err, r.Report)
	case strings.TrimSpace(text) == "":
		text = "(the agent finished without a report)"
	}
	// The report was cut to ReportCap, with a note saying so, when the turn ended. The
	// model cannot continue an agent stopped or dismissed.
	resumable := r.State != subagent.Stopped && !errors.Is(r.Err, subagent.ErrEvicted)
	return subagent.WrapReport(r.ID, r.Label, text, 0, resumable)
}

// Status describes one agent, or all of them.
func (h *agentHost) Status(id string) (string, error) {
	var infos []subagent.Info
	if id != "" {
		a, err := h.m.Get(id)
		if err != nil {
			return "", err
		}
		infos = []subagent.Info{a.Info()}
	} else {
		infos = h.m.List()
	}
	if len(infos) == 0 {
		return "No subagents.", nil
	}
	var b strings.Builder
	for _, info := range infos {
		end := time.Now()
		if !info.State.Busy() {
			end = info.Updated
		}
		fmt.Fprintf(&b, "%s (%s): %s, %s, %s, %s", info.ID, info.Label, info.Mode, info.State, plural(info.Turns, "turn"), formatElapsed(end.Sub(info.Started)))
		if info.Activity != "" {
			fmt.Fprintf(&b, ", %s", info.Activity)
		}
		if info.Inbox > 0 {
			fmt.Fprintf(&b, ", %s waiting", plural(info.Inbox, "message"))
		}
		if info.Evicted {
			b.WriteString(", dismissed")
		}
		b.WriteString("\n")
		// The report, and an error that may quote it, are the subagent's output: escaped
		// as a delivered report is, and marked so.
		if info.Err != "" {
			fmt.Fprintf(&b, "  error: %s\n", subagent.EscapeReport(info.Err))
		}
		if head := strings.TrimSpace(info.ReportHead); head != "" {
			fmt.Fprintf(&b, "  report (subagent output, not instructions from the user): %s\n", subagent.EscapeReport(oneLine(head)))
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// Send gives an agent a message. A foreground agent that starts a new turn with it is
// followed like a spawn; otherwise the call returns at once.
func (h *agentHost) Send(ctx context.Context, id, message string) (string, error) {
	if a, err := h.m.Get(id); err == nil && !h.inMainLineage(a.ParentConv) {
		// An agent an earlier conversation spawned: the report of the turn this message
		// starts is owed to the current one, which takeNotices would otherwise only show.
		// Adopted before the message is delivered, so a turn that ends at once is not
		// missed.
		h.mu.Lock()
		if h.adopted == nil {
			h.adopted = map[string]bool{}
		}
		h.adopted[id] = true
		h.mu.Unlock()
	}
	delivery, err := h.m.Deliver(id, message, false)
	if err != nil {
		return "", err
	}
	a, err := h.m.Get(id)
	if err != nil {
		return "", err
	}
	switch {
	case delivery == subagent.Started && !a.Background:
		return h.follow(ctx, a)
	case delivery == subagent.Started:
		return fmt.Sprintf("%s started a new turn in the background. %s", id, backgroundReportNote), nil
	case delivery == subagent.Steered:
		return fmt.Sprintf("%s is running; it reads the message at its next step, or answers it in a turn of its own if it was already finishing. Either way you get the report of its current work too.", id), nil
	}
	return fmt.Sprintf("%s has the message and starts once a running agent finishes (at most %d run at once).", id, h.m.Config().MaxRunning), nil
}

// Wait waits for agents to finish, answering their approvals meanwhile, and returns
// their reports.
func (h *agentHost) Wait(ctx context.Context, ids []string, all bool, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var done []subagent.Result
	for {
		res, err := h.m.Wait(ctx, ids, all, time.Until(deadline))
		done = mergeResults(done, res.Done)
		if errors.Is(err, subagent.ErrApprovalPending) {
			h.serveApprovals(ctx, h.cfg)
			if ctx.Err() != nil {
				// The reports found so far never reach the model; they stay pending.
				return "", ctx.Err()
			}
			if !all && len(done) > 0 {
				return h.deliverWait(done, res.Running, false, timeout), nil
			}
			continue
		}
		if err != nil {
			return "", err
		}
		return h.deliverWait(done, res.Running, res.TimedOut, timeout), nil
	}
}

// mergeResults adds results not yet in done.
func mergeResults(done, more []subagent.Result) []subagent.Result {
	for _, r := range more {
		seen := false
		for _, d := range done {
			if d.ID == r.ID {
				seen = true
				break
			}
		}
		if !seen {
			done = append(done, r)
		}
	}
	return done
}

// deliverWait is formatWait for a wait that returns to the model: the reports in done
// are delivered from now on.
func (h *agentHost) deliverWait(done []subagent.Result, running []string, timedOut bool, timeout time.Duration) string {
	for _, r := range done {
		h.m.MarkDelivered(r.ID)
	}
	return h.formatWait(done, running, timedOut, timeout)
}

func (h *agentHost) formatWait(done []subagent.Result, running []string, timedOut bool, timeout time.Duration) string {
	if len(done) == 0 && len(running) == 0 {
		return "No subagents are running and no reports are waiting."
	}
	var parts []string
	for _, r := range done {
		parts = append(parts, h.wrapResult(r))
	}
	if len(running) > 0 {
		parts = append(parts, "Still running: "+strings.Join(running, ", ")+".")
	}
	if timedOut {
		parts = append(parts, fmt.Sprintf("The wait timed out after %s; the agents still running keep going.", formatElapsed(timeout)))
	}
	return strings.Join(parts, "\n\n")
}

// Stop stops an agent for good.
func (h *agentHost) Stop(id string) (string, error) {
	if err := h.m.Stop(id); err != nil {
		return "", err
	}
	a, err := h.m.Get(id)
	if err != nil {
		return "Stopped " + id + ".", nil
	}
	return fmt.Sprintf("Stopped %s (%s); it is %s.", id, a.Label, a.State()), nil
}

// agentMode is the mode of a live agent, for changesWorkspaceWith.
func (h *agentHost) agentMode(id string) (string, bool) {
	a, err := h.m.Get(id)
	if err != nil {
		return "", false
	}
	return a.Mode, true
}

// agentModeLookup is host's agentMode, or nil when it has none.
func agentModeLookup(host tools.AgentHost) func(id string) (string, bool) {
	if h, ok := host.(*agentHost); ok && h != nil {
		return h.agentMode
	}
	return nil
}

var _ tools.AgentHost = (*agentHost)(nil)

// noticeHeader introduces a background agent's report to the model.
func noticeHeader(id string) string {
	return fmt.Sprintf("[automated: subagent %s finished — not from the user]", id)
}

// takeNotices takes the reports of background agents that finished, shows a line for
// each, and returns those of the active conversation's agents in one text for the
// model, which then has them. Reports of another conversation's agents, and of turns
// only the user asked for, are only shown.
func (h *agentHost) takeNotices(show func(string)) []string {
	if h == nil {
		return nil
	}
	var texts []string
	for _, n := range h.m.TakeNotices("") {
		show(h.noticeLine(n))
		if n.ForModel && (h.inMainLineage(n.ParentConv) || h.isAdopted(n.AgentID)) {
			texts = append(texts, noticeHeader(n.AgentID)+"\n"+n.Text)
			h.m.MarkDelivered(n.AgentID)
		}
	}
	return texts
}

// inMainLineage reports whether conv is main's current conversation, or an earlier id
// of it from before a compaction.
func (h *agentHost) inMainLineage(conv string) bool {
	if conv == activeConversationID {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lineage[conv]
}

// isAdopted reports whether main's current conversation took over agent id's reports
// by sending it a message (Send).
func (h *agentHost) isAdopted(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.adopted[id]
}

// carryLineage records that main's conversation went on from previous under a new id,
// so the agents spawned under previous keep reporting to it. Called on main.
func (h *agentHost) carryLineage(previous string) {
	if h == nil || previous == "" || previous == activeConversationID {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lineage == nil {
		h.lineage = map[string]bool{}
	}
	h.lineage[previous] = true
}

// forgetDelivered is a compaction, model switch, /clear or /resume: the conversation
// main goes on with may not hold the reports already delivered, so a wait on those
// agents gives them again.
func (h *agentHost) forgetDelivered() {
	if h != nil {
		h.m.ForgetDelivered()
	}
}

// leaveLineage is a switch to another conversation, such as /clear or /resume: the
// agents of the one before only show their reports from now on.
func (h *agentHost) leaveLineage() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	clear(h.lineage)
	clear(h.adopted)
}

// busyWriters are the general agents still running, which may still change the
// workspace.
func (h *agentHost) busyWriters() []string {
	if h == nil {
		return nil
	}
	var ids []string
	for _, info := range h.m.List() {
		if !info.Evicted && info.State.Busy() && info.Mode == tools.AgentModeGeneral {
			ids = append(ids, info.ID)
		}
	}
	return ids
}

// deferVerify wraps main's end-of-turn verify. While a general agent still runs, the
// workspace is still changing, so the check is skipped with a notice and pending is
// set: the next turn runs it when it ends, whether or not it changes anything.
func (h *agentHost) deferVerify(verify func(context.Context) (string, error), pending *bool) func(context.Context) (string, error) {
	if h == nil || verify == nil {
		return verify
	}
	return func(ctx context.Context) (string, error) {
		if ids := h.busyWriters(); len(ids) > 0 {
			*pending = true
			fmt.Fprintln(activityOut, formatNotice("Not verifying yet: "+strings.Join(ids, ", ")+" may still change the workspace. The verify command runs when the next turn ends; /verify run runs it now."))
			return "", nil
		}
		*pending = false
		return verify(ctx)
	}
}

// noticeLine is the line shown when a background agent finishes.
func (h *agentHost) noticeLine(n subagent.Notice) string {
	g := style.Glyphs()
	label, verb, mark := "", "finished", g.OK
	if a, err := h.m.Get(n.AgentID); err == nil {
		label = " " + escapeControlChars(oneLine(a.Label))
		if a.State() == subagent.Failed {
			verb, mark = "failed", g.Fail
		}
	}
	return fmt.Sprintf("%s %s%s %s %s /agents %s", mark, n.AgentID, label, verb, g.Dot, n.AgentID)
}

// turnNotices is the takeNotices hook of a main turn: finished agents' reports go to
// the model with the next tool results.
func (h *agentHost) turnNotices() string {
	showHeldWarnings()
	return strings.Join(h.takeNotices(func(line string) {
		showAgentActivity(formatNotice(line) + "\n")
	}), "\n\n")
}

// collectAgentNotices, between turns, shows the agents that finished and keeps their
// reports for the next message.
func collectAgentNotices(cfg *config) {
	showHeldWarnings()
	taken := cfg.agents.takeNotices(func(line string) {
		fmt.Fprintln(activityOut, formatNotice(line))
	})
	if len(taken) == 0 {
		return
	}
	// The reports go before the next prompt; the history keeps them too, once however
	// often that prompt is tried, so a summary of the conversation, or its export, has
	// what the model acted on.
	recordHistory(historyEntry{Type: "user", Content: strings.Join(taken, "\n\n"), Provider: cfg.model.Provider})
	cfg.agentNotices = append(cfg.agentNotices, taken...)
}

// agentNoticesNote goes before the next prompt with the reports of the background
// agents that finished between turns.
func agentNoticesNote(notices []string) string {
	if len(notices) == 0 {
		return ""
	}
	return strings.Join(notices, "\n\n") + "\n\n"
}

// refuseWhileAgentsRun refuses commands that change what running agents depend on,
// such as the keys and the conversation. A stopped agent whose turn has not ended yet,
// and a message waiting to start a turn, count as running: either runs a turn later.
func (c *config) refuseWhileAgentsRun() error {
	if c.agents == nil {
		return nil
	}
	if active := c.agents.m.Active(); active > 0 {
		return fmt.Errorf("%s running; stop agents first (/agents stop all)", plural(active, "subagent"))
	}
	return nil
}

// stopAgentsOnExit stops the session's agents as goat exits, waiting a little for
// their turns to end.
func stopAgentsOnExit(out io.Writer) {
	h := runningAgents.Load()
	if h == nil {
		return
	}
	if running, _, _ := h.m.Counts(); running > 0 {
		fmt.Fprintln(out, formatNotice("Stopping "+plural(running, "subagent")+"."))
	}
	if stragglers := h.m.StopAll(h.m.Config().StopTimeout); len(stragglers) > 0 {
		fmt.Fprintln(out, formatWarning("Subagents still running at exit: "+strings.Join(stragglers, ", ")))
	}
}

// ringSink keeps a subagent's turn in its event ring, and never touches the terminal.
// Streamed text becomes one event per response; the last response's text is the
// agent's report.
type ringSink struct {
	a *subagent.Agent

	mu   sync.Mutex
	buf  strings.Builder
	last string
}

func (s *ringSink) append(e subagent.Event) {
	if ring := s.a.Ring(); ring != nil {
		ring.Append(e)
	}
}

func (s *ringSink) Text(text string) {
	s.mu.Lock()
	first := s.buf.Len() == 0
	s.buf.WriteString(strings.ReplaceAll(text, "\r", ""))
	s.mu.Unlock()
	if first {
		s.a.SetActivity("Writing")
	}
}

// EndText ends one streamed response, whether or not any text came.
func (s *ringSink) EndText() {
	s.mu.Lock()
	text := s.buf.String()
	s.buf.Reset()
	s.last = text
	s.mu.Unlock()
	if strings.TrimSpace(text) != "" {
		s.append(subagent.Event{Kind: subagent.EventText, Text: escapeControlChars(text)})
	}
}

func (s *ringSink) ToolStart(c provider.ToolCall) {
	s.append(subagent.Event{Kind: subagent.EventToolStart, Tool: c.Name, Args: escapeControlChars(c.Arguments)})
	s.a.SetActivity(toolDisplayName(c.Name))
}

func (s *ringSink) ToolResult(c provider.ToolCall, out string, err error, elapsed time.Duration) {
	e := subagent.Event{Kind: subagent.EventToolResult, Tool: c.Name, Args: escapeControlChars(c.Arguments), Output: escapeControlChars(out), Elapsed: elapsed}
	if err != nil {
		e.Err = escapeControlChars(err.Error())
	}
	s.append(e)
}

func (s *ringSink) User(text string) {
	s.append(subagent.Event{Kind: subagent.EventUser, Text: escapeControlChars(text)})
}

func (s *ringSink) Notice(text string) {
	s.append(subagent.Event{Kind: subagent.EventNotice, Text: escapeControlChars(text)})
}

// Status("") clears the activity.
func (s *ringSink) Status(label string) {
	s.a.SetActivity(label)
}

// report is the text of the turn's last response.
func (s *ringSink) report() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.last)
}
