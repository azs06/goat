package main

import (
	"strings"
	"testing"

	"github.com/azs06/goat/internal/style"
)

func TestSelectorKeys(t *testing.T) {
	s := &selector{options: []selectOption{{label: "low"}, {label: "medium", current: true}, {label: "high"}, {label: "max"}}}
	s.handleKey("\x1b[B")
	s.handleKey("\x1b[B")
	if done, chosen := s.handleKey("\r"); !done || chosen != 2 {
		t.Fatalf("Down, Down, Enter: done=%v chosen=%d", done, chosen)
	}

	s = &selector{options: s.options}
	s.handleKey("\x1b[A") // wraps to the last entry
	if _, chosen := s.handleKey("\r"); chosen != 3 {
		t.Fatalf("Up from the top chose %d", chosen)
	}

	// Typing filters; Enter picks within the filtered list.
	s = &selector{options: s.options}
	for _, key := range []string{"h", "i"} {
		s.handleKey(key)
	}
	if _, chosen := s.handleKey("\r"); chosen != 2 {
		t.Fatalf("filtered pick chose %d", chosen)
	}
	s.handleKey("z")
	if done, _ := s.handleKey("\r"); done {
		t.Fatal("expected Enter to do nothing when nothing matches")
	}
	if done, chosen := s.handleKey("\x1b"); !done || chosen != -1 {
		t.Fatal("expected Esc to cancel")
	}
}

func TestSelectorRender(t *testing.T) {
	withColor(t, true)
	var options []selectOption
	for _, group := range []string{"openai", "anthropic"} {
		for i := 0; i < 8; i++ {
			options = append(options, selectOption{label: group + "-model-with-a-long-name", hint: "a hint that goes on and on and on", group: group})
		}
	}
	options[3].current = true
	s := &selector{title: "Switch model", options: options}
	out := style.StripANSI(s.render(70))
	if !strings.Contains(out, "openai") || !strings.Contains(out, "↓ 6 more") || !strings.Contains(out, "enter select") || !strings.Contains(out, "current") {
		t.Fatalf("unexpected render:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if style.VisibleWidth(line) >= 70 {
			t.Errorf("line too wide (%d): %q", style.VisibleWidth(line), line)
		}
	}
	again := s.render(70)
	if !strings.HasPrefix(again, "\x1b[") || s.rows == 0 {
		t.Fatalf("redraw should start by moving up over the old list: %q", again[:10])
	}
}
