package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/azs06/goat/internal/style"
)

// spinner shows an animated status line ("✻ Thinking… 4s · ctrl-c to interrupt") while
// goat waits on the model or a tool. It only draws on a color terminal; elsewhere it
// does nothing, so piped output never contains carriage returns or cursor movement.
// There is at most one active spinner, kept in activeSpinner, so code that needs the
// terminal (an approval prompt, the first streamed token) can stop it from anywhere.
type spinner struct {
	mu      sync.Mutex
	label   string
	started time.Time
	stop    chan struct{}
	done    chan struct{}
	frame   int
	// rows is how many lines the block took up when last drawn, 0 when nothing is on
	// screen. The cursor is always on its last line.
	rows int
	// cleared is set once the spinner is gone for good, so a late redraw from a key
	// typed at the same moment cannot bring it back.
	cleared bool
}

var (
	spinnerMu     sync.Mutex
	activeSpinner *spinner
)

var spinnerFrames = []string{"·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"}

const spinnerInterval = 110 * time.Millisecond

// startSpinner shows label with an animation until stopSpinner is called. A spinner
// already running is replaced.
func startSpinner(label string) {
	startSpinnerAt(label, time.Now())
}

func startSpinnerAt(label string, started time.Time) {
	if !style.Enabled() {
		return
	}
	stopSpinner()

	s := &spinner{label: label, started: started, stop: make(chan struct{}), done: make(chan struct{})}
	spinnerMu.Lock()
	activeSpinner = s
	spinnerMu.Unlock()
	go s.run()
}

// stopSpinner clears the spinner line, if one is showing, and waits until it is gone so
// the caller can write to the terminal straight away.
func stopSpinner() {
	spinnerMu.Lock()
	s := activeSpinner
	activeSpinner = nil
	spinnerMu.Unlock()
	if s == nil {
		return
	}
	close(s.stop)
	<-s.done
}

// pauseSpinner clears the active spinner, if any, and returns a function that brings
// it back with the same label. Time spent paused does not count toward its elapsed
// time. It lets code write to the terminal mid-task without losing the spinner.
func pauseSpinner() func() {
	spinnerMu.Lock()
	s := activeSpinner
	spinnerMu.Unlock()
	if s == nil {
		return func() {}
	}
	stopSpinner()
	pausedAt := time.Now()
	return func() {
		spinnerMu.Lock()
		busy := activeSpinner != nil
		spinnerMu.Unlock()
		if busy {
			return
		}
		startSpinnerAt(s.label, s.started.Add(time.Since(pausedAt)))
	}
}

func (s *spinner) run() {
	defer close(s.done)
	ticker := time.NewTicker(spinnerInterval)
	defer ticker.Stop()

	for {
		s.mu.Lock()
		s.frame++
		s.mu.Unlock()
		s.draw()
		select {
		case <-s.stop:
			s.clear()
			return
		case <-ticker.C:
		}
	}
}

// redrawSpinner draws the active spinner again straight away, after the text typed
// under it changed.
func redrawSpinner() {
	spinnerMu.Lock()
	s := activeSpinner
	spinnerMu.Unlock()
	if s != nil {
		s.draw()
	}
}

// draw replaces the block on screen: the status line, then the queued messages and the
// line being typed while a turn runs. No line is wider than the terminal, so the block
// never wraps and clearing it only has to move up its row count.
func (s *spinner) draw() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleared {
		return
	}

	frame := s.frame
	elapsed := time.Since(s.started).Truncate(time.Second)
	glyph := spinnerFrames[frame%len(spinnerFrames)]
	line := style.Accent.Render(glyph+" "+s.label+style.Glyphs().Ellipsis) + " " + style.Subtle.Render(fmt.Sprintf("(%s · ctrl-c to interrupt)", formatElapsed(elapsed)))
	// \r and erase-line redraw in place; the line is cut to the terminal width so it
	// never wraps, which would leave stale copies behind.
	width := terminalWidth()
	if style.VisibleWidth(line) >= width {
		line = style.Accent.Render(glyph + " " + s.label + style.Glyphs().Ellipsis)
	}
	below, cursor := queuedInputLines(width)

	var b strings.Builder
	b.WriteString(s.eraseBlock())
	b.WriteString(line)
	for _, extra := range below {
		b.WriteString("\n\r\x1b[2K" + extra)
	}
	if len(below) > 0 {
		fmt.Fprintf(&b, "\x1b[%dG", cursor)
	}
	fmt.Fprint(os.Stdout, b.String())
	s.rows = 1 + len(below)
}

// eraseBlock moves to the first line of the block and clears from there down. The
// caller holds s.mu.
func (s *spinner) eraseBlock() string {
	if s.rows == 0 {
		return "\r\x1b[2K"
	}
	up := ""
	if s.rows > 1 {
		up = fmt.Sprintf("\x1b[%dA", s.rows-1)
	}
	return up + "\r\x1b[J"
}

func (s *spinner) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleared = true
	if s.rows > 0 {
		fmt.Fprint(os.Stdout, s.eraseBlock())
		s.rows = 0
	}
}

// formatElapsed renders a duration as "4s", "1m 12s" or "1h 3m".
func formatElapsed(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
