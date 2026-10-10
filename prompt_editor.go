package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/azs06/goat/internal/style"
)

// The interactive prompt is edited in cbreak mode so slash commands can be completed
// as they are typed. Typing "/" opens a menu of matching commands under the input in
// place of the footer: Up/Down move the selection, Tab completes it, Enter runs it and
// Esc closes the menu. Piped input and plain output never use the editor.

type completion struct {
	name, description string
}

// commandCompletions lists the commands text could be the start of: commands whose name
// starts with what follows the slash, then aliases whose command did not already match.
// Text that does not start with a slash or already has arguments gets none.
func commandCompletions(text string) []completion {
	if !strings.HasPrefix(text, "/") || strings.ContainsFunc(text, unicode.IsSpace) {
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
			matches = append(matches, completion{name: name, description: commands[name].description})
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
			matches = append(matches, completion{name: alias, description: "Same as /" + target})
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
		nameWidth = max(nameWidth, style.VisibleWidth(item.name)+1)
	}

	available := width - 1
	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		marker := "  "
		if i == selected {
			marker = style.Glyphs().Prompt + " "
		}
		head := style.TruncateToWidth(marker+style.PadRight("/"+items[i].name, nameWidth+2), available)
		description := style.TruncateToWidth(items[i].description, available-style.VisibleWidth(head))
		if i == selected {
			lines = append(lines, style.Selected.Render(head)+style.Muted.Render(description))
		} else {
			lines = append(lines, style.Muted.Render(head)+style.Subtle.Render(description))
		}
	}
	return lines
}

// promptEditor is the state of the line being typed at the interactive prompt.
type promptEditor struct {
	buf []rune
	pos int
	// selected is the highlighted menu entry; menuClosed hides the menu after Esc
	// until the text changes.
	selected   int
	menuClosed bool
	// cursorRow is the row of the cursor below the prompt line as last drawn.
	cursorRow int
}

func (e *promptEditor) completions() []completion {
	if e.menuClosed {
		return nil
	}
	return commandCompletions(string(e.buf))
}

func (e *promptEditor) setText(text string) {
	e.buf = []rune(text)
	e.pos = len(e.buf)
	e.edited()
}

func (e *promptEditor) edited() {
	e.selected = 0
	e.menuClosed = false
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

// handleKey applies one key. done is true once the line is submitted; err is io.EOF
// when Ctrl-D is pressed on an empty line.
func (e *promptEditor) handleKey(key string) (done bool, err error) {
	switch key {
	case "\n", "\r":
		// Enter runs the highlighted command unless the text already names one.
		if items := e.completions(); len(items) > 0 {
			if _, exact := findCommand(strings.ToLower(string(e.buf[1:]))); !exact {
				e.setText("/" + items[e.selected].name)
			}
		}
		return true, nil
	case "\t":
		if items := e.completions(); len(items) > 0 {
			e.setText("/" + items[e.selected].name + " ")
		}
	case "\x10", "\x1b[A", "\x1bOA": // Ctrl-P, Up
		if items := e.completions(); len(items) > 0 {
			e.selected = (e.selected - 1 + len(items)) % len(items)
		}
	case "\x0e", "\x1b[B", "\x1bOB": // Ctrl-N, Down
		if items := e.completions(); len(items) > 0 {
			e.selected = (e.selected + 1) % len(items)
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
	case "\x01", "\x1b[H", "\x1bOH", "\x1b[1~", "\x1b[7~": // Ctrl-A, Home
		e.pos = 0
	case "\x05", "\x1b[F", "\x1bOF", "\x1b[4~", "\x1b[8~": // Ctrl-E, End
		e.pos = len(e.buf)
	case "\x15": // Ctrl-U: delete to the start of the line
		e.deleteRange(0, e.pos)
	case "\x0b": // Ctrl-K: delete to the end of the line
		e.deleteRange(e.pos, len(e.buf))
	case "\x17": // Ctrl-W: delete the word before the cursor
		start := e.pos
		for start > 0 && unicode.IsSpace(e.buf[start-1]) {
			start--
		}
		for start > 0 && !unicode.IsSpace(e.buf[start-1]) {
			start--
		}
		e.deleteRange(start, e.pos)
	default:
		// Other control keys and unknown escape sequences are ignored, so they never
		// end up in the prompt.
		if runes := []rune(key); len(runes) == 1 && !unicode.IsControl(runes[0]) {
			e.insert(key)
		}
	}
	return false, nil
}

// render redraws the prompt line and everything under it: it moves from the cursor up
// to the prompt line, clears to the end of the screen, prints the input, the rule and
// the menu (or the footer when there is no menu), then moves the cursor back into the
// input. Moving up after printing works at the bottom of the screen, where printing
// scrolls.
func (e *promptEditor) render(footer string, width int) string {
	width = max(width, 1)
	var b strings.Builder
	if e.cursorRow > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", e.cursorRow)
	}
	b.WriteString("\r\x1b[J")

	prefix := promptPrefix()
	prefixWidth := style.VisibleWidth(prefix)
	b.WriteString(prefix + string(e.buf))
	cells := prefixWidth + len(e.buf)
	rows := cells/width + 1
	if cells%width == 0 {
		// The text filled its last row exactly; the cursor belongs on the empty row
		// after it.
		b.WriteString("\n")
	}
	b.WriteString("\n" + renderRule(width-1))

	below := []string{footer}
	if items := e.completions(); len(items) > 0 {
		below = renderCompletionMenu(items, e.selected, width)
	}
	for _, line := range below {
		b.WriteString("\n" + line)
	}

	cursorCells := prefixWidth + e.pos
	e.cursorRow = cursorCells / width
	if up := rows + len(below) - e.cursorRow; up > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", up)
	}
	fmt.Fprintf(&b, "\x1b[%dG", cursorCells%width+1)
	return b.String()
}

// clear moves from the cursor to the rule above the prompt line and clears everything
// from there down, leaving the cursor at the start of that row.
func (e *promptEditor) clear() string {
	return fmt.Sprintf("\x1b[%dA\r\x1b[J", e.cursorRow+1)
}

// editPrompt draws the prompt frame and edits one line in cbreak mode. ok is false when
// the terminal cannot be put in cbreak mode, and nothing has been drawn. On return the
// frame has been cleared.
func editPrompt(ctx context.Context, input *lineReader, footer string, width int) (text string, ok bool, err error) {
	restore, err := enterCbreakMode()
	if err != nil {
		return "", false, nil
	}
	defer restore()

	// A line whose ReadLine was cancelled is where the user left off.
	editor := &promptEditor{}
	editor.setText(input.partial)
	input.partial = ""

	fmt.Print(renderRule(width-1) + "\n" + editor.render(footer, width))
	for {
		key, err := input.readKey(ctx)
		if err == nil {
			var done bool
			done, err = editor.handleKey(key)
			if !done {
				fmt.Print(editor.render(footer, width))
				continue
			}
		}
		fmt.Print(editor.clear())
		if err != nil {
			return "", true, err
		}
		return string(editor.buf), true, nil
	}
}
