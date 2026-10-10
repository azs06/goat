package markdown

import (
	"strings"
	"testing"
)

// FuzzRenderMarkdownLine checks the renderer never panics and adds no escapes to plain
// output.
func FuzzRenderMarkdownLine(f *testing.F) {
	for _, seed := range []string{"# title", "- [link](http://x)", "```go", "**a _b_ `c`**", "![x](y", "| a | b |", "> quote [x](", "1. `x"} {
		f.Add(seed, false)
	}
	f.Fuzz(func(t *testing.T, line string, color bool) {
		withColor(t, color)
		for _, st := range []mdState{{}, {inFence: true, fenceSeq: "```", lang: "go"}} {
			out, _ := renderMarkdownLine(line, st, 40)
			if !color && !strings.Contains(line, "\x1b") && strings.Contains(out, "\x1b") {
				t.Fatalf("plain output of %q has escapes: %q", line, out)
			}
		}
	})
}
