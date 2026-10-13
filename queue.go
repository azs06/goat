package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/azs06/goat/internal/style"
)

// While a turn runs on an interactive color terminal, keys go to a line under the
// spinner instead of being thrown away. Enter queues the line: plain text is sent to the
// model with the next tool results, so it can steer a turn that is still working, and
// whatever is left when the turn ends is sent as the next prompt. Slash commands and !
// shell commands always wait for the turn to end. Up on an empty line takes the last queued message back to
// edit it. After Ctrl-C or a failed turn nothing is sent; the queue goes back into the
// prompt instead.

type queueState struct {
	mu     sync.Mutex
	items  []string
	editor promptEditor
	// capturing is true while keys are read into editor, which is when it is drawn.
	capturing bool
	// footer is the status under the input while a turn runs, taken when it starts:
	// building it runs git, too slow for every redraw. Without it the input is drawn
	// without its frame. The subagents' part is added live at each redraw.
	footer []string
	// emptyHint replaces the hint on the empty input line, such as in the attach view.
	emptyHint string
	// openAgents is set by Ctrl-G during a turn: /agents runs once the turn is over,
	// before anything else queued.
	openAgents bool
}

var turnQueue queueState

// lineQueued receives a value when a line is queued, for the attach view, which
// handles each line as it comes. It holds at most one.
var lineQueued = make(chan struct{}, 1)

func nudgeLineQueued() {
	select {
	case lineQueued <- struct{}{}:
	default:
	}
}

// editorText is the line being typed; the caller holds mu.
func (q *queueState) editorText() string {
	return string(q.editor.buf)
}

// enqueue adds text to the end of the queue, to run as if the user had typed it
// during a turn.
func enqueue(text string) {
	turnQueue.mu.Lock()
	defer turnQueue.mu.Unlock()
	turnQueue.items = append(turnQueue.items, text)
}

// queueKey applies one key typed during a turn.
func queueKey(key string) {
	turnQueue.mu.Lock()
	defer turnQueue.mu.Unlock()
	editor := &turnQueue.editor
	// The command menu is never shown under the spinner.
	editor.menuClosed = true
	defer func() { editor.menuClosed = true }()

	switch {
	case !editor.pasting && key == "\x07": // Ctrl-G: the agents picker, once the turn is over
		turnQueue.openAgents = true
		nudgeLineQueued()
	case !editor.pasting && (key == "\x10" || key == "\x1b[A" || key == "\x1bOA") && len(editor.buf) == 0: // Ctrl-P, Up
		if len(turnQueue.items) > 0 {
			last := len(turnQueue.items) - 1
			editor.setText(turnQueue.items[last])
			turnQueue.items = turnQueue.items[:last]
		}
	case !editor.pasting && (key == "\x04" || key == "\t"): // Ctrl-D does not end input mid-turn; Tab has no menu here.
	case key == "?": // the shortcuts panel belongs to the prompt; here it is just text
		editor.insert(key)
	default:
		if done, _ := editor.handleKey(key); done {
			if text := strings.TrimSpace(string(editor.buf)); text != "" {
				turnQueue.items = append(turnQueue.items, text)
				nudgeLineQueued()
			}
			editor.setText("")
		}
	}
}

// isQueuedCommand reports whether a queued line is a slash command or a ! shell
// command, which run on their own once the turn is over.
func isQueuedCommand(item string) bool {
	return strings.HasPrefix(item, "/") || strings.HasPrefix(item, "!")
}

// takeQueuedText removes the plain text messages at the front of the queue and joins
// them, stopping at the first command. It returns "" when there are none.
func takeQueuedText() string {
	turnQueue.mu.Lock()
	defer turnQueue.mu.Unlock()
	n := 0
	for n < len(turnQueue.items) && !isQueuedCommand(turnQueue.items[n]) {
		n++
	}
	text := strings.Join(turnQueue.items[:n], "\n")
	turnQueue.items = turnQueue.items[n:]
	return text
}

// nextQueuedInput is what runs next once a turn is over: a command on its own, or the
// plain text messages up to the next command, joined.
func nextQueuedInput() (string, bool) {
	turnQueue.mu.Lock()
	if turnQueue.openAgents {
		turnQueue.openAgents = false
		turnQueue.mu.Unlock()
		return agentsCommandLine, true
	}
	if len(turnQueue.items) > 0 && isQueuedCommand(turnQueue.items[0]) {
		command := turnQueue.items[0]
		turnQueue.items = turnQueue.items[1:]
		turnQueue.mu.Unlock()
		return command, true
	}
	turnQueue.mu.Unlock()
	text := takeQueuedText()
	return text, text != ""
}

// agentsCommandLine is what Ctrl-G runs.
const agentsCommandLine = "/agents"

