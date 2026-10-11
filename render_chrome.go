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
	"github.com/azs06/goat/internal/tools"

	"github.com/azs06/goat/internal/style"
)

// The application chrome: the welcome banner, the prompt frame and footer, the
// run_bash approval card and command listings. Everything here returns strings, so it
// can be tested; callers decide where they go. Plain mode (style.Enabled() false) gets
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
	return style.Subtle.Render(strings.Repeat(style.Glyphs().HLine, width))
}

// renderBanner is the welcome box shown at startup. In plain mode it is the two lines
// goat always printed.
func renderBanner(model, effort, cwd, git string, width int) string {
	if !style.Enabled() {
		return fmt.Sprintf("Welcome to G.O.A.T 🐐 agent\nModel: %s (effort %s). Type /help for commands.", model, effort)
	}
	g := style.Glyphs()
	if width > maxBannerWidth {
		width = maxBannerWidth
	}
	if width < 20 {
		width = 20
	}
	inner := width - 4 // "│ " and " │"

	row := func(content string) string {
		return style.Accent.Render(g.VLine) + " " + style.PadRight(content, inner) + " " + style.Accent.Render(g.VLine)
	}
	dimRow := func(text string) string { return row(style.Muted.Render(style.TruncateToWidth(text, inner))) }

	title := style.TruncateToWidth("✻ Welcome to G.O.A.T 🐐", inner)
	lines := []string{
		style.Accent.Render(g.TopLeft + strings.Repeat(g.HLine, width-2) + g.TopRight),
		row(style.Title.Render(title)),
		row(""),
		dimRow("model  " + modelEffortText(model, effort)),
		dimRow("cwd    " + cwd),
	}
	if git != "" {
		lines = append(lines, dimRow("git    "+git))
	}
	lines = append(lines,
		row(""),
		dimRow("/ commands "+g.Dot+" ? shortcuts "+g.Dot+" tab completes paths "+g.Dot+" ctrl-c interrupts"),
		style.Accent.Render(g.BottomLeft+strings.Repeat(g.HLine, width-2)+g.BottomRight),
	)
	return strings.Join(lines, "\n")
}

// modelEffortText is the banner's model row: "anthropic:claude-opus-5-5 · effort high".
func modelEffortText(model, effort string) string {
	return model + " " + style.Glyphs().Dot + " effort " + effort
}

// renderModelLine restates the banner's model row after the model or effort changes,
// since the banner itself stays in the scrollback as it was printed. It is for color
// mode, where there is a banner box to echo.
func renderModelLine(model, effort string, width int) string {
	return style.Muted.Render(style.TruncateToWidth("  model  "+modelEffortText(model, effort), width-1))
}

// renderFooter joins the status parts with dots into one dim line that fits in width-1
// cells; the last column is left free so the cursor never wraps.
// renderFooter joins the status parts under the prompt. Parts are in order of
// importance: when they do not fit, the last ones are left out rather than cut.
func renderFooter(parts []string, width int) string {
	join := func(parts []string) string { return "  " + strings.Join(parts, " "+style.Glyphs().Dot+" ") }
	for len(parts) > 1 && style.VisibleWidth(join(parts)) > width-1 {
		parts = parts[:len(parts)-1]
	}
	return style.Subtle.Render(style.TruncateToWidth(join(parts), width-1))
}

// shortcut is one key binding shown by "?" and /help.
type shortcut struct{ keys, action string }

var promptShortcuts = []shortcut{
	{"/", "commands"},
	{"tab", "complete command or path"},
	{"↑ ↓", "history, or lines"},
	{"ctrl-j", "new line (or \\ enter)"},
	{"esc", "close a menu"},
	{"ctrl-c", "interrupt; twice to exit"},
	{"alt-← →", "move by word"},
	{"ctrl-u k w", "delete line start, end, word"},
	{"enter", "while goat works: queue"},
	{"/output", "full tool output"},
}

// renderShortcuts lays the shortcuts out in as many columns as fit in width.
func renderShortcuts(width int) []string {
	keyWidth, cell := 0, 0
	for _, s := range promptShortcuts {
		keyWidth = max(keyWidth, style.VisibleWidth(s.keys))
	}
	for _, s := range promptShortcuts {
		cell = max(cell, keyWidth+2+style.VisibleWidth(s.action))
	}
	columns := max(1, (width-3)/(cell+3))
	rows := (len(promptShortcuts) + columns - 1) / columns
	lines := make([]string, rows)
	for i, s := range promptShortcuts {
		row, column := i%rows, i/rows
		entry := style.Muted.Render(style.PadRight(s.keys, keyWidth)) + "  " + style.Subtle.Render(s.action)
		if column < columns-1 && i+rows < len(promptShortcuts) {
			entry = style.PadRight(entry, cell+3)
		}
		lines[row] += entry
	}
	for i := range lines {
		lines[i] = "  " + lines[i]
	}
	return lines
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
	return style.Prompt.Render(style.Glyphs().Prompt) + " "
}

