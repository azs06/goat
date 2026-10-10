package main

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/azs06/goat/internal/provider"
)

// The application chrome: the welcome banner, the prompt frame and footer, the
// run_bash approval card and command listings. Everything here returns strings, so it
// can be tested; callers decide where they go. Plain mode (colorEnabled false) gets
// plain text with no escapes.

const maxBannerWidth = 70

// shortenHome replaces the home directory prefix of path with "~".
func shortenHome(path, home string) string {
	if home == "" || home == "/" {
		return path
	}
	home = strings.TrimSuffix(home, string(filepath.Separator))
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}

// renderRule is a dim horizontal line width cells wide.
func renderRule(width int) string {
	if width < 1 {
		width = 1
	}
	return styleSubtle(strings.Repeat(glyphs().hline, width))
}

// renderBanner is the welcome box shown at startup. In plain mode it is the two lines
// goat always printed.
func renderBanner(model, effort, cwd string, width int) string {
	if !colorEnabled {
		return fmt.Sprintf("Welcome to G.O.A.T agent\nModel: %s (effort %s). Type /help for commands.", model, effort)
	}
	g := glyphs()
	if width > maxBannerWidth {
		width = maxBannerWidth
	}
	if width < 20 {
		width = 20
	}
	inner := width - 4 // "│ " and " │"

	row := func(content string) string {
		return styleAccent(g.vline) + " " + padRight(content, inner) + " " + styleAccent(g.vline)
	}
	dimRow := func(text string) string { return row(styleDim(truncateToWidth(text, inner))) }

	title := truncateToWidth("✻ Welcome to G.O.A.T", inner)
	lines := []string{
		styleAccent(g.topLeft + strings.Repeat(g.hline, width-2) + g.topRight),
		row(paint(title, sgrBold, fgAccent)),
		row(""),
		dimRow("model  " + model + " " + g.dot + " effort " + effort),
		dimRow("cwd    " + cwd),
		row(""),
		dimRow("/help for commands " + g.dot + " ctrl-c interrupts a turn"),
		styleAccent(g.bottomLeft + strings.Repeat(g.hline, width-2) + g.bottomRight),
	}
	return strings.Join(lines, "\n")
}

// renderFooter joins the status parts with dots into one dim line that fits in width-1
// cells; the last column is left free so the cursor never wraps.
func renderFooter(parts []string, width int) string {
	line := "  " + strings.Join(parts, " "+glyphs().dot+" ")
	return styleSubtle(truncateToWidth(line, width-1))
}

// formatTokenCount renders a token count as "850" or "12k".
func formatTokenCount(tokens int64) string {
	if tokens < 1000 {
		return fmt.Sprintf("%d", tokens)
	}
	return fmt.Sprintf("%dk", int64(math.Round(float64(tokens)/1000)))
}

// promptPrefix is what the interactive prompt line starts with.
func promptPrefix() string {
	return styleAccent(glyphs().prompt) + " "
}

// renderPromptFrame draws the rule, the empty prompt line, a second rule and the
// footer, then moves the cursor back up to the end of the prompt prefix. The footer
// is printed first and the cursor moved afterwards so it works at the bottom of the
// screen, where printing scrolls.
func renderPromptFrame(footer string, width int) string {
	col := visibleWidth(promptPrefix()) + 1
	return renderRule(width-1) + "\n" + promptPrefix() + "\n" + renderRule(width-1) + "\n" + footer +
		fmt.Sprintf("\x1b[2A\x1b[%dG", col)
}

// inputRows is how many terminal rows prefix and the typed text took up.
func inputRows(prefixWidth int, text string, width int) int {
	if width < 1 {
		width = 1
	}
	cells := prefixWidth + echoedWidth(text)
	rows := (cells + width - 1) / width
	if rows < 1 {
		rows = 1
	}
	return rows
}

// echoedWidth is how many cells the terminal used to echo text as it was typed. A
// control character such as the ESC an arrow key sends echoes as "^[", two cells.
func echoedWidth(text string) int {
	cells := 0
	for _, r := range text {
		if r != '\t' && unicode.IsControl(r) {
			cells += 2
		} else {
			cells++
		}
	}
	return cells
}

// renderPromptCleanup moves the cursor from just below the echoed input back up to the
// rule above the prompt line and clears everything from there down: the rule, the
// input, and the rule and footer under it.
func renderPromptCleanup(text string, width int) string {
	rows := inputRows(visibleWidth(promptPrefix()), text, width)
	return fmt.Sprintf("\x1b[%dA\r\x1b[J", rows+1)
}

