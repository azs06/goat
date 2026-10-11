package style

import (
	"unicode"

	"golang.org/x/text/width"
	"regexp"
	"strings"
	"unicode/utf8"
)

// GlyphSet is the glyphs goat draws with; plain output uses ASCII fallbacks.
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

// CellWidth is how many terminal cells text takes, styles aside: two for wide
// characters, such as CJK ideographs and most emoji, none for combining marks, one for
// the rest. VisibleWidth counts every rune as one cell; this is for lines whose rows
// must not wrap, such as the spinner's.
func CellWidth(text string) int {
	cells := 0
	for _, r := range StripANSI(text) {
		cells += runeCells(r)
	}
	return cells
}

func runeCells(r rune) int {
	switch {
	case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200D:
		return 0
	case r >= 0x1F300 && r <= 0x1FAFF:
		return 2
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}

// TruncateToCells is TruncateToWidth counting terminal cells, as CellWidth does.
func TruncateToCells(text string, cells int) string {
	if cells <= 0 {
		return ""
	}
	if CellWidth(text) <= cells {
		return text
	}
	ellipsis := Glyphs().Ellipsis
	room := cells - CellWidth(ellipsis)
	if room < 0 {
		room, ellipsis = cells, ""
	}
	var b strings.Builder
	used := 0
	for _, r := range text {
		if used+runeCells(r) > room {
			break
		}
		used += runeCells(r)
		b.WriteRune(r)
	}
	return b.String() + ellipsis
}

// Wrap word-wraps styled text to width visible cells. The first row starts with
// first and the rest with rest. A style open at a row break is closed at the end of
// the row and re-opened after the next row's prefix, so it never colors the indent.
// Words longer than a row are left to overflow.
func Wrap(text string, width int, first, rest string) string {
	words := strings.Split(text, " ")
	var rows []string
	row := first
	rowWidth := VisibleWidth(first)
	empty := true
	active := ""
	for _, word := range words {
		wordWidth := VisibleWidth(word)
		switch {
		case empty:
			row += word
			rowWidth += wordWidth
			empty = false
		case rowWidth+1+wordWidth > width && wordWidth > 0:
			if active != "" {
				row += Reset
			}
			rows = append(rows, row)
			row = rest + active + word
			rowWidth = VisibleWidth(rest) + wordWidth
		default:
			row += " " + word
			rowWidth += 1 + wordWidth
		}
		active = ActiveAfter(active, word)
	}
	rows = append(rows, row)
	return strings.Join(rows, "\n")
}
