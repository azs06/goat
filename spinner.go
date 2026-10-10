package main

import (
	"fmt"
	"os"
	"sync"
	"time"
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
	drawn   bool
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
	if !colorEnabled {
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

	frame := 0
	for {
		s.draw(frame)
		frame++
		select {
		case <-s.stop:
			s.clear()
			return
		case <-ticker.C:
		}
	}
}

func (s *spinner) draw(frame int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	elapsed := time.Since(s.started).Truncate(time.Second)
	glyph := spinnerFrames[frame%len(spinnerFrames)]
	line := styleAccent(glyph+" "+s.label+glyphs().ellipsis) + " " + styleSubtle(fmt.Sprintf("(%s · ctrl-c to interrupt)", formatElapsed(elapsed)))
	// \r and erase-line redraw in place; the line is cut to the terminal width so it
	// never wraps, which would leave stale copies behind.
	if visibleWidth(line) >= terminalWidth() {
		line = styleAccent(glyph + " " + s.label + glyphs().ellipsis)
	}
	fmt.Fprint(os.Stdout, "\r\x1b[2K"+line)
	s.drawn = true
}

func (s *spinner) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drawn {
		fmt.Fprint(os.Stdout, "\r\x1b[2K")
		s.drawn = false
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