// renderUserMessage is the submitted prompt as it stays in the transcript. Control
// characters (an arrow key typed at the prompt sends ESC sequences) are made visible
// so re-showing the line cannot move the cursor.
func renderUserMessage(text string, width int) string {
	text = escapeControlChars(text)
	if !colorEnabled {
		return glyphs().prompt + " " + text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		prefix := "  "
		if i == 0 {
			prefix = glyphs().prompt + " "
		}
		// The highlighted pad space is dropped when the line already ends exactly at
		// the right edge, where it would wrap onto a row of its own.
		pad := " "
		if width > 0 && (visibleWidth(prefix)+visibleWidth(line))%width == 0 {
			pad = ""
		}
		lines[i] = paint(prefix, fgAccent, bgPrompt) + paint(line+pad, sgrBold, bgPrompt)
	}
	return strings.Join(lines, "\n")
}

// renderWorked is the summary under a finished turn.
func renderWorked(elapsed time.Duration) string {
	return styleSubtle("✻ Worked for " + formatElapsed(elapsed.Truncate(time.Second)))
}

// renderApprovalCard is the framed run_bash approval question, ending with the input
// prompt. Only used in color mode; plain mode keeps its original text. Command lines
// are hard-wrapped, never cut, to fit width so every row keeps the bar and the whole
// command is visible before it is approved.
func renderApprovalCard(command, workdir string, width int) string {
	g := glyphs()
	bar := styleWarn(g.vline) + " "
	room := width - 3 // the bar, its space, and the last column left free
	if room < 10 {
		room = 10
	}
	var b strings.Builder
	title := styleWarn(g.topLeft+g.hline+" ") + paint("Run command?", sgrBold, fgYellow)
	where := "in " + escapeControlChars(workdir)
	if visibleWidth(title)+2+visibleWidth(where) <= room+2 {
		b.WriteString("\n" + title + "  " + styleDim(where) + "\n")
	} else {
		b.WriteString("\n" + title + "\n")
		for _, row := range hardWrap(where, room) {
			b.WriteString(bar + styleDim(row) + "\n")
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(escapeControlChars(command), "\t", "    "), "\n") {
		for _, row := range hardWrap(line, room) {
			b.WriteString(bar + row + "\n")
		}
	}
	b.WriteString(renderApprovalPrompt())
	return b.String()
}

// renderApprovalPrompt is the last row of the approval card, where the answer is typed.
func renderApprovalPrompt() string {
	g := glyphs()
	return styleWarn(g.bottomLeft+g.hline+" ") +
		styleAccent("[y]") + styleDim("es "+g.dot+" ") +
		styleAccent("[n]") + styleDim("o "+g.dot+" ") +
		styleAccent("[a]") + styleDim("lways "+g.dot+" or type feedback ") +
		styleAccent(g.prompt) + " "
}

// hardWrap splits plain text into rows of at most width runes.
func hardWrap(text string, width int) []string {
	runes := []rune(text)
	if width < 1 || len(runes) <= width {
		return []string{text}
	}
	var rows []string
	for len(runes) > width {
		rows = append(rows, string(runes[:width]))
		runes = runes[width:]
	}
	return append(rows, string(runes))
}

// renderHelpTable lists the commands in two aligned columns, sorted by name.
func renderHelpTable(cmds map[string]*cliCommand) string {
	names := make([]string, 0, len(cmds))
	for name := range cmds {
		names = append(names, name)
	}
	sort.Strings(names)

	lines := make([]string, 0, len(names))
	for _, name := range names {
		cmd := cmds[name]
		line := "  " + padRight(styleAccent(cmd.usage), 28) + " " + styleDim(cmd.description)
		if len(cmd.aliases) > 0 {
			aliases := make([]string, len(cmd.aliases))
			for i, alias := range cmd.aliases {
				aliases[i] = "/" + alias
			}
			line += styleSubtle(" (also " + strings.Join(aliases, ", ") + ")")
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// renderKeyValues aligns rows of key and value; keys are dim.
func renderKeyValues(rows [][2]string) string {
	width := 0
	for _, row := range rows {
		if w := visibleWidth(row[0]); w > width {
			width = w
		}
	}
	lines := make([]string, len(rows))
	for i, row := range rows {
		lines[i] = padRight(styleDim(row[0]), width) + " " + row[1]
	}
	return strings.Join(lines, "\n")
}

// renderModelChoices numbers the models, marking the current one.
func renderModelChoices(choices []provider.Model, current provider.Model) string {
	lines := make([]string, len(choices))
	for i, choice := range choices {
		number := styleSubtle(fmt.Sprintf("%2d.", i+1))
		if choice == current {
			marker := "*"
			if colorEnabled {
				marker = paint(glyphs().prompt, sgrBold, fgAccent)
			}
			lines[i] = marker + " " + number + " " + paint(choice.String(), sgrBold, fgAccent)
			continue
		}
		lines[i] = "  " + number + " " + choice.String()
	}
	return strings.Join(lines, "\n")
}
