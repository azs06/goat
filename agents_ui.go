package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/subagent"
)

// This file is the subagents' part of the interface: the footer badge, the /agents
// picker and its subcommands, the attach view that shows one agent's work and sends
// it messages, and what the idle prompt does when an agent asks or finishes. Like the
// rest of goat it reads stdin through one reader at a time: the attach view takes its
// lines from the key capture of the command it runs in, the same queue a turn uses.

// agentsFooterPart is the footer's subagents badge, such as "2 agents running · 1 needs
// approval (/agents)", or "" when none runs. It only counts, so it is cheap enough for
// every redraw of the frame under the spinner.
func agentsFooterPart() string {
	return runningAgents.Load().footerPart()
}

func (h *agentHost) footerPart() string {
	if h == nil {
		return ""
	}
	running, waiting, _ := h.m.Counts()
	if running == 0 {
		return ""
	}
	text := plural(running, "agent") + " running"
	if waiting > 0 {
		text += " " + style.Glyphs().Dot + fmt.Sprintf(" %d needs approval", waiting)
	}
	return text + " (/agents)"
}

// withAgentsPart puts the subagents badge in the footer parts, after the model and the
// modes, where it is not the first thing left out on a narrow terminal. A badge the
// parts already have is replaced.
func withAgentsPart(parts []string, part string) []string {
	kept := make([]string, 0, len(parts)+1)
	for _, p := range parts {
		if !strings.HasSuffix(p, "(/agents)") {
			kept = append(kept, p)
		}
	}
	if part == "" {
		return kept
	}
	at := min(2, len(kept))
	for at < len(kept) && (kept[at] == "plan mode" || kept[at] == "code mode") {
		at++
	}
	return slices.Insert(kept, at, part)
}

// promptEvents wakes the idle prompt when an agent changes. The prompt gives way when
// an agent finished or, with nothing typed, when one asks to run a command; otherwise
// it only redraws the footer.
func (h *agentHost) promptEvents(footer func() string) *promptEvents {
	if h == nil {
		return nil
	}
	return &promptEvents{
		changed: h.m.Changed(),
		pending: h.m.Broker.Pending(),
		interrupt: func(empty bool) bool {
			return h.m.NoticeCount() > 0 || (empty && h.m.Broker.Len() > 0)
		},
		footer: footer,
	}
}

