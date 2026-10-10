package style

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Glyphs with ASCII fallbacks for plain output.
type GlyphSet struct {
	Bullet, Result, OK, Fail, Prompt, Arrow, Warn, Info, Dot, Ellipsis string
	HLine, VLine, TopLeft, TopRight, BottomLeft, BottomRight           string
}

var unicodeGlyphs = GlyphSet{
	Bullet: "●", Result: "⎿", OK: "✓", Fail: "✗", Prompt: "❯", Arrow: "→", Warn: "⚠", Info: "ℹ", Dot: "·", Ellipsis: "…",
	HLine: "─", VLine: "│", TopLeft: "╭", TopRight: "╮", BottomLeft: "╰", BottomRight: "╯",
}

var asciiGlyphs = GlyphSet{
	Bullet: "*", Result: "|", OK: "ok", Fail: "x", Prompt: ">", Arrow: "->", Warn: "!", Info: "i", Dot: "-", Ellipsis: "...",
	HLine: "-", VLine: "|", TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+",
}

// Glyphs returns the Unicode glyphs when styling is on and ASCII ones otherwise.
func Glyphs() GlyphSet {
	if enabled {
		return unicodeGlyphs
	}
	return asciiGlyphs
}

// ansiPattern matches CSI sequences (styles, cursor movement) and OSC sequences such as
// OSC 8 hyperlinks, neither of which takes up cells.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// Hyperlink makes text a link to url in terminals that support OSC 8; others show the
// text alone.
func Hyperlink(text, url string) string {
	if !Enabled() {
		return text
	}
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// StripANSI removes SGR and other CSI escape sequences, and OSC sequences.
func StripANSI(text string) string {
	return ansiPattern.ReplaceAllString(text, "")
}

// ActiveAfter returns the SGR escapes still in effect after text, given those in
// effect before it: a reset clears them, any other SGR adds to them. Other CSI
// sequences are not styles and are never replayed.
func ActiveAfter(active, text string) string {
	for _, esc := range ansiPattern.FindAllString(text, -1) {
		if !strings.HasSuffix(esc, "m") {
			continue
		}
		if esc == Reset {
			active = ""
		} else {
			active += esc
		}
	}
	return active
}

// VisibleWidth is the number of terminal cells text takes, ignoring escapes. Wide
// characters are counted as one cell, which is close enough for alignment and padding.
func VisibleWidth(text string) int {
	return utf8.RuneCountInString(StripANSI(text))
}

// PadRight pads text with spaces to width visible cells.
func PadRight(text string, width int) string {
	if gap := width - VisibleWidth(text); gap > 0 {
		return text + strings.Repeat(" ", gap)
	}
	return text
}

// TruncateToWidth cuts plain text to at most width runes, ending with an ellipsis when
// it had to cut, and returns "" when width is not positive. It is meant for unstyled
// text; style after truncating.
func TruncateToWidth(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if utf8.RuneCountInString(text) <= width {
		return text
	}
	ellipsis := Glyphs().Ellipsis
	keep := width - utf8.RuneCountInString(ellipsis)
	if keep <= 0 {
		return string([]rune(text)[:width])
	}
	return string([]rune(text)[:keep]) + ellipsis
}
