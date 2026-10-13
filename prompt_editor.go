package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/terminal"
)

// The interactive prompt is edited in cbreak mode so slash commands can be completed
// as they are typed. Typing "/" opens a menu of matching commands under the input in
// place of the footer: Up/Down move the selection, Tab completes it, Enter runs it and
// Esc closes the menu. Piped input and plain output never use the editor.

// completion is one entry of the menu: a command, or a value for a command's argument.
// display is what the menu shows; text is what Tab and Enter put in the prompt.
type completion struct {
	display, description, text string
	// argument is set for argument values, which Enter runs straight away.
	argument bool
	// path is set for file path matches, which Enter and Tab put in the prompt without
	// sending it; cursor is where the cursor goes after.
	path   bool
	cursor int
}

// argumentCompletions, when set, offers values for a command's first argument, such as
// the effort levels after "/effort ". It returns nil for commands without suggestions.
var argumentCompletions func(command, partial string) []completion

// commandCompletions lists what text could be completed to: after a slash, commands
// whose name starts with what follows it, then aliases whose command did not already
// match; after a command and a space, values for its first argument. Other text gets
// none.
func commandCompletions(text string) []completion {
	if !strings.HasPrefix(text, "/") {
		return nil
	}
	if name, partial, found := strings.Cut(text[1:], " "); found {
		cmd, ok := findCommand(strings.ToLower(name))
		if !ok || argumentCompletions == nil || strings.ContainsFunc(partial, unicode.IsSpace) {
			return nil
		}
		return argumentCompletions(cmd.name, partial)
	}
	if strings.ContainsFunc(text, unicode.IsSpace) {
		return nil
	}
	prefix := strings.ToLower(text[1:])

	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	var matches []completion
	for _, name := range names {
		if strings.HasPrefix(name, prefix) {
			matches = append(matches, completion{display: "/" + name, description: commands[name].description, text: "/" + name})
		}
	}
	if prefix == "" {
		return matches
	}

	aliases := make([]string, 0, len(commandAliases))
	for alias := range commandAliases {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		target := commandAliases[alias]
		if strings.HasPrefix(alias, prefix) && !strings.HasPrefix(target, prefix) {
			matches = append(matches, completion{display: "/" + alias, description: "Same as /" + target, text: "/" + alias})
		}
	}
	return matches
}

// maxCompletionRows is how many menu entries show at once; the list scrolls past it.
const maxCompletionRows = 6

// renderCompletionMenu draws the visible window of the menu, one line per entry, each
// fitting in width-1 cells.
func renderCompletionMenu(items []completion, selected, width int) []string {
	start := 0
	if selected >= maxCompletionRows {
		start = selected - maxCompletionRows + 1
	}
	end := min(len(items), start+maxCompletionRows)

	nameWidth := 0
	for _, item := range items {
		nameWidth = max(nameWidth, style.VisibleWidth(item.display))
	}

	available := width - 1
	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		marker := "  "
		if i == selected {
			marker = style.Glyphs().Prompt + " "
		}
		head := style.TruncateToWidth(marker+style.PadRight(items[i].display, nameWidth+2), available)
		description := style.TruncateToWidth(items[i].description, available-style.VisibleWidth(head))
		if i == selected {
			lines = append(lines, style.Selected.Render(head)+style.Muted.Render(description))
		} else {
			lines = append(lines, style.Muted.Render(head)+style.Subtle.Render(description))
		}
	}
	return lines
}

// promptEditor is the state of the text being typed at the interactive prompt. The text
// may span several lines: Ctrl-J, Alt-Enter or a backslash before Enter starts a new
// line, and pasted text keeps its line breaks.
type promptEditor struct {
	buf []rune
	pos int
	// selected is the highlighted menu entry; menuClosed hides the menu after Esc
	// until the text changes.
	selected   int
	menuClosed bool
	// pasting is set between the markers a terminal in bracketed paste mode puts around
	// pasted text, so a pasted line break is text rather than Enter.
	pasting bool
	// pathItems is the menu Tab opened for a path with several matches.
	pathItems []completion
	// showShortcuts replaces the footer with the key bindings; "?" on an empty prompt
	// turns it on and the next key turns it off.
	showShortcuts bool
	// history holds earlier prompts, oldest first; historyIndex is the one shown, or
	// len(history) for the text being written, which draft keeps while browsing.
	history      []string
	historyIndex int
	draft        string
	// cursorRow is the row of the cursor below the prompt line as last drawn.
	cursorRow int
	// images, when set, attaches images to the message: Ctrl-V (or Alt-V, for
	// terminals that keep Ctrl-V for their own paste) takes the clipboard's image, and a
	// pasted path to an image file, as dragging a file onto the terminal pastes, becomes
	// one. The image's label goes in the text in its place.
	images imageAttacher
	// pasteFrom is where the text being pasted started.
	pasteFrom int
	// notice replaces the footer until the next key, such as why no image was attached.
	notice string
}

