// Package markdown renders the model's streamed markdown replies as styled terminal
// text, highlighting fenced code with chroma.
package markdown

import (
	"regexp"
	"strings"

	"github.com/azs06/goat/internal/style"
)

// Stream renders streamed assistant text as styled markdown for a color terminal.
// It holds back only the current incomplete line, so output stays line-incremental:
// each completed line is rendered as soon as its newline arrives. Every line of the message is indented two columns and the first one carries a "●"
// bullet, like a Claude Code transcript block.
type Stream struct {
	width         int // terminal width; prose wraps at width-2
	partial       string
	state         mdState
	started       bool // the bullet row has been written
	pendingBlanks int  // blank lines held back until more content follows
}

// NewStream returns a Stream that wraps prose for a terminal width columns wide.
func NewStream(width int) *Stream {
	return &Stream{width: width}
}

// Write takes the next chunk of streamed text and returns what should be printed for
// the lines it completes.
func (m *Stream) Write(chunk string) string {
	m.partial += chunk
	var out strings.Builder
	for {
		line, rest, found := strings.Cut(m.partial, "\n")
		if !found {
			break
		}
		m.partial = rest
		out.WriteString(m.renderLine(line))
	}
	return out.String()
}

// Flush renders a final unterminated line, if any, and drops held-back blank lines.
func (m *Stream) Flush() string {
	line := m.partial
	m.partial = ""
	if line == "" {
		return ""
	}
	return m.renderLine(line)
}

func (m *Stream) renderLine(line string) string {
	line = strings.TrimSuffix(line, "\r")
	if !m.state.inFence && strings.TrimSpace(line) == "" {
		if m.started {
			m.pendingBlanks++
		}
		return ""
	}

	rendered, next := renderMarkdownLine(line, m.state, m.width-2)
	m.state = next

	var out strings.Builder
	if !m.started {
		// A message starts after a blank line, separating it from what came before.
		out.WriteString("\n")
	}
	if m.pendingBlanks > 0 {
		// Runs of blank lines collapse to one.
		out.WriteString("\n")
		m.pendingBlanks = 0
	}
	for _, row := range strings.Split(rendered, "\n") {
		prefix := "  "
		if !m.started {
			prefix = style.Glyphs().Bullet + " "
			m.started = true
		}
		if row == "" {
			out.WriteString("\n")
			continue
		}
		out.WriteString(prefix + row + "\n")
	}
	return out.String()
}

// mdState is what a markdown line needs to know about the lines before it.
type mdState struct {
	inFence  bool
	fenceSeq string // the run of ` or ~ that opened the fence
	lang     string // the fence's language tag
	code     []string
}

var (
	mdFencePattern    = regexp.MustCompile("^\\s*(`{3,}|~{3,})\\s*([^`\\s]*)")
	mdHeadingPattern  = regexp.MustCompile(`^ {0,3}(#{1,6})\s+(.*?)(?:\s+#+)?\s*$`)
	mdRulePattern     = regexp.MustCompile(`^ {0,3}(?:(?:-\s*){3,}|(?:\*\s*){3,}|(?:_\s*){3,})$`)
	mdBulletPattern   = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	mdNumberedPattern = regexp.MustCompile(`^(\s*)(\d{1,9}[.)])\s+(.*)$`)
	mdQuotePattern    = regexp.MustCompile(`^\s*>\s?(.*)$`)
)