// serveIdleApprovals asks about the commands agents wait to run, between turns, once
// nothing is typed at the prompt: the rest of a line the user keeps typing must not
// become the answer. The prompt gives way when the line is empty, and a prompt sent
// meanwhile asks first. Ctrl-C declines rather than ending goat.
func serveIdleApprovals(cfg *config) {
	h := cfg.agents
	if h == nil || h.m.Broker.Len() == 0 || cfg.input != nil && cfg.input.partial != "" {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := runWithInputGuard(false, func() error {
		h.serveApprovals(ctx, cfg)
		return nil
	})
	if err != nil {
		warnf("%v", err)
	}
}

// agentStateGlyph marks an agent's state: ▶ running, ◌ needs approval, · idle,
// ✗ failed, ■ stopped; ASCII in plain output.
func agentStateGlyph(state subagent.State) string {
	glyphs := map[subagent.State][2]string{
		subagent.Running:         {"▶", ">"},
		subagent.WaitingApproval: {"◌", "o"},
		subagent.Idle:            {"·", "-"},
		subagent.Failed:          {"✗", "x"},
		subagent.Stopped:         {"■", "#"},
	}
	g, ok := glyphs[state]
	if !ok {
		return "?"
	}
	if style.Enabled() {
		return g[0]
	}
	return g[1]
}

// agentElapsed is how long an agent has existed, up to its last change once it is
// done.
func agentElapsed(info subagent.Info, now time.Time) time.Duration {
	end := now
	if !info.State.Busy() {
		end = info.Updated
	}
	return max(end.Sub(info.Started), 0)
}

// agentDetail is what an agent is doing, or else the start of its report or error.
func agentDetail(info subagent.Info, width int) string {
	detail := info.Activity
	switch {
	case info.State.Busy() && detail == "":
		detail = "working"
	case info.State.Busy():
	case info.Err != "":
		detail = info.Err
	default:
		detail = info.ReportHead
	}
	return truncateForLog(escapeControlChars(oneLine(detail)), width)
}

// agentStateText names a state for listings.
func agentStateText(info subagent.Info) string {
	text := info.State.String()
	if info.State == subagent.WaitingApproval {
		text = "needs approval"
	}
	if info.Evicted {
		text += ", dismissed"
	}
	return text
}

// agentGroup is the picker heading an agent is listed under.
func agentGroup(info subagent.Info) string {
	switch {
	case info.State == subagent.WaitingApproval:
		return "Needs approval"
	case info.State == subagent.Running:
		return "Running"
	}
	return "Finished"
}

// agentPickerOptions are the rows of the /agents picker: main first, then the agents
// grouped as running, needing approval and finished. ids holds each row's agent id,
// "" for main.
func agentPickerOptions(infos []subagent.Info, now time.Time) (options []selectOption, ids []string) {
	options = append(options, selectOption{label: "main", hint: "the main conversation", current: true})
	ids = append(ids, "")
	dot := " " + style.Glyphs().Dot + " "
	for _, group := range []string{"Running", "Needs approval", "Finished"} {
		for _, info := range infos {
			if agentGroup(info) != group {
				continue
			}
			hint := agentStateGlyph(info.State) + " " + agentStateText(info) + dot + formatElapsed(agentElapsed(info, now))
			if detail := agentDetail(info, 60); detail != "" {
				hint += dot + detail
			}
			options = append(options, selectOption{label: info.ID + "  " + escapeControlChars(oneLine(info.Label)), hint: hint, group: group})
			ids = append(ids, info.ID)
		}
	}
	return options, ids
}

// renderAgentsTable lists the agents in plain text: id, state, time, label and what
// each is doing or reported.
func renderAgentsTable(infos []subagent.Info, now time.Time) string {
	if len(infos) == 0 {
		return "No subagents yet; the model starts them with spawn_agent."
	}
	rows := make([][]string, 0, len(infos))
	for _, info := range infos {
		rows = append(rows, []string{info.ID, agentStateGlyph(info.State) + " " + agentStateText(info), formatElapsed(agentElapsed(info, now)), info.Mode, escapeControlChars(oneLine(info.Label)), agentDetail(info, 60)})
	}
	widths := make([]int, 5)
	for _, row := range rows {
		for i := range widths {
			widths[i] = max(widths[i], style.VisibleWidth(row[i]))
		}
	}
	var b strings.Builder
	b.WriteString("Subagents:")
	for _, row := range rows {
		for i := range widths {
			row[i] = style.PadRight(row[i], widths[i])
		}
		b.WriteString("\n  " + strings.TrimRight(strings.Join(row, "  "), " "))
	}
	return b.String()
}

// commandAgents is /agents: the picker, or with arguments attach, log, stop and
// dismiss.
func commandAgents(ctx context.Context, c *config, args []string) error {
	h := c.agents
	if h == nil {
		return errors.New("subagents are not available here")
	}
	if len(args) == 0 {
		infos := h.m.List()
		if len(infos) == 0 {
			fmt.Println(formatNotice("No subagents yet; the model starts them with spawn_agent."))
			return nil
		}
		options, ids := agentPickerOptions(infos, time.Now())
		chosen, err := selectOptionInteractive(ctx, c, "Subagents", options)
		switch {
		case errors.Is(err, errSelectorUnavailable):
			fmt.Println(renderAgentsTable(infos, time.Now()))
			fmt.Println("/agents <id> shows an agent's work and sends it messages; /agents log <id> shows its whole transcript.")
			return nil
		case err != nil:
			return err
		case chosen <= 0:
			return nil
		}
		return h.attachOrLog(ctx, c, ids[chosen])
	}
	switch strings.ToLower(args[0]) {
	case "log":
		if len(args) != 2 {
			return errors.New("use /agents log <id>")
		}
		text, err := h.logText(args[1])
		if err != nil {
			return err
		}
		return showLong(ctx, c, text)
	case "stop":
		if len(args) != 2 {
			return errors.New("use /agents stop <id> or /agents stop all")
		}
		return h.stopFromUser(args[1])
	case "dismiss":
		if len(args) != 2 {
			return errors.New("use /agents dismiss <id> or /agents dismiss finished")
		}
		return h.dismissFromUser(args[1])
	}
	if len(args) > 1 {
		return errors.New("use /agents, /agents <id>, /agents log <id>, /agents stop <id|all> or /agents dismiss <id|finished>")
	}
	return h.attachOrLog(ctx, c, args[0])
}

// attachOrLog attaches to a live agent, and shows the transcript of a dismissed one.
func (h *agentHost) attachOrLog(ctx context.Context, c *config, id string) error {
	a, err := h.m.Get(id)
	if err == nil {
		return h.attach(ctx, c, a)
	}
	if _, ok := h.info(id); ok {
		text, err := h.logText(id)
		if err != nil {
			return err
		}
		return showLong(ctx, c, text)
	}
	return err
}

// info finds an agent, live or dismissed.
func (h *agentHost) info(id string) (subagent.Info, bool) {
	for _, info := range h.m.List() {
		if info.ID == id {
			return info, true
		}
	}
	return subagent.Info{}, false
}

func (h *agentHost) stopFromUser(target string) error {
	if strings.EqualFold(target, "all") {
		var stopped []string
		for _, info := range h.m.List() {
			// An idle agent with messages waiting for a slot starts once one frees, so
			// it is stopped too.
			if info.Evicted || !info.State.Busy() && info.Inbox == 0 {
				continue
			}
			if err := h.m.Stop(info.ID); err == nil {
				stopped = append(stopped, info.ID)
			}
		}
		if len(stopped) == 0 {
			fmt.Println(formatNotice("No subagent is running."))
			return nil
		}
		fmt.Println(formatSuccess("Stopped " + strings.Join(stopped, ", ") + "."))
		return nil
	}
	if err := h.m.Stop(target); err != nil {
		return err
	}
	fmt.Println(formatSuccess("Stopped " + target + "; /agents " + target + " can still send it a message."))
	return nil
}

func (h *agentHost) dismissFromUser(target string) error {
	if strings.EqualFold(target, "finished") {
		ids := h.m.DismissFinished()
		if len(ids) == 0 {
			fmt.Println(formatNotice("No finished subagent to dismiss."))
			return nil
		}
		fmt.Println(formatSuccess("Dismissed " + strings.Join(ids, ", ") + "; /agents log <id> still shows their transcripts."))
		return nil
	}
	if err := h.m.Dismiss(target); err != nil {
		if errors.Is(err, subagent.ErrBusy) {
			return fmt.Errorf("%s is running, or has messages or a report not yet delivered; stop it or wait first", target)
		}
		return err
	}
	fmt.Println(formatSuccess("Dismissed " + target + "; /agents log " + target + " still shows its transcript."))
	return nil
}

// logText is an agent's whole transcript, from history, so it works for dismissed
// agents too.
func (h *agentHost) logText(id string) (string, error) {
	info, ok := h.info(id)
	if !ok {
		_, err := h.m.Get(id)
		return "", err
	}
	entries, err := loadConversationHistoryEntries()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(agentDivider(info, "transcript"))
	b.WriteString("\n")
	shown := 0
	for _, entry := range entries {
		if entry.ConversationID != info.ConvID {
			continue
		}
		if text := renderAgentLogEntry(entry); text != "" {
			b.WriteString(text)
			shown++
		}
	}
	if shown == 0 {
		b.WriteString("(nothing recorded yet)\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// renderAgentLogEntry draws one history entry of an agent's transcript.
func renderAgentLogEntry(entry historyEntry) string {
	call := provider.ToolCall{Name: entry.ToolName, Arguments: entry.Arguments}
	switch entry.Type {
	case "user":
		return renderAgentEvent(subagent.Event{Kind: subagent.EventUser, Text: escapeControlChars(strings.TrimSpace(entry.Content))})
	case "assistant":
		if strings.TrimSpace(entry.Content) == "" {
			return ""
		}
		return renderAgentEvent(subagent.Event{Kind: subagent.EventText, Text: escapeControlChars(entry.Content)})
	case "tool_call":
		return indentBlock(renderToolHeader(call, toolRenderWidth()-len(nestedIndent)), nestedIndent) + "\n"
	case "tool_output":
		var err error
		if entry.Status != "" && entry.Status != "ok" {
			err = errors.New(entry.Status)
		}
		return indentBlock(renderToolResult(call, escapeControlChars(entry.Content), err, 0, toolRenderWidth()-len(nestedIndent)), nestedIndent) + "\n"
	case "error":
		return indentBlock(formatError(escapeControlChars(entry.Content)), nestedIndent) + "\n"
	}
	return ""
}

// agentDivider is the rule that opens an agent's view: "── a2 · fix flaky test ·
// general ── what".
func agentDivider(info subagent.Info, what string) string {
	g := style.Glyphs()
	rule := g.HLine + g.HLine
	text := rule + " " + info.ID + " " + g.Dot + " " + escapeControlChars(oneLine(info.Label)) + " " + g.Dot + " " + info.Mode + " " + rule + " " + what
	return style.Subtle.Render(style.TruncateToWidth(text, max(terminalWidth()-1, 20)))
}

// attachReplay is how many recent events attaching shows.
const attachReplay = 60

// replayAgent is the start of the attach view: the divider, then the agent's latest
// events, after a line counting those left out. It returns the last event's Seq.
func replayAgent(a *subagent.Agent, what string) (string, uint64) {
	var b strings.Builder
	b.WriteString(agentDivider(a.Info(), what) + "\n")
	ring := a.Ring()
	if ring == nil {
		return b.String(), 0
	}
	events := ring.Since(0)
	hidden := ring.Dropped()
	if len(events) > attachReplay {
		hidden += len(events) - attachReplay
		events = events[len(events)-attachReplay:]
	}
	if hidden > 0 {
		b.WriteString(style.Subtle.Render(fmt.Sprintf("%s %s (/agents log %s)", style.Glyphs().Ellipsis, plural(hidden, "earlier event"), a.ID)) + "\n")
	}
	var cursor uint64
	for _, e := range events {
		b.WriteString(renderAgentEvent(e))
		cursor = e.Seq
	}
	return b.String(), cursor
}

// attachAction is what a line typed in the attach view leads to.
type attachAction int

const (
	attachStay attachAction = iota
	attachBack
	attachSwitch
)

// attachReadOnly are the commands that run in the attach view; the rest act on the
// main conversation.
var attachReadOnly = map[string]bool{"agents": true, "help": true, "ps": true, "tools": true, "output": true}

// attach shows an agent's work as it happens and sends it what the user types, until
// /back or Ctrl-C, which leave the agent running (in plain mode Ctrl-C interrupts the
// turn it shows). /agents <id> switches agents. A turn
// the user starts here is not announced when it ends while the user still looks on.
//
// Opened during main's turn (runsMidTurn), it is one of main's pauses: main's turn
// waits until the view closes, and Ctrl-C only closes the view, leaving the turn
// running.
func (h *agentHost) attach(ctx context.Context, cfg *config, a *subagent.Agent) error {
	midTurn := runsMidTurn(ctx)
	if midTurn {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		defer redirectInterrupt(cancel)()
		// What is typed here is for the agent, not commands for main's pause.
		defer setMidTurn(setMidTurn(false))
		defer setTurnStatus("")
	}
	defer h.m.SetAttached("")
	// Lines queued, and the line typed, before the view opened were meant for main:
	// they wait, and go to main as they would have once the view closes. Ctrl-C sends
	// nothing, so after it the queued lines wait in the prompt instead.
	held := holdQueuedInput()
	defer func() { restoreQueuedInput(held, ctx.Err() != nil) }()
	for {
		h.m.SetAttached(a.ID)
		next, err := h.attachOnce(ctx, cfg, a)
		if err != nil || next == "" {
			return err
		}
		other, err := h.m.Get(next)
		if err != nil {
			showAgentActivity(formatError(err.Error()) + "\n")
			continue
		}
		a = other
	}
}

// attachOnce is the attach view of one agent. It returns the id of the agent to switch
// to, or "" when the user went back to main.
func (h *agentHost) attachOnce(ctx context.Context, cfg *config, a *subagent.Agent) (string, error) {
	if currentKeyCapture() == nil || !style.Enabled() {
		return h.attachPlain(ctx, cfg, a)
	}
	stopSpinner()
	dot := " " + style.Glyphs().Dot + " "
	what := "type to message" + dot + "/stop interrupts" + dot + "/back or Ctrl-C returns"
	hint := "type a message for " + a.ID + dot + "/back returns"
	if runsMidTurn(ctx) {
		// Main's turn waits on this view, on main's goroutine; the agents do not.
		what = "main's turn paused" + dot + what
		hint = "type a message for " + a.ID + dot + "/back returns to main's turn"
		setTurnStatus("attached to " + a.ID + dot + "main's turn paused")
	}
	replay, cursor := replayAgent(a, what)
	h.noteShown(a.ID, cursor)
	fmt.Print("\n" + replay)
	setQueueHint(hint)
	defer setQueueHint("")
	startSpinnerHint(attachLabel(a), "ctrl-c returns to main")
	// Lines not handled yet go back to the input, so nothing typed here reaches main.
	defer returnQueuedLinesToEditor()
	defer stopSpinner()

	ring := a.Ring()
	var notify <-chan struct{}
	if ring != nil {
		notify = ring.Notify()
	}
	for {
		h.showAttached(a, ring, &cursor)
		if ctx.Err() != nil {
			stopSpinner()
			fmt.Fprintln(activityOut, formatNotice("Back on main; "+a.ID+" keeps going (/agents "+a.ID+" returns)."))
			return "", nil
		}
		// As at the idle prompt, an approval waits while the user has text typed: the
		// rest of the line would become its answer. Emptying the line wakes this loop.
		if h.m.Broker.Len() > 0 && queueEditorEmpty() {
			h.serveApprovals(ctx, cfg)
			continue
		}
		if line, ok := takeQueuedLine(); ok {
			switch action, target := h.attachLine(ctx, cfg, a, line); action {
			case attachBack:
				stopSpinner()
				fmt.Fprintln(activityOut, formatNotice("Back on main."))
				return "", nil
			case attachSwitch:
				return target, nil
			}
			continue
		}
		select {
		case <-notify:
		case <-h.m.Broker.Pending():
		case <-lineQueued:
		case <-h.m.Changed():
		case <-ctx.Done():
		}
	}
}

// attachLabel is the spinner's text in the attach view.
func attachLabel(a *subagent.Agent) string {
	info := a.Info()
	activity := info.Activity
	if !info.State.Busy() {
		activity = agentStateText(info)
	} else if activity == "" {
		activity = "working"
	}
	return a.ID + " " + style.Glyphs().Dot + " " + activity
}

// showAttached prints the agent's events after cursor and names what it does.
func (h *agentHost) showAttached(a *subagent.Agent, ring *subagent.EventRing, cursor *uint64) {
	if ring != nil {
		if events := ring.Since(*cursor); len(events) > 0 {
			*cursor = events[len(events)-1].Seq
			var b strings.Builder
			for _, e := range events {
				b.WriteString(renderAgentEvent(e))
			}
			showAgentActivity(b.String())
			h.noteShown(a.ID, *cursor)
		}
	}
	relabelSpinner(attachLabel(a))
}

// setQueueHint sets the hint on the empty input line under the spinner.
func setQueueHint(hint string) {
	turnQueue.mu.Lock()
	defer turnQueue.mu.Unlock()
	turnQueue.emptyHint = hint
}

// attachLine handles one line typed in the attach view: text goes to the agent, /back
// returns, /stop interrupts its turn, and read-only commands run; other commands act
// on main and are refused here.
func (h *agentHost) attachLine(ctx context.Context, cfg *config, a *subagent.Agent, line string) (attachAction, string) {
	say := func(text string) { showAgentActivity(text + "\n") }
	text := strings.TrimSpace(line)
	switch strings.ToLower(text) {
	case "":
		return attachStay, ""
	case "/back":
		return attachBack, ""
	case "/stop":
		if !a.State().Busy() {
			say(formatNotice(a.ID + " is not running."))
			return attachStay, ""
		}
		a.InterruptTurn(subagent.ErrUserInterrupt)
		say(formatNotice("Interrupting " + a.ID + "; it keeps its conversation."))
		return attachStay, ""
	}
	if strings.HasPrefix(text, "!") {
		say(formatNotice("commands act on the main conversation; /back first"))
		return attachStay, ""
	}
	if strings.HasPrefix(text, "/") {
		cmd, args, err := lookupCommand(text)
		if err != nil {
			say(formatError(err.Error()))
			return attachStay, ""
		}
		if cmd != nil {
			if cmd.name == "agents" && len(args) == 0 {
				say(renderAgentsTable(h.m.List(), time.Now()))
				return attachStay, ""
			}
			if cmd.name == "agents" && len(args) == 1 {
				if args[0] == a.ID {
					return attachStay, ""
				}
				if _, err := h.m.Get(args[0]); err == nil {
					return attachSwitch, args[0]
				}
			}
			if !attachReadOnly[cmd.name] {
				say(formatNotice("commands act on the main conversation; /back first"))
				return attachStay, ""
			}
			resume := pauseSpinner()
			if err := cmd.callback(ctx, cfg, args); err != nil {
				fmt.Println(formatError(err.Error()))
			}
			resume()
			return attachStay, ""
		}
	}
	note, err := h.sendFromUser(a, text)
	switch {
	case err != nil:
		say(formatError(err.Error()))
	case note != "":
		say(formatNotice(note))
	}
	return attachStay, ""
}

// sendFromUser gives an agent the user's message. It starts a new turn of an agent
// that is not running, the user's to resume even when stopped; a running agent reads
// it at its next step. Either way the message shows in the agent's events once, when
// a turn takes it, and the note says what became of it.
func (h *agentHost) sendFromUser(a *subagent.Agent, text string) (string, error) {
	delivery, err := h.m.Deliver(a.ID, text, true)
	if err != nil {
		return "", err
	}
	switch delivery {
	case subagent.Started:
		return "", nil
	case subagent.Steered:
		return a.ID + " reads it at its next step.", nil
	case subagent.Held:
		return a.ID + "'s report is on its way to main; the message starts a turn once main has it.", nil
	}
	running, _, _ := h.m.Counts()
	return fmt.Sprintf("%s running; message kept in %s's inbox until one finishes.", plural(running, "agent"), a.ID), nil
}

// attachPlain is the attach view without the live input line: plain output, or no
// key capture. It asks for one message at a time and shows the turn it starts.
func (h *agentHost) attachPlain(ctx context.Context, cfg *config, a *subagent.Agent) (string, error) {
	replay, cursor := replayAgent(a, "type a message; an empty line returns")
	fmt.Print(replay)
	ring := a.Ring()
	h.followPlain(ctx, cfg, a, ring, &cursor)
	for ctx.Err() == nil {
		answer, err := cfg.ask(ctx, "message for "+a.ID+" (empty returns): ")
		if err != nil || answer == "" {
			return "", nil
		}
		switch action, target := h.attachLine(ctx, cfg, a, answer); action {
		case attachBack:
			return "", nil
		case attachSwitch:
			return target, nil
		}
		h.followPlain(ctx, cfg, a, ring, &cursor)
	}
	return "", nil
}

// followPlain shows the agent's events until its turn ends, answering approvals. Where
// nothing may run in the background, Ctrl-C interrupts the turn as it does a followed
// spawn's: the plain prompt main goes back to would not serve its approvals.
func (h *agentHost) followPlain(ctx context.Context, cfg *config, a *subagent.Agent, ring *subagent.EventRing, cursor *uint64) {
	var notify <-chan struct{}
	if ring != nil {
		notify = ring.Notify()
	}
	for {
		h.showAttached(a, ring, cursor)
		if !a.State().Busy() {
			return
		}
		if ctx.Err() != nil {
			if !h.backgroundAllowed() {
				// As follow does, give the turn StopTimeout to end before the prompt.
				a.InterruptTurn(subagent.ErrUserInterrupt)
				timer := time.NewTimer(h.m.Config().StopTimeout)
				defer timer.Stop()
				select {
				case <-a.TurnDone():
				case <-timer.C:
				}
			}
			return
		}
		select {
		case <-a.TurnDone():
		case <-notify:
		case <-h.m.Broker.Pending():
			h.serveApprovals(ctx, cfg)
		case <-ctx.Done():
		}
	}
}

// agentCompletions offers the agents' ids and the subcommands after "/agents ".
func agentCompletions(c *config, add func(value, description string)) {
	if c.agents != nil {
		for _, info := range c.agents.m.List() {
			add(info.ID, agentStateText(info)+" "+style.Glyphs().Dot+" "+escapeControlChars(oneLine(info.Label)))
		}
	}
	add("log", "an agent's whole transcript")
	add("stop", "stop an agent, or all")
	add("dismiss", "forget a finished agent, or all finished")
}
