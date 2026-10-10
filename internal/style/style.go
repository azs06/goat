package style

import "strings"

// Reset is the escape that turns every style off.
const Reset = "\x1b[0m"

// Style is a compiled set of text attributes.
type Style struct {
	Fg, Bg                                 Color
	Bold, Faint, Italic, Underline, Strike bool
}

// IsZero reports whether the style changes nothing.
func (s Style) IsZero() bool { return s == Style{} }

// Merge returns s with inner on top: inner's colors replace s's where inner sets them,
// and attributes either one turns on stay on. It is how a span nested inside another
// styled span is drawn.
func (s Style) Merge(inner Style) Style {
	if inner.Fg.IsSet() {
		s.Fg = inner.Fg
	}
	if inner.Bg.IsSet() {
		s.Bg = inner.Bg
	}
	s.Bold = s.Bold || inner.Bold
	s.Faint = s.Faint || inner.Faint
	s.Italic = s.Italic || inner.Italic
	s.Underline = s.Underline || inner.Underline
	s.Strike = s.Strike || inner.Strike
	return s
}

// Open is the escape that turns the style on, or "" when styling is off or the style
// changes nothing.
func (s Style) Open() string {
	if !enabled || s.IsZero() {
		return ""
	}
	var codes []string
	for _, attr := range []struct {
		on   bool
		code string
	}{{s.Bold, "1"}, {s.Faint, "2"}, {s.Italic, "3"}, {s.Underline, "4"}, {s.Strike, "9"}} {
		if attr.on {
			codes = append(codes, attr.code)
		}
	}
	if s.Fg.IsSet() {
		codes = append(codes, s.Fg.sgr(38))
	}
	if s.Bg.IsSet() {
		codes = append(codes, s.Bg.sgr(48))
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

// Render wraps text in the style. Each line is wrapped separately so a style never
// bleeds across a newline into the next prompt or output.
func (s Style) Render(text string) string {
	open := s.Open()
	if open == "" || text == "" {
		return text
	}
	if !strings.Contains(text, "\n") {
		return open + text + Reset
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = open + line + Reset
		}
	}
	return strings.Join(lines, "\n")
}