// renderMarkdownLine renders one complete line of markdown, wrapping prose to width
// cells. It returns the rendered rows joined by newlines (no trailing newline) and the
// state for the next line. Code blocks and tables are never wrapped.
func renderMarkdownLine(line string, st mdState, width int) (string, mdState) {
	line = strings.ReplaceAll(line, "\t", "    ")
	g := style.Glyphs()

	if st.inFence {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, st.fenceSeq) && strings.Trim(trimmed, st.fenceSeq[:1]) == "" {
			return style.Subtle.Render(g.BottomLeft + g.HLine), mdState{}
		}
		st.code = append(st.code, line)
		if style.Enabled() {
			if highlighted, ok := highlightCodeLine(st.lang, st.code); ok {
				return style.Subtle.Render(g.VLine) + " " + highlighted, st
			}
		}
		return style.Subtle.Render(g.VLine) + " " + renderCodeLine(line), st
	}

	if m := mdFencePattern.FindStringSubmatch(line); m != nil {
		label := g.TopLeft + g.HLine
		if m[2] != "" {
			label += " " + m[2]
		}
		return style.Subtle.Render(label), mdState{inFence: true, fenceSeq: m[1], lang: m[2]}
	}

	if strings.HasPrefix(strings.TrimSpace(line), "|") {
		return line, st
	}

	if m := mdHeadingPattern.FindStringSubmatch(line); m != nil {
		role := style.Subheading
		if len(m[1]) <= 2 {
			role = style.Heading
		}
		return style.Wrap(styleSpan(m[2], role.Style()), width, "", ""), st
	}

	if mdRulePattern.MatchString(line) {
		return style.Subtle.Render(strings.Repeat(g.HLine, max(width, 1))), st
	}

	if m := mdBulletPattern.FindStringSubmatch(line); m != nil {
		marker := m[1] + style.Muted.Render("•") + " "
		hang := m[1] + "  "
		return style.Wrap(renderInline(m[2], style.Style{}), width, marker, hang), st
	}

	if m := mdNumberedPattern.FindStringSubmatch(line); m != nil {
		marker := m[1] + style.Muted.Render(m[2]) + " "
		hang := m[1] + strings.Repeat(" ", len(m[2])+1)
		return style.Wrap(renderInline(m[3], style.Style{}), width, marker, hang), st
	}

	if m := mdQuotePattern.FindStringSubmatch(line); m != nil {
		bar := style.Subtle.Render(g.VLine) + " "
		return style.Wrap(styleSpan(m[1], style.Quote.Style()), width, bar, bar), st
	}

	indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
	return style.Wrap(renderInline(strings.TrimLeft(line, " "), style.Style{}), width, indent, indent), st
}

// renderCodeLine tints a line inside a fenced code block: whole-line comments are
// the code-comment role, everything else the code role.
func renderCodeLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "//") || trimmed == "#" || strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "-- ") {
		return style.CodeComment.Render(line)
	}
	return style.Code.Render(line)
}

// styleSpan renders text's inline markdown inside an outer style.
func styleSpan(text string, outer style.Style) string {
	if !style.Enabled() || text == "" {
		return renderInline(text, style.Style{})
	}
	return outer.Open() + renderInline(text, outer) + style.Reset
}

// renderInline styles `code`, **bold**, __bold__, *italic* and _italic_ spans. active
// is the style of the enclosing span, which is re-opened after each inner span.
// Emphasis is conservative: a delimiter must not touch a letter or digit on its outer
// side, so snake_case_names and 2*3*4 are left alone.
func renderInline(text string, active style.Style) string {
	if !style.Enabled() {
		return text
	}
	var out strings.Builder
	for i := 0; i < len(text); {
		c := text[i]
		switch c {
		case '`':
			n := runLength(text, i, '`')
			if end := findCodeClose(text, i+n, n); end >= 0 {
				inner := text[i+n : end]
				if len(inner) > 2 && inner[0] == ' ' && inner[len(inner)-1] == ' ' {
					inner = inner[1 : len(inner)-1]
				}
				out.WriteString(active.Merge(style.InlineCode.Style()).Open() + inner + style.Reset + active.Open())
				i = end + n
				continue
			}
			out.WriteString(text[i : i+n])
			i += n
			continue
		case '[', '!':
			if label, url, end, ok := parseMarkdownLink(text, i); ok {
				out.WriteString(renderLink(label, url, c == '!', active))
				i = end
				continue
			}
		case '*', '_':
			n := runLength(text, i, c)
			if n <= 2 && (i == 0 || !isWordByte(text[i-1])) && i+n < len(text) && !isSpaceByte(text[i+n]) {
				if end := findEmphasisClose(text, i+n, c, n); end >= 0 {
					role := style.Emphasis
					if n == 2 {
						role = style.Strong
					}
					inner := active.Merge(role.Style())
					out.WriteString(inner.Open() + renderInline(text[i+n:end], inner) + style.Reset + active.Open())
					i = end + n
					continue
				}
			}
			out.WriteString(text[i : i+n])
			i += n
			continue
		}
		out.WriteByte(c)
		i++
	}
	return out.String()
}