// renderPromptFrame draws the rule, the empty prompt line, a second rule and the
// footer, then moves the cursor back up to the end of the prompt prefix. The footer
// is printed first and the cursor moved afterwards so it works at the bottom of the
// screen, where printing scrolls.
func renderPromptFrame(footer string, width int) string {
	col := style.VisibleWidth(promptPrefix()) + 1
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
	rows := inputRows(style.VisibleWidth(promptPrefix()), text, width)
	return fmt.Sprintf("\x1b[%dA\r\x1b[J", rows+1)
}

// renderUserMessage is the submitted prompt as it stays in the transcript. Control
// characters (an arrow key typed at the prompt sends ESC sequences) are made visible
// so re-showing the line cannot move the cursor.
func renderUserMessage(text string, width int) string {
	text = escapeControlChars(text)
	if !style.Enabled() {
		return style.Glyphs().Prompt + " " + text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		prefix := "  "
		if i == 0 {
			prefix = style.Glyphs().Prompt + " "
		}
		// The highlighted pad space is dropped when the line already ends exactly at
		// the right edge, where it would wrap onto a row of its own.
		pad := " "
		if width > 0 && (style.VisibleWidth(prefix)+style.VisibleWidth(line))%width == 0 {
			pad = ""
		}
		lines[i] = style.UserMarker.Render(prefix) + style.UserMessage.Render(line+pad)
	}
	return strings.Join(lines, "\n")
}

// renderWorked is the summary under a finished turn; details, such as its cost, follow
// the time.
func renderWorked(elapsed time.Duration, details ...string) string {
	line := "✻ Worked for " + formatElapsed(elapsed.Truncate(time.Second))
	for _, detail := range details {
		if detail != "" {
			line += " " + style.Glyphs().Dot + " " + detail
		}
	}
	return style.Subtle.Render(line)
}

