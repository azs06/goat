package main

import (
	"fmt"
	"os"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/terminal"
)

// The style engine lives in internal/style; this file holds goat's own message
// formatting and the theme loading built on it.

// terminalWidth is the width of the terminal stdout writes to, clamped to a sensible
// range, or 80 when it cannot be determined.
// terminalRows is the terminal's height, or 24 when it is unknown.
func terminalRows() int {
	if rows := terminal.Rows(); rows > 0 {
		return rows
	}
	return 24
}

func terminalWidth() int {
	width := terminal.Columns()
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
	if !style.Enabled() {
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
		fmt.Fprintln(os.Stderr, style.Warning.Render(style.Glyphs().Warn+" "+message))
		return
	}
	fmt.Fprintln(os.Stderr, "[warn] "+message)
}

// Status and notice lines share one look: a styled glyph and a message.

func formatWarning(message string) string {
	g := style.Glyphs()
	if !style.Enabled() {
		return "[warn] " + message
	}
	return style.Warning.Render(g.Warn + " " + message)
}

func formatError(message string) string {
	message = provider.HideEndpointPaths(message)
	g := style.Glyphs()
	if !style.Enabled() {
		return "Error: " + message
	}
	return style.Error.Render(g.Fail + " " + message)
}

func formatNotice(message string) string {
	if !style.Enabled() {
		return message
	}
	return style.Muted.Render(style.Glyphs().Dot + " " + message)
}

func formatSuccess(message string) string {
	if !style.Enabled() {
		return message
	}
	return style.Success.Render(style.Glyphs().OK) + " " + message
}
