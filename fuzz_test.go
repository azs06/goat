package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzRenderMarkdownLine checks the renderer never panics and adds no escapes to plain
// output.
func FuzzRenderMarkdownLine(f *testing.F) {
	for _, seed := range []string{"# title", "- [link](http://x)", "```go", "**a _b_ `c`**", "![x](y", "| a | b |", "> quote [x](", "1. `x"} {
		f.Add(seed, false)
	}
	f.Fuzz(func(t *testing.T, line string, color bool) {
		chromeWithColor(t, color)
		for _, st := range []mdState{{}, {inFence: true, fenceSeq: "```", lang: "go"}} {
			out, _ := renderMarkdownLine(line, st, 40)
			if !color && !strings.Contains(line, "\x1b") && strings.Contains(out, "\x1b") {
				t.Fatalf("plain output of %q has escapes: %q", line, out)
			}
		}
	})
}

// FuzzPromptEditorKeys checks that any key sequence keeps the cursor inside the text and
// renders.
func FuzzPromptEditorKeys(f *testing.F) {
	f.Add([]byte("hello\x1b[D\x1b[Dxx\r"))
	f.Add([]byte("a\nb\nc\x1b[A\x1b[A\x15\x0b"))
	f.Add([]byte("\x1b[200~p\rq\x1b[201~/ef\t"))
	f.Fuzz(func(t *testing.T, input []byte) {
		e := &promptEditor{history: []string{"old", "two\nlines"}, historyIndex: 2}
		keys := splitKeys(string(input))
		for _, key := range keys {
			e.handleKey(key)
			if e.pos < 0 || e.pos > len(e.buf) {
				t.Fatalf("cursor %d outside text of %d runes after %q", e.pos, len(e.buf), keys)
			}
		}
		e.render("footer", 30)
	})
}

// splitKeys cuts input into keys the way readKeyFrom does for a burst of input.
func splitKeys(input string) []string {
	var keys []string
	for len(input) > 0 {
		if strings.HasPrefix(input, "\x1b[") {
			end := 2
			for end < len(input) && !(input[end] >= 0x40 && input[end] <= 0x7e) {
				end++
			}
			end = min(end+1, len(input))
			keys = append(keys, input[:end])
			input = input[end:]
			continue
		}
		_, size := utf8.DecodeRuneInString(input)
		keys = append(keys, input[:size])
		input = input[size:]
	}
	return keys
}
