package main

import (
	"strings"
	"testing"
)

func withColor(t *testing.T, on bool) {
	t.Helper()
	prev := colorEnabled
	colorEnabled = on
	t.Cleanup(func() { colorEnabled = prev })
}

func renderPlain(line string, width int) string {
	out, _ := renderMarkdownLine(line, mdState{}, width)
	return stripANSI(out)
}

func TestRenderMarkdownHeadings(t *testing.T) {
	withColor(t, true)
	h1, _ := renderMarkdownLine("# Title", mdState{}, 80)
	if stripANSI(h1) != "Title" || !strings.Contains(h1, fgAccent) || !strings.Contains(h1, "\x1b[1;") {
		t.Fatalf("h1 = %q", h1)
	}
	h3, _ := renderMarkdownLine("### Sub ###", mdState{}, 80)
	if stripANSI(h3) != "Sub" || strings.Contains(h3, fgAccent) {
		t.Fatalf("h3 = %q", h3)
	}
	// Bold inside a heading re-opens the heading style afterwards.
	h2, _ := renderMarkdownLine("## a **b** c", mdState{}, 80)
	if !strings.Contains(h2, sgrReset+"\x1b[1;"+fgAccent+"m c") {
		t.Fatalf("h2 = %q", h2)
	}
}

func TestRenderInlineEmphasisAndCode(t *testing.T) {
	withColor(t, true)
	cases := map[string]string{
		"**bold** text":   "\x1b[1mbold" + sgrReset + " text",
		"an *italic* one": "an \x1b[3mitalic" + sgrReset + " one",
		"an _italic_ one": "an \x1b[3mitalic" + sgrReset + " one",
		"*a* *b*":         "\x1b[3ma" + sgrReset + " \x1b[3mb" + sgrReset,
		"use `x*y*z` now": "use \x1b[" + fgMagenta + "mx*y*z" + sgrReset + " now",
	}
	for in, want := range cases {
		if got := renderInline(in, nil); got != want {
			t.Errorf("renderInline(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderInlineLeavesIdentifiersAlone(t *testing.T) {
	withColor(t, true)
	for _, in := range []string{
		"snake_case_identifiers and my_var_name",
		"2*3*4 = 24",
		"a * b * c",
		"unclosed **bold and `code",
		"***",
	} {
		if got := renderInline(in, nil); got != in {
			t.Errorf("renderInline(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestRenderMarkdownCodeFence(t *testing.T) {
	withColor(t, true)
	st := mdState{}
	var rows []string
	for _, line := range []string{"```go", "x := a*b*c // **not bold**", "", "```", "after **b**"} {
		var out string
		out, st = renderMarkdownLine(line, st, 20)
		rows = append(rows, out)
	}
	if stripANSI(rows[0]) != "╭─ go" {
		t.Errorf("open = %q", rows[0])
	}
	if stripANSI(rows[1]) != "│ x := a*b*c // **not bold**" || strings.Contains(rows[1], "\x1b[1m") {
		t.Errorf("code row = %q (must not be wrapped or styled as markdown)", rows[1])
	}
	if stripANSI(rows[2]) != "│ " {
		t.Errorf("blank code row = %q", rows[2])
	}
	if stripANSI(rows[3]) != "╰─" || st.inFence {
		t.Errorf("close = %q, state %+v", rows[3], st)
	}
	if !strings.Contains(rows[4], "\x1b[1mb") {
		t.Errorf("markdown after the fence = %q", rows[4])
	}

	// A longer fence is only closed by a run at least as long.
	_, st = renderMarkdownLine("````", mdState{}, 80)
	_, st = renderMarkdownLine("```", st, 80)
	if !st.inFence {
		t.Error("```` fence closed by ```")
	}
}

func TestRenderMarkdownBlocks(t *testing.T) {
	withColor(t, true)
	cases := map[string]string{
		"- item":        "• item",
		"  * nested":    "  • nested",
		"+ plus":        "• plus",
		"12. twelfth":   "12. twelfth",
		"> quoted text": "│ quoted text",
		"| a | b |":     "| a | b |",
		"**bold** line": "bold line",
	}
	for in, want := range cases {
		if got := renderPlain(in, 80); got != want {
			t.Errorf("render(%q) = %q, want %q", in, got, want)
		}
	}
	if got := renderPlain("* * *", 10); got != strings.Repeat("─", 10) {
		t.Errorf("rule = %q", got)
	}
	if got := renderPlain("---", 5); got != "─────" {
		t.Errorf("rule = %q", got)
	}
}

func TestRenderMarkdownWrapping(t *testing.T) {
	withColor(t, true)
	got := renderPlain("- one two three four five", 12)
	want := "• one two\n  three four\n  five"
	if got != want {
		t.Fatalf("wrapped list = %q, want %q", got, want)
	}

	// A style that spans a break is closed before the newline and re-opened after the
	// next row's indent.
	out, _ := renderMarkdownLine("- **aaa bbb ccc**", mdState{}, 9)
	rows := strings.Split(out, "\n")
	if len(rows) != 2 || !strings.HasSuffix(rows[0], sgrReset) || !strings.HasPrefix(rows[1], "  \x1b[1m") {
		t.Fatalf("styled wrap = %q", rows)
	}

	// Tables are never wrapped.
	table := "| a long cell | another long cell |"
	if got := renderPlain(table, 10); got != table {
		t.Fatalf("table = %q", got)
	}
}

func TestMarkdownStreamLayout(t *testing.T) {
	withColor(t, true)
	md := newMarkdownStream(40)
	var out strings.Builder
	for _, chunk := range []string{"\n\nHel", "lo **wor", "ld**\n\n\n", "- a\n", "tail"} {
		out.WriteString(md.Write(chunk))
	}
	if strings.Contains(out.String(), "tail") {
		t.Fatal("an incomplete line was printed before its newline")
	}
	out.WriteString(md.Flush())
	got := stripANSI(out.String())
	want := "\n● Hello world\n\n  • a\n  tail\n"
	if got != want {
		t.Fatalf("stream = %q, want %q", got, want)
	}

	// Whitespace-only output renders nothing at all.
	md = newMarkdownStream(40)
	if got := md.Write("\n\n  \n") + md.Flush(); got != "" {
		t.Fatalf("whitespace-only stream = %q", got)
	}
}

func TestTrackSGRIgnoresNonStyleSequences(t *testing.T) {
	if got := trackSGR("", "\x1b[2J\x1b[31mred"); got != "\x1b[31m" {
		t.Errorf("got %q", got)
	}
}
