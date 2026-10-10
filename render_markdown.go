package main

import (
	"regexp"
	"strings"
)

// markdownStream renders streamed assistant text as styled markdown for a color
// terminal. It holds back only the current incomplete line, so output stays
// line-incremental: each completed line is rendered as soon as its newline arrives.
// Every line of the message is indented two columns and the first one carries a "●"
// bullet, like a Claude Code transcript block.
type markdownStream struct {
	width         int // terminal width; prose wraps at width-2
	partial       string
	state         mdState
	started       bool // the bullet row has been written
	pendingBlanks int  // blank lines held back until more content follows
}

func newMarkdownStream(width int) *markdownStream {
	return &markdownStream{width: width}
}

// Write takes the next chunk of streamed text and returns what should be printed for
// the lines it completes.
func (m *markdownStream) Write(chunk string) string {
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
func (m *markdownStream) Flush() string {
	line := m.partial
	m.partial = ""
	if line == "" {
		return ""
	}
	return m.renderLine(line)
}

func (m *markdownStream) renderLine(line string) string {
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
			prefix = glyphs().bullet + " "
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
	g := glyphs()

	if st.inFence {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, st.fenceSeq) && strings.Trim(trimmed, st.fenceSeq[:1]) == "" {
			return paint(g.bottomLeft+g.hline, fgSubtle), mdState{}
		}
		return paint(g.vline, fgSubtle) + " " + renderCodeLine(line), st
	}

	if m := mdFencePattern.FindStringSubmatch(line); m != nil {
		label := g.topLeft + g.hline
		if m[2] != "" {
			label += " " + m[2]
		}
		return paint(label, fgSubtle), mdState{inFence: true, fenceSeq: m[1]}
	}

	if strings.HasPrefix(strings.TrimSpace(line), "|") {
		return line, st
	}

	if m := mdHeadingPattern.FindStringSubmatch(line); m != nil {
		codes := []string{sgrBold}
		if len(m[1]) <= 2 {
			codes = append(codes, fgAccent)
		}
		return wrapStyled(styleSpan(m[2], codes), width, "", ""), st
	}

	if mdRulePattern.MatchString(line) {
		return paint(strings.Repeat(g.hline, max(width, 1)), fgSubtle), st
	}

	if m := mdBulletPattern.FindStringSubmatch(line); m != nil {
		marker := m[1] + styleDim("•") + " "
		hang := m[1] + "  "
		return wrapStyled(renderInline(m[2], nil), width, marker, hang), st
	}

	if m := mdNumberedPattern.FindStringSubmatch(line); m != nil {
		marker := m[1] + styleDim(m[2]) + " "
		hang := m[1] + strings.Repeat(" ", len(m[2])+1)
		return wrapStyled(renderInline(m[3], nil), width, marker, hang), st
	}

	if m := mdQuotePattern.FindStringSubmatch(line); m != nil {
		bar := paint(g.vline, fgSubtle) + " "
		return wrapStyled(styleSpan(m[1], []string{fgMuted, sgrItalic}), width, bar, bar), st
	}

	indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
	return wrapStyled(renderInline(strings.TrimLeft(line, " "), nil), width, indent, indent), st
}

// renderCodeLine tints a line inside a fenced code block: whole-line comments are
// subtle, everything else is cyan.
func renderCodeLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "//") || trimmed == "#" || strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "-- ") {
		return paint(line, fgSubtle)
	}
	return paint(line, fgCyan)
}

// styleSpan renders text's inline markdown inside an outer style.
func styleSpan(text string, codes []string) string {
	if !colorEnabled || text == "" {
		return renderInline(text, nil)
	}
	return sgrOpen(codes) + renderInline(text, codes) + sgrReset
}

// renderInline styles `code`, **bold**, __bold__, *italic* and _italic_ spans. active
// holds the codes of the enclosing style, which are re-opened after each inner span.
// Emphasis is conservative: a delimiter must not touch a letter or digit on its outer
// side, so snake_case_names and 2*3*4 are left alone.
func renderInline(text string, active []string) string {
	if !colorEnabled {
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
				out.WriteString(sgrOpen(append(cloneCodes(active), fgMagenta)) + inner + sgrReset + sgrOpen(active))
				i = end + n
				continue
			}
			out.WriteString(text[i : i+n])
			i += n
			continue
		case '*', '_':
			n := runLength(text, i, c)
			if n <= 2 && (i == 0 || !isWordByte(text[i-1])) && i+n < len(text) && !isSpaceByte(text[i+n]) {
				if end := findEmphasisClose(text, i+n, c, n); end >= 0 {
					code := sgrItalic
					if n == 2 {
						code = sgrBold
					}
					inner := append(cloneCodes(active), code)
					out.WriteString(sgrOpen(inner) + renderInline(text[i+n:end], inner) + sgrReset + sgrOpen(active))
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

func cloneCodes(codes []string) []string {
	return append([]string(nil), codes...)
}

// wrapStyled word-wraps styled text to width visible cells. The first row starts with
// first and the rest with rest. A style open at a row break is closed at the end of
// the row and re-opened after the next row's prefix, so it never colors the indent.
// Words longer than a row are left to overflow.
func wrapStyled(text string, width int, first, rest string) string {
	words := strings.Split(text, " ")
	var rows []string
	row := first
	rowWidth := visibleWidth(first)
	empty := true
	active := ""
	for _, word := range words {
		wordWidth := visibleWidth(word)
		switch {
		case empty:
			row += word
			rowWidth += wordWidth
			empty = false
		case rowWidth+1+wordWidth > width && wordWidth > 0:
			if active != "" {
				row += sgrReset
			}
			rows = append(rows, row)
			row = rest + active + word
			rowWidth = visibleWidth(rest) + wordWidth
		default:
			row += " " + word
			rowWidth += 1 + wordWidth
		}
		active = trackSGR(active, word)
	}
	rows = append(rows, row)
	return strings.Join(rows, "\n")
}

// trackSGR returns the SGR escapes still in effect after text, given those in effect
// before it: a reset clears them, any other SGR adds to them. Other CSI sequences are
// not styles and are never replayed.
func trackSGR(active, text string) string {
	for _, esc := range ansiPattern.FindAllString(text, -1) {
		if !strings.HasSuffix(esc, "m") {
			continue
		}
		if esc == sgrReset {
			active = ""
		} else {
			active += esc
		}
	}
	return active
}