// takeQueuedLine removes and returns the first queued line, whatever it is, or the
// /agents of a Ctrl-G. The attach view handles lines one at a time.
func takeQueuedLine() (string, bool) {
	turnQueue.mu.Lock()
	defer turnQueue.mu.Unlock()
	if turnQueue.openAgents {
		turnQueue.openAgents = false
		return agentsCommandLine, true
	}
	if len(turnQueue.items) == 0 {
		return "", false
	}
	line := turnQueue.items[0]
	turnQueue.items = turnQueue.items[1:]
	return line, true
}

// heldInput is what was queued or being typed for main when the attach view opened.
type heldInput struct {
	lines []string
	draft string
}

// holdQueuedInput takes the queued lines and the line being typed out of the queue,
// for restoreQueuedInput to put back, so the attach view sends none of what was meant
// for main to its agent.
func holdQueuedInput() heldInput {
	turnQueue.mu.Lock()
	defer turnQueue.mu.Unlock()
	held := heldInput{lines: turnQueue.items, draft: turnQueue.editorText()}
	turnQueue.items = nil
	turnQueue.editor.setText("")
	return held
}

// restoreQueuedInput puts held back: the draft in front of the text being typed, and
// the lines at the front of the queue, or, when toEditor is set, in front of the draft
// in the input, so they are not sent before the user sees them again.
func restoreQueuedInput(held heldInput, toEditor bool) {
	turnQueue.mu.Lock()
	defer turnQueue.mu.Unlock()
	var parts []string
	if toEditor {
		parts = append(parts, held.lines...)
	} else if len(held.lines) > 0 {
		turnQueue.items = append(held.lines, turnQueue.items...)
	}
	parts = append(parts, held.draft, turnQueue.editorText())
	var kept []string
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	if text := strings.Join(kept, "\n"); text != turnQueue.editorText() {
		turnQueue.editor.setText(text)
		turnQueue.editor.menuClosed = true
	}
}

// returnQueuedLinesToEditor puts the lines still queued back in front of the text
// being typed, so leaving the attach view sends none of them anywhere.
func returnQueuedLinesToEditor() {
	turnQueue.mu.Lock()
	defer turnQueue.mu.Unlock()
	turnQueue.openAgents = false
	if len(turnQueue.items) == 0 {
		return
	}
	parts := append(turnQueue.items, turnQueue.editorText())
	turnQueue.items = nil
	turnQueue.editor.setText(strings.TrimRight(strings.Join(parts, "\n"), "\n"))
	turnQueue.editor.menuClosed = true
}

// returnQueueToPrompt empties the queue into the next prompt's text, after Ctrl-C or an
// error, so nothing is sent that the user has not seen again.
func returnQueueToPrompt(input *lineReader) {
	turnQueue.mu.Lock()
	parts := append(turnQueue.items, strings.TrimSpace(input.partial))
	turnQueue.items = nil
	turnQueue.openAgents = false
	turnQueue.mu.Unlock()
	var kept []string
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	// One line each, so a queued ! command does not run the messages after it as its
	// arguments.
	input.partial = strings.Join(kept, "\n")
}

// queuedInputLines is the part of the live block under the spinner: the queued
// messages and the line being typed, each cut to fit in width-1 cells. cursor is the
// column (1-based) of the cursor on the last line. It returns nothing when keys are
// not being captured.
func queuedInputLines(width int) (lines []string, cursor int) {
	turnQueue.mu.Lock()
	defer turnQueue.mu.Unlock()
	if !turnQueue.capturing {
		return nil, 0
	}
	available := max(width-1, 10)
	items := turnQueue.items
	if turnQueue.openAgents {
		items = append([]string{agentsCommandLine}, items...)
	}
	for _, item := range items {
		label := "  queued  "
		lines = append(lines, style.Subtle.Render(label)+style.Muted.Render(style.TruncateToWidth(flattenBreaks(item), available-len(label))))
	}

	prefix := promptPrefix()
	prefixWidth := style.VisibleWidth(prefix)
	editor := turnQueue.editor
	if len(editor.buf) == 0 {
		hint := turnQueue.emptyHint
		if hint == "" {
			hint = "type a message to send when goat is ready"
		}
		if len(turnQueue.items) > 0 {
			hint = "enter queues another · ↑ edits the last one"
		}
		lines = append(lines, prefix+style.Subtle.Render(style.TruncateToWidth(hint, available-prefixWidth)))
		return lines, prefixWidth + 1
	}

	// Show the end of a line too long to fit, keeping the cursor in view.
	room := max(available-prefixWidth, 1)
	start := 0
	if editor.pos > room {
		start = editor.pos - room
	}
	end := min(len(editor.buf), start+room)
	lines = append(lines, prefix+escapeControlChars(flattenBreaks(string(editor.buf[start:end]))))
	return lines, prefixWidth + editor.pos - start + 1
}

// setTurnFooter sets the status shown under the input while the turn runs.
func setTurnFooter(parts []string) {
	turnQueue.mu.Lock()
	defer turnQueue.mu.Unlock()
	turnQueue.footer = parts
}