// imageAttacher attaches images at the prompt; see promptEditor.images.
type imageAttacher interface {
	pasteClipboard() (label string, err error)
	attachPasted(text string) (label string, ok bool)
}

// insertImageLabel puts an image's label at the cursor, apart from the words around it.
func (e *promptEditor) insertImageLabel(label string) {
	if e.pos > 0 && !unicode.IsSpace(e.buf[e.pos-1]) {
		label = " " + label
	}
	e.insert(label + " ")
}

func (e *promptEditor) completions() []completion {
	if e.menuClosed {
		return nil
	}
	if len(e.pathItems) > 0 {
		return e.pathItems
	}
	if strings.ContainsRune(string(e.buf), '\n') {
		return nil
	}
	return commandCompletions(string(e.buf))
}

// apply puts a path match in the prompt.
func (e *promptEditor) apply(item completion) {
	e.buf = []rune(item.text)
	e.pos = item.cursor
	e.edited()
}

// completePath completes the path before the cursor: in place when there is one match
// or the matches share a longer start, otherwise by opening a menu of them.
func (e *promptEditor) completePath() {
	matches, start := pathCompletions(e.buf, e.pos)
	if len(matches) == 0 {
		return
	}
	replace := func(with string) completion {
		text := string(e.buf[:start]) + with + string(e.buf[e.pos:])
		return completion{display: with, text: text, path: true, cursor: start + len([]rune(with))}
	}
	if len(matches) == 1 {
		e.apply(replace(matches[0]))
		return
	}
	if prefix := commonPrefix(matches); len([]rune(prefix)) > e.pos-start {
		e.apply(replace(prefix))
		return
	}
	items := make([]completion, len(matches))
	for i, match := range matches {
		items[i] = replace(match)
	}
	e.pathItems = items
	e.selected = 0
}

// browseHistory shows the previous (-1) or next (+1) prompt from history.
func (e *promptEditor) browseHistory(direction int) {
	next := e.historyIndex + direction
	if next < 0 || next > len(e.history) {
		return
	}
	if e.historyIndex == len(e.history) {
		e.draft = string(e.buf)
	}
	e.historyIndex = next
	if next == len(e.history) {
		e.setText(e.draft)
	} else {
		e.setText(e.history[next])
	}
}

func (e *promptEditor) setText(text string) {
	e.buf = []rune(text)
	e.pos = len(e.buf)
	e.edited()
}

func (e *promptEditor) edited() {
	e.selected = 0
	e.menuClosed = false
	e.pathItems = nil
}

func (e *promptEditor) insert(text string) {
	runes := []rune(text)
	e.buf = append(e.buf[:e.pos], append(runes, e.buf[e.pos:]...)...)
	e.pos += len(runes)
	e.edited()
}

func (e *promptEditor) deleteRange(from, to int) {
	if from >= to {
		return
	}
	e.buf = append(e.buf[:from], e.buf[to:]...)
	e.pos = from
	e.edited()
}

// lineStart and lineEnd are the bounds of the line the cursor is on.
func (e *promptEditor) lineStart() int {
	start := e.pos
	for start > 0 && e.buf[start-1] != '\n' {
		start--
	}
	return start
}

func (e *promptEditor) lineEnd() int {
	end := e.pos
	for end < len(e.buf) && e.buf[end] != '\n' {
		end++
	}
	return end
}

// moveLine moves the cursor to the line above (-1) or below (+1), keeping its column
// where the line is long enough. It reports false when there is no such line.
func (e *promptEditor) moveLine(direction int) bool {
	start := e.lineStart()
	column := e.pos - start
	if direction < 0 {
		if start == 0 {
			return false
		}
		prevEnd := start - 1
		prevStart := prevEnd
		for prevStart > 0 && e.buf[prevStart-1] != '\n' {
			prevStart--
		}
		e.pos = min(prevStart+column, prevEnd)
		return true
	}
	end := e.lineEnd()
	if end == len(e.buf) {
		return false
	}
	nextStart := end + 1
	nextEnd := nextStart
	for nextEnd < len(e.buf) && e.buf[nextEnd] != '\n' {
		nextEnd++
	}
	e.pos = min(nextStart+column, nextEnd)
	return true
}

