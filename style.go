package main

import (
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"
)

// The terminal UI styles text with ANSI escapes only when stdout is a terminal that
// can show them. Piped or redirected output, NO_COLOR, TERM=dumb and Windows consoles
// (which may not have VT processing on) get plain text and ASCII glyphs, so scripted
// runs and logs stay readable.
var colorEnabled = detectColor()

func detectColor() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" || runtime.GOOS == "windows" {
		return false
	}
	if os.Getenv("GOAT_FORCE_COLOR") != "" {
		return true
	}
	return stdoutIsTerminal()
}

func stdoutIsTerminal() bool {
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// ANSI SGR codes. Colors use the 256-color palette, which every terminal in common use
// supports, rather than truecolor.
const (
	sgrReset  = "\x1b[0m"
	sgrBold   = "1"
	sgrDim    = "2"
	sgrItalic = "3"

	fgAccent  = "38;5;173" // goat amber, the brand color
	fgMuted   = "38;5;245"
	fgSubtle  = "38;5;240"
	fgGreen   = "38;5;71"
	fgRed     = "38;5;167"
	fgYellow  = "38;5;179"
	fgCyan    = "38;5;80"
	fgMagenta = "38;5;140"

	bgPrompt = "48;5;236"
)

// paint wraps text in the given SGR codes when color is enabled. Each line is wrapped
// separately so a style never bleeds across a newline into the next prompt or output.
func paint(text string, codes ...string) string {
	if !colorEnabled || text == "" || len(codes) == 0 {
		return text
	}
	open := sgrOpen(codes)
	if !strings.Contains(text, "\n") {
		return open + text + sgrReset
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = open + line + sgrReset
		}
	}
	return strings.Join(lines, "\n")
}

func styleBold(text string) string    { return paint(text, sgrBold) }
func styleDim(text string) string     { return paint(text, fgMuted) }
func styleSubtle(text string) string  { return paint(text, fgSubtle) }
func styleAccent(text string) string  { return paint(text, fgAccent) }
func styleSuccess(text string) string { return paint(text, fgGreen) }
func styleError(text string) string   { return paint(text, fgRed) }
func styleWarn(text string) string    { return paint(text, fgYellow) }

// sgrOpen is the escape that turns on codes, or "" when there are none.
func sgrOpen(codes []string) string {
	if len(codes) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

// Glyphs with ASCII fallbacks for plain output.
type glyphSet struct {
	bullet, result, ok, fail, prompt, arrow, warn, info, dot, ellipsis string
	hline, vline, topLeft, topRight, bottomLeft, bottomRight           string
}

var unicodeGlyphs = glyphSet{
	bullet: "●", result: "⎿", ok: "✓", fail: "✗", prompt: "❯", arrow: "→", warn: "⚠", info: "ℹ", dot: "·", ellipsis: "…",
	hline: "─", vline: "│", topLeft: "╭", topRight: "╮", bottomLeft: "╰", bottomRight: "╯",
}

var asciiGlyphs = glyphSet{
	bullet: "*", result: "|", ok: "ok", fail: "x", prompt: ">", arrow: "->", warn: "!", info: "i", dot: "-", ellipsis: "...",
	hline: "-", vline: "|", topLeft: "+", topRight: "+", bottomLeft: "+", bottomRight: "+",
}

func glyphs() glyphSet {
	if colorEnabled {
		return unicodeGlyphs
	}
	return asciiGlyphs
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// stripANSI removes SGR and other CSI escape sequences.
func stripANSI(text string) string {
	return ansiPattern.ReplaceAllString(text, "")
}

// visibleWidth is the number of terminal cells text takes, ignoring escapes. Wide
// characters are counted as one cell, which is close enough for alignment and padding.
func visibleWidth(text string) int {
	return utf8.RuneCountInString(stripANSI(text))
}

// padRight pads text with spaces to width visible cells.
func padRight(text string, width int) string {
	if gap := width - visibleWidth(text); gap > 0 {
		return text + strings.Repeat(" ", gap)
	}
	return text
}

// truncateToWidth cuts plain text to at most width runes, ending with an ellipsis when
// it had to cut, and returns "" when width is not positive. It is meant for unstyled
// text; style after truncating.
func truncateToWidth(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if utf8.RuneCountInString(text) <= width {
		return text
	}
	ellipsis := glyphs().ellipsis
	keep := width - utf8.RuneCountInString(ellipsis)
	if keep <= 0 {
		return string([]rune(text)[:width])
	}
	return string([]rune(text)[:keep]) + ellipsis
}

// terminalWidth is the width of the terminal stdout writes to, clamped to a sensible
// range, or 80 when it cannot be determined.
func terminalWidth() int {
	width := stdoutColumns()
	if width <= 0 {
		return 80
	}
	if width < 20 {
		return 20
	}
	return width
}

// stderrColor reports whether warnings written to stderr may be styled: color is on and
// stderr is a terminal too (or color is forced), so a redirected stderr log stays plain.
func stderrColor() bool {
	if !colorEnabled {
		return false
	}
	if os.Getenv("GOAT_FORCE_COLOR") != "" {
		return true
	}
	info, err := os.Stderr.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// warnf writes a warning to stderr. A spinner on the terminal is cleared first and
// redrawn afterwards, so the warning never ends up glued to the spinner line.
func warnf(format string, args ...any) {
	resume := pauseSpinner()
	defer resume()
	message := fmt.Sprintf(format, args...)
	if stderrColor() {
		fmt.Fprintln(os.Stderr, styleWarn(glyphs().warn+" "+message))
		return
	}
	fmt.Fprintln(os.Stderr, "[warn] "+message)
}

// Status and notice lines share one look: a colored glyph and a message.

func formatWarning(message string) string {
	g := glyphs()
	if !colorEnabled {
		return "[warn] " + message
	}
	return styleWarn(g.warn + " " + message)
}

func formatError(message string) string {
	g := glyphs()
	if !colorEnabled {
		return "Error: " + message
	}
	return styleError(g.fail + " " + message)
}

func formatNotice(message string) string {
	if !colorEnabled {
		return message
	}
	return styleDim(glyphs().dot + " " + message)
}

func formatSuccess(message string) string {
	if !colorEnabled {
		return message
	}
	return styleSuccess(glyphs().ok) + " " + message
}
