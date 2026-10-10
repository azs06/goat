package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/style"
)

func completionNames(items []completion) []string {
	names := make([]string, len(items))
	for i, item := range items {
		names[i] = strings.TrimPrefix(item.display, "/")
	}
	return names
}

func TestCommandCompletions(t *testing.T) {
	if got := commandCompletions("/"); len(got) != len(commands) {
		t.Fatalf("\"/\" lists %d commands, want all %d", len(got), len(commands))
	}
	cases := map[string]string{
		"/mo":        "model",
		"/re":        "resume retry reset",
		"/q":         "quit",
		"/Ex":        "exit export",
		"/zzz":       "",
		"hello":      "",
		"/model gpt": "",
		"/help me":   "",
	}
	for text, want := range cases {
		if got := strings.Join(completionNames(commandCompletions(text)), " "); got != want {
			t.Errorf("commandCompletions(%q) = %q, want %q", text, got, want)
		}
	}
}

func typeKeys(t *testing.T, e *promptEditor, keys ...string) (bool, error) {
	t.Helper()
	for i, key := range keys {
		done, err := e.handleKey(key)
		if done || err != nil {
			if i != len(keys)-1 {
				t.Fatalf("editor finished at key %d of %d", i+1, len(keys))
			}
			return done, err
		}
	}
	return false, nil
}

func TestPromptEditorEnterRunsSelectedCommand(t *testing.T) {
	e := &promptEditor{}
	if done, _ := typeKeys(t, e, "/", "m", "o", "\r"); !done || string(e.buf) != "/model" {
		t.Fatalf("got %q done=%v, want /model submitted", string(e.buf), done)
	}

	e = &promptEditor{}
	typeKeys(t, e, "/", "r", "e", "\x1b[B", "\x1b[B", "\x1b[B", "\x1b[A", "\r")
	if string(e.buf) != "/reset" {
		t.Fatalf("Down, Down, Down (wrapping), Up selected %q, want /reset", string(e.buf))
	}

	// Text that already names a command is submitted as typed, alias included.
	e = &promptEditor{}
	typeKeys(t, e, "/", "n", "e", "w", "\r")
	if string(e.buf) != "/new" {
		t.Fatalf("exact alias was replaced: %q", string(e.buf))
	}

	// Esc closes the menu, so Enter sends what was typed.
	e = &promptEditor{}
	typeKeys(t, e, "/", "m", "\x1b", "\r")
	if string(e.buf) != "/m" {
		t.Fatalf("Enter after Esc gave %q, want /m", string(e.buf))
	}
}

func TestPromptEditorTabCompletes(t *testing.T) {
	e := &promptEditor{}
	typeKeys(t, e, "/", "e", "f", "\t", "h", "i", "g", "h")
	if string(e.buf) != "/effort high" || e.completions() != nil {
		t.Fatalf("got %q with menu %v, want /effort high and no menu", string(e.buf), e.completions())
	}
}

func TestPromptEditorEditing(t *testing.T) {
	e := &promptEditor{}
	typeKeys(t, e, "h", "e", "l", "o", "\x1b[D", "l", "\x01", ">", "\x05", "!", "\x7f", "\x1b[Z", "\x07")
	if string(e.buf) != ">hello" || e.pos != len(e.buf) {
		t.Fatalf("got %q at %d", string(e.buf), e.pos)
	}
	typeKeys(t, e, " ", "w", "o", "r", "l", "d", "\x17")
	if string(e.buf) != ">hello " {
		t.Fatalf("Ctrl-W left %q", string(e.buf))
	}
	typeKeys(t, e, "\x1b[H", "\x1b[3~", "\x0b")
	if string(e.buf) != "" {
		t.Fatalf("Delete then Ctrl-K left %q", string(e.buf))
	}
	if done, err := e.handleKey("\x04"); !done || !errors.Is(err, io.EOF) {
		t.Fatalf("Ctrl-D on an empty line: done=%v err=%v, want io.EOF", done, err)
	}
}

func TestPromptEditorRenderShowsMenuInPlaceOfFooter(t *testing.T) {
	withColor(t, true)
	e := &promptEditor{}
	typeKeys(t, e, "/", "u")
	out := e.render("FOOTER", 40)
	plain := style.StripANSI(out)
	if strings.Contains(plain, "FOOTER") || !strings.Contains(plain, "/undo") {
		t.Fatalf("menu not drawn in place of the footer: %q", plain)
	}
	// Prompt row, rule and one menu row were printed: the cursor goes back up two
	// rows to column 2 + len("/u") + 1.
	if !strings.HasSuffix(out, "\x1b[2A\x1b[5G") {
		t.Fatalf("cursor not returned to the input: %q", out)
	}

	typeKeys(t, e, "\x7f", "\x7f", "x")
	if out := style.StripANSI(e.render("FOOTER", 40)); !strings.Contains(out, "FOOTER") {
		t.Fatalf("footer missing without a menu: %q", out)
	}
}

