package main

import (
	"strings"
	"testing"

	"github.com/azs06/goat/internal/style"
)

func TestHighlightCodeLine(t *testing.T) {
	chromeWithColor(t, true)

	line, ok := highlightCodeLine("go", []string{"package main", `func main() { return "hi" }`})
	if !ok {
		t.Fatal("expected a Go lexer")
	}
	if !strings.Contains(line, style.CodeKeyword.Render("func")) || !strings.Contains(line, style.CodeString.Render(`"hi"`)) || !strings.Contains(line, style.CodeFunction.Render("main")) {
		t.Fatalf("unexpected highlighting: %q", line)
	}
	if style.StripANSI(line) != `func main() { return "hi" }` {
		t.Fatalf("highlighting changed the text: %q", style.StripANSI(line))
	}

	// A comment that started on an earlier line keeps its color.
	line, _ = highlightCodeLine("go", []string{"/* a long", "comment */ x := 1"})
	if !strings.HasPrefix(line, style.CodeComment.Render("comment */")) {
		t.Fatalf("expected the comment to continue: %q", line)
	}

	if _, ok := highlightCodeLine("no-such-language", []string{"x"}); ok {
		t.Fatal("expected no lexer for an unknown language")
	}
}

func TestMarkdownCodeBlocksAreHighlighted(t *testing.T) {
	chromeWithColor(t, true)
	st := mdState{}
	var out []string
	for _, line := range []string{"```python", "def f():", "    return 42", "```"} {
		var rendered string
		rendered, st = renderMarkdownLine(line, st, 80)
		out = append(out, rendered)
	}
	if !strings.Contains(out[1], style.CodeKeyword.Render("def")) || !strings.Contains(out[2], style.CodeNumber.Render("42")) {
		t.Fatalf("unexpected block: %q", out)
	}

	chromeWithColor(t, false)
	plain, _ := renderMarkdownLine("x = 1", mdState{inFence: true, fenceSeq: "```", lang: "python"}, 80)
	if strings.Contains(plain, "\x1b") {
		t.Fatalf("plain output has escapes: %q", plain)
	}
}

func TestMarkdownLinksAndImages(t *testing.T) {
	chromeWithColor(t, true)
	out := renderInline("see [the docs](https://example.com/docs) and ![a cat](cat.png).", style.Style{})
	if !strings.Contains(out, "\x1b]8;;https://example.com/docs\x1b\\") {
		t.Fatalf("expected an OSC 8 link: %q", out)
	}
	if plain := style.StripANSI(out); plain != "see the docs (https://example.com/docs) and [image: a cat]." {
		t.Fatalf("visible text: %q", plain)
	}
	if width := style.VisibleWidth(out); width != len("see the docs (https://example.com/docs) and [image: a cat].") {
		t.Fatalf("width counts the escape sequences: %d", width)
	}
	if out := renderInline("[x] done, [not a link] (here)", style.Style{}); style.StripANSI(out) != "[x] done, [not a link] (here)" {
		t.Fatalf("brackets that are not links changed: %q", style.StripANSI(out))
	}
}