const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
	// bracketedPasteOn asks the terminal to mark pasted text, so its line breaks do not
	// submit it; bracketedPasteOff turns that off again.
	bracketedPasteOn  = "\x1b[?2004h"
	bracketedPasteOff = "\x1b[?2004l"
)

// handleKey applies one key. done is true once the text is submitted; err is io.EOF
// when Ctrl-D is pressed on an empty line.
func (e *promptEditor) handleKey(key string) (done bool, err error) {
	e.notice = ""
	switch key {
	case pasteStart:
		e.pasting = true
		e.pasteFrom = e.pos
		return false, nil
	case pasteEnd:
		e.pasting = false
		if e.images != nil && e.pasteFrom < e.pos && e.pos <= len(e.buf) {
			if label, ok := e.images.attachPasted(string(e.buf[e.pasteFrom:e.pos])); ok {
				e.deleteRange(e.pasteFrom, e.pos)
				e.insertImageLabel(label)
			}
		}
		return false, nil
	}
	if !e.pasting {
		if key == "?" && len(e.buf) == 0 && !e.showShortcuts {
			e.showShortcuts = true
			return false, nil
		}
		e.showShortcuts = false
	}
	if e.pasting {
		switch key {
		case "\r", "\n":
			e.insert("\n")
		case "\t":
			e.insert("    ")
		default:
			if runes := []rune(key); len(runes) == 1 && !unicode.IsControl(runes[0]) {
				e.insert(key)
			}
		}
		return false, nil
	}

	switch key {
	case "\r":
		// A backslash right before the cursor turns Enter into a line break.
		if e.pos > 0 && e.buf[e.pos-1] == '\\' {
			e.deleteRange(e.pos-1, e.pos)
			e.insert("\n")
			return false, nil
		}
		// In the path menu Enter takes the highlighted path. Otherwise it runs the
		// highlighted command, or the command with the highlighted argument, unless the
		// text already names a command.
		if items := e.completions(); len(items) > 0 && items[0].path {
			e.apply(items[e.selected])
			return false, nil
		}
		if items := e.completions(); len(items) > 0 {
			_, exact := findCommand(strings.ToLower(string(e.buf[1:])))
			// "/effort " with nothing after the space runs /effort itself, which asks.
			bareArgument := items[0].argument && strings.HasSuffix(string(e.buf), " ")
			if !exact && !bareArgument {
				e.setText(items[e.selected].text)
			}
		}
		e.setText(strings.TrimRight(string(e.buf), " "))
		return true, nil
	case "\n", "\x1b\r", "\x1b\n": // Ctrl-J, Alt-Enter
		e.insert("\n")
	case "\t":
		items := e.completions()
		switch {
		case len(items) == 0:
			e.completePath()
		case items[0].path:
			e.apply(items[e.selected])
		case items[0].argument:
			e.setText(items[e.selected].text)
		default:
			e.setText(items[e.selected].text + " ")
		}
	case "\x10", "\x1b[A", "\x1bOA": // Ctrl-P, Up
		if items := e.completions(); len(items) > 0 {
			e.selected = (e.selected - 1 + len(items)) % len(items)
		} else if !e.moveLine(-1) {
			e.browseHistory(-1)
		}
	case "\x0e", "\x1b[B", "\x1bOB": // Ctrl-N, Down
		if items := e.completions(); len(items) > 0 {
			e.selected = (e.selected + 1) % len(items)
		} else if !e.moveLine(1) {
			e.browseHistory(1)
		}
	case "\x1b":
		e.menuClosed = true
	case "\x7f", "\x08": // Backspace
		if e.pos > 0 {
			e.deleteRange(e.pos-1, e.pos)
		}
	case "\x04": // Ctrl-D: end of input on an empty line, otherwise delete
		if len(e.buf) == 0 {
			return true, io.EOF
		}
		if e.pos < len(e.buf) {
			e.deleteRange(e.pos, e.pos+1)
		}
	case "\x1b[3~": // Delete
		if e.pos < len(e.buf) {
			e.deleteRange(e.pos, e.pos+1)
		}
	case "\x02", "\x1b[D", "\x1bOD": // Ctrl-B, Left
		e.pos = max(e.pos-1, 0)
	case "\x06", "\x1b[C", "\x1bOC": // Ctrl-F, Right
		e.pos = min(e.pos+1, len(e.buf))
	case "\x1bb", "\x1b[1;3D", "\x1b[1;5D": // Alt-B, Alt-Left, Ctrl-Left: word left
		e.pos = e.wordStart()
	case "\x1bf", "\x1b[1;3C", "\x1b[1;5C": // Alt-F, Alt-Right, Ctrl-Right: word right
		e.pos = e.wordEnd()
	case "\x01", "\x1b[H", "\x1bOH", "\x1b[1~", "\x1b[7~": // Ctrl-A, Home
		e.pos = e.lineStart()
	case "\x05", "\x1b[F", "\x1bOF", "\x1b[4~", "\x1b[8~": // Ctrl-E, End
		e.pos = e.lineEnd()
	case "\x15": // Ctrl-U: delete to the start of the line
		e.deleteRange(e.lineStart(), e.pos)
	case "\x0b": // Ctrl-K: delete to the end of the line
		e.deleteRange(e.pos, e.lineEnd())
	case "\x17", "\x1b\x7f": // Ctrl-W, Alt-Backspace: delete the word before the cursor
		e.deleteRange(e.wordStart(), e.pos)
	case "\x07": // Ctrl-G: the agents picker; the text typed so far waits for the prompt
		return true, errOpenAgents
	case "\x16", "\x1bv": // Ctrl-V, Alt-V: attach the clipboard's image
		if e.images != nil {
			if label, err := e.images.pasteClipboard(); err != nil {
				e.notice = err.Error()
			} else {
				e.insertImageLabel(label)
			}
		}
	default:
		// Other control keys and unknown escape sequences are ignored, so they never
		// end up in the prompt.
		if runes := []rune(key); len(runes) == 1 && !unicode.IsControl(runes[0]) {
			e.insert(key)
		}
	}
	return false, nil
}