// renderApprovalCard is the framed run_bash approval question, ending with the input
// prompt. Only used in color mode; plain mode keeps its original text. Command lines
// are hard-wrapped, never cut, to fit width so every row keeps the bar and the whole
// command is visible before it is approved.
func renderApprovalCard(command, workdir string, width int) string {
	g := style.Glyphs()
	bar := style.Warning.Render(g.VLine) + " "
	room := width - 3 // the bar, its space, and the last column left free
	if room < 10 {
		room = 10
	}
	var b strings.Builder
	title := style.Warning.Render(g.TopLeft+g.HLine+" ") + style.ApprovalTitle.Render("Run command?")
	where := "in " + escapeControlChars(workdir)
	if style.VisibleWidth(title)+2+style.VisibleWidth(where) <= room+2 {
		b.WriteString("\n" + title + "  " + style.Muted.Render(where) + "\n")
	} else {
		b.WriteString("\n" + title + "\n")
		for _, row := range hardWrap(where, room) {
			b.WriteString(bar + style.Muted.Render(row) + "\n")
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
	g := style.Glyphs()
	return style.Warning.Render(g.BottomLeft+g.HLine+" ") +
		style.Accent.Render("[y]") + style.Muted.Render("es "+g.Dot+" ") +
		style.Accent.Render("[n]") + style.Muted.Render("o "+g.Dot+" ") +
		style.Accent.Render("[a]") + style.Muted.Render("lways "+g.Dot+" or type feedback ") +
		style.Accent.Render(g.Prompt) + " "
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
// helpGroups orders the commands /help lists under each heading. Commands missing
// here are listed under "Other".
var helpGroups = []struct {
	title string
	names []string
}{
	{"Conversation", []string{"clear", "resume", "retry", "compact", "copy", "export"}},
	{"Model", []string{"model", "effort", "status"}},
	{"Workspace", []string{"plan", "codemode", "diff", "output", "undo", "ps", "approve", "tools"}},
	{"goat", []string{"theme", "history", "help", "exit"}},
}

func renderHelpTable(cmds map[string]*cliCommand) string {
	usageWidth := 0
	for _, cmd := range cmds {
		usageWidth = max(usageWidth, style.VisibleWidth(cmd.usage))
	}
	indent := strings.Repeat(" ", 2+usageWidth+2)
	row := func(cmd *cliCommand) string {
		description := cmd.description
		if len(cmd.aliases) > 0 {
			aliases := make([]string, len(cmd.aliases))
			for i, alias := range cmd.aliases {
				aliases[i] = "/" + alias
			}
			description += " (also " + strings.Join(aliases, ", ") + ")"
		}
		lead := "  " + style.PadRight(style.Accent.Render(cmd.usage), usageWidth) + "  "
		if !style.Enabled() {
			return lead + description
		}
		return style.Wrap(style.Muted.Render(description), terminalWidth()-1, lead, indent)
	}

	var lines []string
	listed := map[string]bool{}
	for _, group := range helpGroups {
		var rows []string
		for _, name := range group.names {
			if cmd, ok := cmds[name]; ok {
				rows = append(rows, row(cmd))
				listed[name] = true
			}
		}
		if len(rows) > 0 {
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, style.Strong.Render(group.title))
			lines = append(lines, rows...)
		}
	}
	var others []string
	for name := range cmds {
		if !listed[name] {
			others = append(others, name)
		}
	}
	sort.Strings(others)
	if len(others) > 0 {
		lines = append(lines, "", style.Strong.Render("Other"))
		for _, name := range others {
			lines = append(lines, row(cmds[name]))
		}
	}
	return strings.Join(lines, "\n")
}

// renderToolList lists the tools the model can call, grouped by what plan mode does
// with them. Each tool gets its name and required arguments, then the first sentence of
// the description it is offered to the model with, on its own line so it has the full
// width.
func renderToolList() string {
	g := style.Glyphs()
	row := func(spec provider.ToolSpec) string {
		label := style.Accent.Render(spec.Name)
		if required := tools.RequiredArguments(spec); len(required) > 0 {
			label += " " + style.Subtle.Render("("+strings.Join(required, ", ")+")")
		}
		summary := firstSentence(spec.Description)
		if !style.Enabled() {
			return "  " + label + "\n    " + summary
		}
		return "  " + label + "\n" + style.Wrap(style.Muted.Render(summary), terminalWidth()-1, "    ", "    ")
	}

	groups := []struct {
		title, hint string
		policy      tools.PlanModePolicy
	}{
		{"Read-only", "plan mode allows these", tools.PlanModeAllowed},
		{"File changes", "plan mode runs these only as previews", tools.PlanModePreview},
		{"Shell", "blocked in plan mode", tools.PlanModeBlocked},
		{"Code mode", "runs only in code mode; plan mode checks each call it makes", tools.PlanModePerCall},
	}
	var lines []string
	for _, group := range groups {
		var rows []string
		for _, spec := range tools.CodeModeSpecs {
			if tools.PlanModePolicyFor(spec.Name) == group.policy {
				rows = append(rows, row(spec))
			}
		}
		if len(rows) == 0 {
			continue
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, style.Strong.Render(group.title)+" "+style.Subtle.Render(g.Dot+" "+group.hint))
		lines = append(lines, rows...)
	}
	return strings.Join(lines, "\n")
}

// firstSentence returns text up to and including its first ". ", or all of it.
func firstSentence(text string) string {
	if i := strings.Index(text, ". "); i >= 0 {
		return text[:i+1]
	}
	return text
}

// renderKeyValues aligns rows of key and value; keys are dim.
func renderKeyValues(rows [][2]string) string {
	width := 0
	for _, row := range rows {
		if w := style.VisibleWidth(row[0]); w > width {
			width = w
		}
	}
	lines := make([]string, len(rows))
	for i, row := range rows {
		key := style.PadRight(style.Muted.Render(row[0]), width) + " "
		if !style.Enabled() {
			lines[i] = key + row[1]
			continue
		}
		// Long values wrap under themselves rather than under the keys.
		lines[i] = style.Wrap(row[1], terminalWidth()-1, key, strings.Repeat(" ", width+1))
	}
	return strings.Join(lines, "\n")
}

// renderModelChoices numbers the models, marking the current one.
func renderModelChoices(choices []provider.Model, current provider.Model) string {
	lines := make([]string, len(choices))
	for i, choice := range choices {
		number := style.Subtle.Render(fmt.Sprintf("%2d.", i+1))
		if choice == current {
			marker := "*"
			if style.Enabled() {
				marker = style.Highlight.Render(style.Glyphs().Prompt)
			}
			lines[i] = marker + " " + number + " " + style.Highlight.Render(choice.String())
			continue
		}
		lines[i] = "  " + number + " " + choice.String()
	}
	return strings.Join(lines, "\n")
}
