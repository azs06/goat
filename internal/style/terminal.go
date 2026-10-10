// Package style is goat's terminal style engine. Text is styled by role (Accent,
// Muted, Heading, ...) and a theme decides what each role looks like. Themes are JSON
// files whose styles are written as CSS declarations; the built-in default theme is
// one of them.
//
// Styling only happens when stdout is a terminal that can show it. Piped or redirected
// output, NO_COLOR, TERM=dumb and Windows consoles (which may not have VT processing
// on) get plain text and ASCII glyphs, so scripted runs and logs stay readable, whatever
// the theme says.
package style

import (
	"os"
	"runtime"
)

var (
	enabled   = detectEnabled()
	trueColor = detectTrueColor()
)

func detectEnabled() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" || runtime.GOOS == "windows" {
		return false
	}
	if os.Getenv("GOAT_FORCE_COLOR") != "" {
		return true
	}
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// detectTrueColor reports whether the terminal advertises 24-bit color. Without it,
// colors are mapped to the nearest entry of the 256-color palette.
func detectTrueColor() bool {
	switch os.Getenv("COLORTERM") {
	case "truecolor", "24bit":
		return true
	}
	return false
}

// Enabled reports whether output is styled at all.
func Enabled() bool { return enabled }

// SetEnabled turns styling on or off and returns a function that restores the previous
// setting. It is meant for tests.
func SetEnabled(on bool) (restore func()) {
	previous := enabled
	enabled = on
	return func() { enabled = previous }
}

// SetTrueColor chooses between 24-bit color and the 256-color palette and returns a
// function that restores the previous setting. It is meant for tests.
func SetTrueColor(on bool) (restore func()) {
	previous := trueColor
	trueColor = on
	return func() { trueColor = previous }
}