func (e *promptEditor) wordStart() int {
	start := e.pos
	for start > 0 && unicode.IsSpace(e.buf[start-1]) {
		start--
	}
	for start > 0 && !unicode.IsSpace(e.buf[start-1]) {
		start--
	}
	return start
}

func (e *promptEditor) wordEnd() int {
	end := e.pos
	for end < len(e.buf) && unicode.IsSpace(e.buf[end]) {
		end++
	}
	for end < len(e.buf) && !unicode.IsSpace(e.buf[end]) {
		end++
	}
	return end
}

// layoutRows splits the text into the rows it is drawn on. Every row starts with the
// prompt prefix or an indent as wide, and holds at most width-1 cells, so the terminal
// never wraps a row itself. It also returns the cursor's row and 0-based column.
func (e *promptEditor) layoutRows(width int) (rows []string, cursorRow, cursorCol int) {
	prefix := promptPrefix()
	prefixWidth := style.VisibleWidth(prefix)
	indent := strings.Repeat(" ", prefixWidth)
	room := max(width-1-prefixWidth, 1)

	offset := 0
	for i, line := range strings.Split(string(e.buf), "\n") {
		runes := []rune(line)
		lead := indent
		if i == 0 {
			lead = prefix
		}
		count := len(runes)/room + 1
		for r := 0; r < count; r++ {
			chunk := runes[min(r*room, len(runes)):min((r+1)*room, len(runes))]
			rows = append(rows, lead+string(chunk))
			lead = indent
		}
		if e.pos >= offset && e.pos <= offset+len(runes) {
			column := e.pos - offset
			cursorRow = len(rows) - count + column/room
			cursorCol = prefixWidth + column%room
		}
		offset += len(runes) + 1
	}
	return rows, cursorRow, cursorCol
}

