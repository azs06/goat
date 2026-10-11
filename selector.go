package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/terminal"
)

// selectOption is one entry of an interactive list.
type selectOption struct {
	label string
	// hint is shown dimmed after the label.
	hint string
	// group, when it differs from the previous entry's, is shown as a heading above it.
	group string
	// current marks the value in use now.
	current bool
	// disabled options are listed but cannot be picked; the cursor passes over them.
	disabled bool
}

// labelText is the label as listed, with the marker of the value in use.
func (o selectOption) labelText() string {
	if o.current {
		return o.label + " " + style.Glyphs().Dot + " current"
	}
	return o.label
}

// selector is the state of an interactive list: Up/Down move, typing filters, Enter
// picks and Esc cancels.
type selector struct {
	title    string
	options  []selectOption
	filter   []rune
	selected int // index into visible()
	// rows is how many lines the list took up when last drawn.
	rows int
}

const maxSelectorRows = 10

// visible returns the indexes of the options matching the filter.
func (s *selector) visible() []int {
	query := strings.ToLower(string(s.filter))
	var shown []int
	for i, option := range s.options {
		if query == "" || strings.Contains(strings.ToLower(option.label+" "+option.group+" "+option.hint), query) {
			shown = append(shown, i)
		}
	}
	return shown
}

// handleKey applies one key. done is true when the user picked or cancelled; chosen is
// -1 on cancel.
func (s *selector) handleKey(key string) (done bool, chosen int) {
	shown := s.visible()
	switch key {
	case "\r", "\n":
		if len(shown) == 0 || s.options[shown[s.selected]].disabled {
			return false, -1
		}
		return true, shown[s.selected]
	case "\x1b", "\x03", "\x04":
		return true, -1
	case "\x10", "\x1b[A", "\x1bOA":
		s.move(shown, s.selected, -1)
	case "\x0e", "\x1b[B", "\x1bOB", "\t":
		s.move(shown, s.selected, 1)
	case "\x1b[H", "\x1bOH", "\x1b[1~", "\x1b[5~": // Home, Page Up
		s.move(shown, -1, 1)
	case "\x1b[F", "\x1bOF", "\x1b[4~", "\x1b[6~": // End, Page Down
		s.move(shown, len(shown), -1)
	case "\x7f", "\x08":
		if len(s.filter) > 0 {
			s.filter = s.filter[:len(s.filter)-1]
			s.move(s.visible(), -1, 1)
		}
	case "\x15":
		s.filter = nil
		s.move(s.visible(), -1, 1)
	default:
		if runes := []rune(key); len(runes) == 1 && !unicode.IsControl(runes[0]) {
			s.filter = append(s.filter, runes[0])
			s.move(s.visible(), -1, 1)
		}
	}
	return false, -1
}

// move selects the next option that can be picked from position from, a step at a time
// in direction step, wrapping around. With none that can be picked it selects the first.
func (s *selector) move(shown []int, from, step int) {
	for i := 1; i <= len(shown); i++ {
		next := ((from+step*i)%len(shown) + len(shown)) % len(shown)
		if !s.options[shown[next]].disabled {
			s.selected = next
			return
		}
	}
	s.selected = 0
}

// render draws the list, replacing the previous drawing. The cursor ends on the last
// row, so the next render or clear can move up from there.
func (s *selector) render(width int) string {
	var lines []string
	title := style.Strong.Render(s.title)
	if len(s.filter) > 0 {
		title += style.Muted.Render("  filter: ") + string(s.filter)
	}
	lines = append(lines, title)

	shown := s.visible()
	if len(shown) == 0 {
		lines = append(lines, style.Muted.Render("  nothing matches"))
	}
	start := 0
	if s.selected >= maxSelectorRows {
		start = s.selected - maxSelectorRows + 1
	}
	end := min(len(shown), start+maxSelectorRows)
	if start > 0 {
		lines = append(lines, style.Subtle.Render(fmt.Sprintf("  ↑ %d more", start)))
	}
	group := ""
	if start > 0 {
		group = s.options[shown[start-1]].group
	}
	available := max(width-1, 20)
	labelWidth := 0
	for _, index := range shown[start:end] {
		labelWidth = max(labelWidth, style.VisibleWidth(s.options[index].labelText()))
	}
	for i := start; i < end; i++ {
		option := s.options[shown[i]]
		if option.group != "" && option.group != group {
			lines = append(lines, style.Subtle.Render("  "+option.group))
		}
		group = option.group
		marker := "  "
		if i == s.selected {
			marker = style.Glyphs().Prompt + " "
		}
		head := marker + style.PadRight(option.labelText(), labelWidth)
		hint := ""
		if option.hint != "" {
			hint = style.TruncateToWidth("  "+option.hint, available-style.VisibleWidth(head))
		}
		head = style.TruncateToWidth(head, available)
		if option.disabled {
			lines = append(lines, style.Subtle.Render(head+hint))
			continue
		}
		if i == s.selected {
			lines = append(lines, style.Selected.Render(head)+style.Muted.Render(hint))
		} else {
			lines = append(lines, head+style.Subtle.Render(hint))
		}
	}
	if end < len(shown) {
		lines = append(lines, style.Subtle.Render(fmt.Sprintf("  ↓ %d more", len(shown)-end)))
	}
	lines = append(lines, style.Subtle.Render(style.TruncateToWidth("  ↑↓ move · enter select · esc cancel · type to filter", available)))

	var b strings.Builder
	b.WriteString(s.clear())
	b.WriteString(strings.Join(lines, "\n"))
	s.rows = len(lines)
	return b.String()
}

// clear removes the list from the screen, leaving the cursor where it started.
func (s *selector) clear() string {
	if s.rows == 0 {
		return "\r\x1b[J"
	}
	return fmt.Sprintf("\x1b[%dA\r\x1b[J", s.rows-1)
}

var errSelectorUnavailable = errors.New("interactive selection needs a color terminal")

// selectOptionInteractive shows options and returns the index picked, or -1 when the
// user cancelled. It needs an interactive color terminal; otherwise it returns
// errSelectorUnavailable and the caller falls back to a typed answer.
func selectOptionInteractive(ctx context.Context, c *config, title string, options []selectOption) (int, error) {
	if c.input == nil || !c.interactive || !style.Enabled() {
		return -1, errSelectorUnavailable
	}
	resume := suspendInputGuard()
	defer resume()
	restore, err := terminal.EnterCbreak()
	if err != nil {
		return -1, errSelectorUnavailable
	}
	defer restore()

	s := &selector{title: title, options: options}
	s.move(s.visible(), -1, 1)
	for i, option := range options {
		if option.current && !option.disabled {
			s.selected = i
		}
	}
	width := terminalWidth()
	fmt.Print(s.render(width))
	for {
		key, err := c.input.readKey(ctx)
		if err != nil {
			fmt.Print(s.clear())
			return -1, err
		}
		done, chosen := s.handleKey(key)
		if done {
			fmt.Print(s.clear())
			return chosen, nil
		}
		fmt.Print(s.render(width))
	}
}