func runLength(text string, i int, c byte) int {
	n := 0
	for i+n < len(text) && text[i+n] == c {
		n++
	}
	return n
}

// parseMarkdownLink reads [label](url) or ![alt](url) starting at i, returning the
// position after it.
func parseMarkdownLink(text string, i int) (label, url string, end int, ok bool) {
	start := i
	if text[i] == '!' {
		if i+1 >= len(text) || text[i+1] != '[' {
			return "", "", 0, false
		}
		start++
	}
	depth := 0
	closeBracket := -1
	for j := start; j < len(text); j++ {
		if text[j] == '[' {
			depth++
		} else if text[j] == ']' {
			depth--
			if depth == 0 {
				closeBracket = j
				break
			}
		}
	}
	if closeBracket < 0 || closeBracket+1 >= len(text) || text[closeBracket+1] != '(' {
		return "", "", 0, false
	}
	closeParen := strings.IndexByte(text[closeBracket+2:], ')')
	if closeParen < 0 {
		return "", "", 0, false
	}
	url = strings.TrimSpace(text[closeBracket+2 : closeBracket+2+closeParen])
	if url == "" || strings.ContainsAny(url, " \t") {
		return "", "", 0, false
	}
	return text[start+1 : closeBracket], url, closeBracket + 3 + closeParen, true
}

// renderLink draws a link as its underlined label, clickable in terminals that support
// OSC 8, followed by the address when the label hides it. Images cannot be shown
// inline, so they become a labelled placeholder linking to the image.
func renderLink(label, url string, image bool, active style.Style) string {
	underline := active.Merge(style.Style{Underline: true})
	if image {
		if label == "" {
			label = "image"
		}
		return style.Muted.Render("[image: ") + style.Hyperlink(underline.Open()+label+style.Reset, url) + style.Muted.Render("]") + active.Open()
	}
	text := underline.Open() + renderInline(label, underline) + style.Reset + active.Open()
	linked := style.Hyperlink(text, url)
	if label != url && !strings.HasPrefix(url, "#") {
		linked += style.Subtle.Render(" ("+url+")") + active.Open()
	}
	return linked
}

// findCodeClose finds a backtick run of exactly n starting at or after from.
func findCodeClose(text string, from, n int) int {
	for j := from; j < len(text); {
		if text[j] != '`' {
			j++
			continue
		}
		run := runLength(text, j, '`')
		if run == n {
			return j
		}
		j += run
	}
	return -1
}

// findEmphasisClose finds a closing run of exactly n c's that follows a non-space and
// is not followed by a letter or digit. Code spans are skipped.
func findEmphasisClose(text string, from int, c byte, n int) int {
	for j := from; j < len(text); {
		switch text[j] {
		case '`':
			run := runLength(text, j, '`')
			if end := findCodeClose(text, j+run, run); end >= 0 {
				j = end + run
				continue
			}
			j += run
		case c:
			run := runLength(text, j, c)
			after := j + run
			if run == n && j > from && !isSpaceByte(text[j-1]) && (after == len(text) || !isWordByte(text[after])) {
				return j
			}
			j += run
		default:
			j++
		}
	}
	return -1
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b >= 0x80
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t'
}
