package main

import (
	"os"
	"strings"
	"testing"
)

func TestPromptHistoryBrowsing(t *testing.T) {
	e := &promptEditor{history: []string{"first", "second\nline"}, historyIndex: 2}
	e.setText("draft")
	typeKeys(t, e, "\x1b[A")
	if string(e.buf) != "second\nline" {
		t.Fatalf("Up gave %q", string(e.buf))
	}
	// Inside a multi-line entry Up moves a line first, then goes further back.
	typeKeys(t, e, "\x1b[A", "\x1b[A")
	if string(e.buf) != "first" {
		t.Fatalf("got %q", string(e.buf))
	}
	typeKeys(t, e, "\x1b[A")
	if string(e.buf) != "first" {
		t.Fatal("expected Up at the oldest entry to stay")
	}
	typeKeys(t, e, "\x1b[B", "\x1b[B", "\x1b[B")
	if string(e.buf) != "draft" {
		t.Fatalf("expected Down to return to the draft, got %q", string(e.buf))
	}
}

func TestPromptHistoryFile(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	history := loadPromptHistory()
	history = rememberPrompt(history, "one")
	history = rememberPrompt(history, "one")
	history = rememberPrompt(history, "two\nlines")
	history = rememberPrompt(history, "   ")
	if got := strings.Join(loadPromptHistory(), "|"); got != "one|two\nlines" || len(history) != 2 {
		t.Fatalf("reloaded %q", got)
	}
}

func TestPathCompletion(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	for _, dir := range []string{"internal/provider", "internal/style", ".goat"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"main.go", "main_test.go", "readme.md", ".hidden"} {
		os.WriteFile(file, nil, 0o644)
	}

	e := &promptEditor{}
	typeKeys(t, e, "l", "o", "o", "k", " ", "a", "t", " ", "r", "\t")
	if string(e.buf) != "look at readme.md" {
		t.Fatalf("single match: %q", string(e.buf))
	}

	e = &promptEditor{}
	typeKeys(t, e, "m", "\t")
	if string(e.buf) != "main" {
		t.Fatalf("expected the shared start completed, got %q", string(e.buf))
	}
	typeKeys(t, e, "\t")
	items := e.completions()
	if len(items) != 2 || items[0].display != "main.go" || items[1].display != "main_test.go" {
		t.Fatalf("expected a menu of both files, got %+v", items)
	}
	if done, _ := typeKeys(t, e, "\x1b[B", "\r"); done || string(e.buf) != "main_test.go" {
		t.Fatalf("Enter in the path menu should take the path, got %q done=%v", string(e.buf), done)
	}

	e = &promptEditor{}
	typeKeys(t, e, "@", "i", "n", "\t", "p", "\t")
	if string(e.buf) != "@internal/provider/" {
		t.Fatalf("@ mention and directories: %q", string(e.buf))
	}

	if matches, _ := pathCompletions([]rune("."), 1); strings.Contains(strings.Join(matches, " "), ".goat") || len(matches) != 1 {
		t.Fatalf("expected hidden files but never .goat, got %v", matches)
	}
	if matches, _ := pathCompletions([]rune("../"), 3); matches != nil {
		t.Fatalf("expected nothing outside the workspace, got %v", matches)
	}
}