func TestPromptEditorRenderWrapsLongInput(t *testing.T) {
	withColor(t, true)
	e := &promptEditor{}
	// Rows hold 9 cells: the 2-cell prefix and 7 letters, then the indent and "h".
	e.setText("abcdefgh")
	out := e.render("F", 10)
	if e.cursorRow != 1 || !strings.Contains(out, "abcdefg\n  h\n") || !strings.HasSuffix(out, "\x1b[2A\x1b[4G") {
		t.Fatalf("cursorRow=%d out=%q", e.cursorRow, out)
	}
	if again := e.render("F", 10); !strings.HasPrefix(again, "\x1b[1A\r\x1b[J") {
		t.Fatalf("redraw does not start from the prompt row: %q", again)
	}
	if got := e.clear(); got != "\x1b[2A\r\x1b[J" {
		t.Fatalf("clear = %q", got)
	}
}

func TestReadKeyGroupsEscapeSequences(t *testing.T) {
	reader := newLineReader(strings.NewReader("a\x1b[A\x1b[3~\x1bOHé\x1b"))
	var keys []string
	for {
		key, err := reader.readKey(context.Background())
		if err != nil {
			break
		}
		keys = append(keys, key)
	}
	want := []string{"a", "\x1b[A", "\x1b[3~", "\x1bOH", "é", "\x1b"}
	if strings.Join(keys, "|") != strings.Join(want, "|") {
		t.Fatalf("keys = %q, want %q", keys, want)
	}
}

func TestReadKeyFromLoneEscape(t *testing.T) {
	if key, err := readKeyFrom(bufio.NewReader(strings.NewReader("\x1b"))); err != nil || key != "\x1b" {
		t.Fatalf("got %q, %v", key, err)
	}
}

func TestPromptEditorMultiLine(t *testing.T) {
	e := &promptEditor{}
	// Ctrl-J, Alt-Enter and a backslash before Enter all start a new line.
	if done, _ := typeKeys(t, e, "a", "\n", "b", "\x1b\r", "c", "\\", "\r", "d"); done {
		t.Fatal("expected no submit")
	}
	if string(e.buf) != "a\nb\nc\nd" {
		t.Fatalf("got %q", string(e.buf))
	}
	// Up and Down move between lines when no menu is open; Home and Ctrl-U act on the
	// current line only.
	typeKeys(t, e, "\x1b[A", "\x1b[A", "\x01", "X", "\x1b[B", "\x05", "\x15")
	if string(e.buf) != "a\nXb\n\nd" {
		t.Fatalf("got %q", string(e.buf))
	}
	if done, _ := typeKeys(t, e, "\r"); !done {
		t.Fatal("expected Enter to submit")
	}
}

func TestPromptEditorPasteKeepsLineBreaks(t *testing.T) {
	e := &promptEditor{}
	done, _ := typeKeys(t, e, pasteStart, "l", "1", "\r", "l", "2", "\t", "x", pasteEnd)
	if done || string(e.buf) != "l1\nl2    x" {
		t.Fatalf("got %q done=%v", string(e.buf), done)
	}
	if e.completions() != nil {
		t.Fatal("expected no menu for multi-line text")
	}
}

func TestPromptEditorRenderMultiLine(t *testing.T) {
	withColor(t, true)
	e := &promptEditor{}
	e.setText("one\ntwo")
	out := style.StripANSI(e.render("F", 40))
	if !strings.Contains(out, "❯ one\n  two\n") || e.cursorRow != 1 {
		t.Fatalf("got %q cursorRow=%d", out, e.cursorRow)
	}
}

func TestArgumentCompletions(t *testing.T) {
	setCredentials(t, false, true)
	cfg := &config{model: provider.Model{Provider: provider.Anthropic, ID: "claude-opus-5-5"}, reasoningEffort: provider.EffortHigh, providers: map[string]provider.Provider{}}
	previous := argumentCompletions
	argumentCompletions = commandArgumentCompletions(cfg)
	t.Cleanup(func() { argumentCompletions = previous })

	if got := strings.Join(completionNames(commandCompletions("/effort ")), " "); got != "none minimal low medium high xhigh max" {
		t.Fatalf("effort levels: %q", got)
	}
	if got := strings.Join(completionNames(commandCompletions("/effort m")), " "); got != "minimal medium max" {
		t.Fatalf("filtered levels: %q", got)
	}
	if got := commandCompletions("/model claude-s"); len(got) != 1 || got[0].display != "claude-sonnet-5-5" {
		t.Fatalf("model completions: %+v", got)
	}

	// Enter on a highlighted value runs the command with it; with nothing typed after
	// the space it runs the bare command, which opens the picker.
	e := &promptEditor{}
	if done, _ := typeKeys(t, e, "/", "e", "f", "f", "o", "r", "t", " ", "h", "\x1b[B", "\r"); !done || string(e.buf) != "/effort high" {
		t.Fatalf("got %q", string(e.buf))
	}
	e = &promptEditor{}
	if done, _ := typeKeys(t, e, "/", "e", "f", "f", "o", "r", "t", " ", "\r"); !done || string(e.buf) != "/effort" {
		t.Fatalf("got %q", string(e.buf))
	}
	e = &promptEditor{}
	if _, _ = typeKeys(t, e, "/", "e", "f", "f", "o", "r", "t", " ", "l", "\t"); string(e.buf) != "/effort low" {
		t.Fatalf("Tab gave %q", string(e.buf))
	}
}