// render redraws the prompt and everything under it: it moves from the cursor up to the
// first prompt row, clears to the end of the screen, prints the input, the rule and the
// menu (or the footer when there is no menu), then moves the cursor back into the
// input. Moving up after printing works at the bottom of the screen, where printing
// scrolls.
func (e *promptEditor) render(footer string, width int) string {
	width = max(width, 1)
	var b strings.Builder
	if e.cursorRow > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", e.cursorRow)
	}
	b.WriteString("\r\x1b[J")

	rows, cursorRow, cursorCol := e.layoutRows(width)
	b.WriteString(strings.Join(rows, "\n"))
	b.WriteString("\n" + renderRule(width-1))

	below := []string{footer}
	if e.notice != "" {
		below = []string{style.Muted.Render(style.TruncateToWidth(e.notice, width-1))}
	}
	if items := e.completions(); len(items) > 0 {
		below = renderCompletionMenu(items, e.selected, width)
	} else if e.showShortcuts {
		below = renderShortcuts(width)
	} else if strings.HasPrefix(strings.TrimSpace(string(e.buf)), "!") {
		below = []string{renderShellHint(width)}
	}
	for _, line := range below {
		b.WriteString("\n" + line)
	}

	e.cursorRow = cursorRow
	if up := len(rows) + len(below) - cursorRow; up > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", up)
	}
	fmt.Fprintf(&b, "\x1b[%dG", cursorCol+1)
	return b.String()
}

// clear moves from the cursor to the rule above the prompt line and clears everything
// from there down, leaving the cursor at the start of that row.
func (e *promptEditor) clear() string {
	return fmt.Sprintf("\x1b[%dA\r\x1b[J", e.cursorRow+1)
}

var (
	// errUIEvent ends editPrompt so main can deal with something that came up while the
	// prompt waited, such as a subagent asking to run a command or finishing. The text
	// typed so far is kept in input.partial for the prompt that follows.
	errUIEvent = errors.New("the prompt gave way to a subagent event")
	// errOpenAgents is Ctrl-G at the prompt: open the agents picker.
	errOpenAgents = errors.New("open the agents picker")
)

// promptEvents lets what happens elsewhere reach the idle prompt.
type promptEvents struct {
	// changed and pending receive a value when something may have changed. They hold at
	// most one value and can be stale, so they only make the prompt look again.
	changed, pending <-chan struct{}
	// interrupt reports whether the prompt should give way now; empty is whether
	// nothing is typed.
	interrupt func(empty bool) bool
	// footer draws the footer again, for counts that changed.
	footer func() string
}

// enterCbreak puts the terminal in cbreak mode; a variable so tests can edit without
// a terminal.
var enterCbreak = terminal.EnterCbreak

// editPrompt draws the prompt frame and edits one line in cbreak mode. ok is false when
// the terminal cannot be put in cbreak mode, and nothing has been drawn. On return the
// frame has been cleared. On any error, errUIEvent and Ctrl-C included, what was typed
// is kept in input.partial for the next prompt. events may be nil.
func editPrompt(ctx context.Context, input *lineReader, history []string, footer string, width int, attacher imageAttacher, events *promptEvents) (text string, ok bool, err error) {
	restore, err := enterCbreak()
	if err != nil {
		return "", false, nil
	}
	defer restore()

	// A line whose ReadLine was cancelled is where the user left off.
	editor := &promptEditor{history: history, historyIndex: len(history), images: attacher}
	editor.setText(input.partial)
	input.partial = ""

	var changed, pending <-chan struct{}
	interrupt := func() bool { return false }
	if events != nil {
		changed, pending = events.changed, events.pending
		if events.interrupt != nil {
			interrupt = func() bool { return events.interrupt(len(editor.buf) == 0) }
		}
		if events.footer != nil {
			footer = events.footer()
		}
	}
	if interrupt() {
		input.partial = string(editor.buf)
		return "", true, errUIEvent
	}

	fmt.Print(bracketedPasteOn + renderRule(width-1) + "\n" + editor.render(footer, width))
	defer fmt.Print(bracketedPasteOff)
	for {
		key, woke, err := input.readKeyOr(ctx, changed, pending)
		switch {
		case err != nil:
		case woke && interrupt():
			err = errUIEvent
		case woke:
			if events.footer != nil {
				footer = events.footer()
			}
			fmt.Print(editor.render(footer, width))
			continue
		default:
			var done bool
			done, err = editor.handleKey(key)
			if !done && editor.pasting {
				fmt.Print(editor.render(footer, width))
				continue
			}
			if !done && interrupt() {
				err = errUIEvent
			} else if !done {
				fmt.Print(editor.render(footer, width))
				continue
			}
		}
		fmt.Print(editor.clear())
		if err != nil {
			input.partial = string(editor.buf)
			return "", true, err
		}
		return string(editor.buf), true, nil
	}
}