// turnFrameLines is the live block under the status line while keys are captured: the
// queued messages, then the input in the same frame as the prompt has between turns
// (a rule, the input line, a rule and the footer), so the bottom of the screen keeps
// its shape while goat works. cursorRow is the index of the input line in lines and
// cursor its 1-based column. When the footer is unknown or the frame would not fit in
// maxRows, the input is drawn on its own as the last line.
func turnFrameLines(width, maxRows int) (lines []string, cursorRow, cursor int) {
	lines, cursor = queuedInputLines(width)
	if len(lines) == 0 {
		return nil, 0, 0
	}
	turnQueue.mu.Lock()
	footer := turnQueue.footer
	turnQueue.mu.Unlock()
	if footer != nil {
		footer = withAgentsPart(footer, agentsFooterPart())
	}
	frame := 3
	if footer == nil || maxRows < 1+frame+1 {
		frame = 0
	}
	lines = capQueuedLines(lines, max(maxRows-frame, 1), width)
	if frame == 0 {
		return lines, len(lines) - 1, cursor
	}
	queued, input := lines[:len(lines)-1], lines[len(lines)-1]
	rule := renderRule(width - 1)
	framed := append(append([]string{}, queued...), rule, input, rule, renderFooter(footer, width))
	return framed, len(queued) + 1, cursor
}

// capQueuedLines keeps the queued messages and the input line within rows lines, so the
// block always fits on the screen: the latest messages stay, under a line counting the
// ones left out.
func capQueuedLines(lines []string, rows, width int) []string {
	if len(lines) <= rows {
		return lines
	}
	input := lines[len(lines)-1]
	keep := rows - 2 // the count line and the input line
	if keep < 0 {
		return []string{input}
	}
	queued := lines[:len(lines)-1]
	hidden := len(queued) - keep
	more := style.Subtle.Render(style.TruncateToWidth(fmt.Sprintf("  +%d more queued", hidden), max(width-1, 1)))
	return append(append([]string{more}, queued[hidden:]...), input)
}

// flattenBreaks shows a message with line breaks on a single line, one cell per break so
// cursor positions still match.
func flattenBreaks(text string) string {
	return strings.ReplaceAll(text, "\n", "↵")
}

// keyCapture reads keys into the queue while a turn runs.
type keyCapture struct {
	input  *lineReader
	parent context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// activeKeyCapture is the capture of the turn that is running, if any.
var activeKeyCapture *keyCapture

func currentKeyCapture() *keyCapture {
	uiMu.Lock()
	defer uiMu.Unlock()
	return activeKeyCapture
}

func setActiveKeyCapture(c *keyCapture) {
	uiMu.Lock()
	defer uiMu.Unlock()
	activeKeyCapture = c
}

// startKeyCapture starts reading keys into the queue until stopKeyCapture. The line
// left unsent at the previous prompt, if any, is where typing continues.
func startKeyCapture(ctx context.Context, input *lineReader) {
	turnQueue.mu.Lock()
	turnQueue.editor.setText(turnQueue.editorText() + input.partial)
	turnQueue.editor.menuClosed = true
	turnQueue.mu.Unlock()
	input.partial = ""

	c := &keyCapture{input: input, parent: ctx}
	setActiveKeyCapture(c)
	c.run()
}

func (c *keyCapture) run() {
	ctx, cancel := context.WithCancel(c.parent)
	c.cancel, c.done = cancel, make(chan struct{})
	turnQueue.mu.Lock()
	turnQueue.capturing = true
	turnQueue.mu.Unlock()
	go func() {
		defer close(c.done)
		for {
			key, err := c.input.readKey(ctx)
			if err != nil {
				return
			}
			queueKey(key)
			redrawSpinner()
		}
	}()
}

// pause stops reading, so a question asked mid-turn gets the keys, and waits until the
// reader is done. A key read in flight is kept for whoever reads next.
func (c *keyCapture) pause() {
	if c == nil || c.cancel == nil {
		return
	}
	turnQueue.mu.Lock()
	turnQueue.capturing = false
	turnQueue.mu.Unlock()
	c.cancel()
	<-c.done
	c.cancel = nil
}

func (c *keyCapture) resume() {
	if c == nil || c.cancel != nil {
		return
	}
	c.run()
}

// stopKeyCapture ends the turn's capture. The line typed but not queued goes back to
// the prompt.
func stopKeyCapture() {
	uiMu.Lock()
	c := activeKeyCapture
	activeKeyCapture = nil
	uiMu.Unlock()
	if c == nil {
		return
	}
	c.pause()
	turnQueue.mu.Lock()
	c.input.partial = turnQueue.editorText()
	turnQueue.editor.setText("")
	turnQueue.footer = nil
	turnQueue.mu.Unlock()
}
