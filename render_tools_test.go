package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/azs06/goat/internal/provider"

	"github.com/azs06/goat/internal/style"
)

func withToolTestColor(t *testing.T, enabled bool) {
	t.Helper()
	t.Cleanup(style.SetEnabled(enabled))
	t.Cleanup(style.SetTrueColor(false))
}

func toolTestCall(name, args string) provider.ToolCall {
	return provider.ToolCall{ID: "1", Name: name, Arguments: args}
}

func toolTestLines(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	return strings.Join(lines, "\n")
}

func TestRenderToolHeaderPlain(t *testing.T) {
	withToolTestColor(t, false)

	cases := []struct {
		call provider.ToolCall
		want string
	}{
		{toolTestCall("read_file", `{"path":"main.go"}`), "* Read(main.go)"},
		{toolTestCall("run_bash", `{"command":"go test ./...\n  -v","workdir":"internal"}`), "* Bash(go test ./... -v) [in internal]"},
		{toolTestCall("grep_files", `{"path":".","query":"TODO"}`), `* Grep("TODO" in .)`},
		{toolTestCall("find_files", `{"path":"internal","pattern":"*.go"}`), "* Find(*.go in internal)"},
		{toolTestCall("move_file", `{"source_path":"a.go","destination_path":"b.go","preview":true}`), "* Move(a.go -> b.go) [preview]"},
		{toolTestCall("edit_file", `{"path":"x.go","old_text":"a","new_text":"b","replace_all":true}`), "* Update(x.go) [replace all]"},
		{toolTestCall("undo_last_change", `{}`), "* Undo"},
		{toolTestCall("mystery", `{"a": 1}`), `* mystery({"a": 1})`},
		{toolTestCall("read_file", `not json`), "* Read(not json)"},
	}
	for _, tc := range cases {
		if got := renderToolHeader(tc.call, 80); got != tc.want {
			t.Errorf("renderToolHeader(%s %s) = %q, want %q", tc.call.Name, tc.call.Arguments, got, tc.want)
		}
	}
}

func TestRenderToolHeaderTruncatesToWidth(t *testing.T) {
	for _, color := range []bool{false, true} {
		withToolTestColor(t, color)
		header := renderToolHeader(toolTestCall("run_bash", `{"command":"`+strings.Repeat("x", 200)+`"}`), 40)
		if style.VisibleWidth(header) > 40 {
			t.Fatalf("color=%v: header is %d cells wide: %q", color, style.VisibleWidth(header), header)
		}
		if !strings.HasSuffix(style.StripANSI(header), style.Glyphs().Ellipsis+")") {
			t.Fatalf("color=%v: truncated header should end with an ellipsis: %q", color, header)
		}
	}
}

func TestRenderToolHeaderColor(t *testing.T) {
	withToolTestColor(t, true)
	header := renderToolHeader(toolTestCall("read_file", `{"path":"main.go"}`), 80)
	if !strings.Contains(header, style.Strong.Style().Open()+"Read") || !strings.Contains(header, style.Accent.Style().Open()) {
		t.Fatalf("header should have a bold name and accent bullet: %q", header)
	}
	if got := style.StripANSI(header); got != "● Read(main.go)" {
		t.Fatalf("stripped header = %q", got)
	}
}

func TestRenderToolResultSummaries(t *testing.T) {
	withToolTestColor(t, false)

	cases := []struct {
		name   string
		call   provider.ToolCall
		output string
		want   string
	}{
		{"read", toolTestCall("read_file", `{"path":"a.txt"}`), "File contents for a.txt:\none\ntwo\nthree\n", "  | Read 3 lines"},
		{"write", toolTestCall("write_file", `{"path":"a.txt","content":"x\ny"}`), "File a.txt written successfully.", "  | Wrote 2 lines to a.txt"},
		{"empty dir", toolTestCall("read_dir", `{"path":"d"}`), "Directory listing for d:\n(empty)", "  | Empty directory"},
		{"no matches", toolTestCall("grep_files", `{"path":".","query":"q"}`), `No matches found for "q" under ..`, `  | No matches found for "q" under ..`},
		{"bash", toolTestCall("run_bash", `{"command":"echo hi"}`), "Bash output for \"echo hi\" in .:\nhi", "  | hi"},
		{"copy", toolTestCall("copy_file", `{}`), "File a copied to b successfully.", "  | File a copied to b successfully."},
	}
	for _, tc := range cases {
		if got := renderToolResult(tc.call, tc.output, nil, 0, 80); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRenderToolResultHidesExtraLines(t *testing.T) {
	withToolTestColor(t, false)
	output := "Bash output for \"seq 20\" in .:\n" + toolTestLines(20)
	got := renderToolResult(toolTestCall("run_bash", `{"command":"seq 20"}`), output, nil, 0, 80)
	lines := strings.Split(got, "\n")
	if lines[0] != "  | line 1" || lines[toolResultPreviewLines] != "  | line 6" {
		t.Fatalf("unexpected preview:\n%s", got)
	}
	if last := lines[len(lines)-1]; last != "  | ... +14 lines" {
		t.Fatalf("last line = %q", last)
	}

	found := renderToolResult(toolTestCall("find_files", `{"path":"."}`), "Found 3 file(s) under .:\na\nb\nc", nil, 0, 80)
	if found != "  | Found 3 file(s)\n  | a\n  | b\n  | c" {
		t.Fatalf("find result = %q", found)
	}
}

func TestRenderToolResultDiff(t *testing.T) {
	output := "Preview for write_file on a.txt:\n--- a.txt\n+++ a.txt\n-old\n+new"
	c := toolTestCall("write_file", `{"path":"a.txt","content":"new","preview":true}`)

	withToolTestColor(t, false)
	plain := renderToolResult(c, output, nil, 0, 80)
	want := "  | Preview, no changes made\n  | --- a.txt\n  | +++ a.txt\n  | -old\n  | +new"
	if plain != want {
		t.Fatalf("plain diff = %q, want %q", plain, want)
	}

	withToolTestColor(t, true)
	colored := renderToolResult(c, output, nil, 0, 80)
	if !strings.Contains(colored, style.DiffAdded.Style().Open()+"+new") || !strings.Contains(colored, style.DiffRemoved.Style().Open()+"-old") {
		t.Fatalf("diff lines should be green and red: %q", colored)
	}
}

func TestRenderToolResultAppliedEdit(t *testing.T) {
	withToolTestColor(t, false)
	c := toolTestCall("edit_file", `{"path":"a.go","old_text":"keep\nold\n","new_text":"keep\nnew\nmore\n"}`)
	got := renderToolResult(c, "File a.go edited successfully. Replaced 1 occurrence(s).", nil, 0, 80)
	want := "  | Added 2 lines, removed 1 line\n  |  keep\n  | -old\n  | +new\n  | +more"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderToolResultErrors(t *testing.T) {
	withToolTestColor(t, false)
	err := errors.New("exit status 1\nBash output for \"false\" in .:\nboom")
	got := renderToolResult(toolTestCall("run_bash", `{"command":"false"}`), "", err, 0, 80)
	if got != "  | Error: exit status 1\n  | boom" {
		t.Fatalf("got %q", got)
	}

	withToolTestColor(t, true)
	colored := renderToolResult(toolTestCall("read_file", `{}`), "", errors.New("no such file"), 0, 80)
	if !strings.Contains(colored, style.Error.Style().Open()+"Error: no such file") {
		t.Fatalf("error should be red: %q", colored)
	}
}

func TestRenderToolResultElapsed(t *testing.T) {
	withToolTestColor(t, false)
	c := toolTestCall("copy_file", `{}`)
	if got := renderToolResult(c, "done", nil, 500*time.Millisecond, 80); got != "  | done" {
		t.Fatalf("sub-second calls should not show elapsed time: %q", got)
	}
	if got := renderToolResult(c, "done", nil, 3200*time.Millisecond, 80); got != "  | done [3.2s]" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderToolOutputEscapesControlChars(t *testing.T) {
	for _, color := range []bool{false, true} {
		withToolTestColor(t, color)
		output := "Bash output for \"x\" in .:\n\x1b[31mred\rhidden"
		got := renderToolResult(toolTestCall("run_bash", `{"command":"x"}`), output, nil, 0, 80)
		if strings.Contains(got, "\r") || strings.Contains(style.StripANSI(got), "\x1b") {
			t.Fatalf("color=%v: control characters reached the terminal: %q", color, got)
		}
		if !strings.Contains(got, `\x1b[31mred\x0dhidden`) {
			t.Fatalf("color=%v: control characters should be shown escaped: %q", color, got)
		}
		if !color && strings.Contains(got, "\x1b") {
			t.Fatalf("plain output contains an escape: %q", got)
		}
	}
}

func TestRenderToolResultClipsLongLines(t *testing.T) {
	withToolTestColor(t, false)
	output := "Bash output for \"x\" in .:\nfirst\n" + strings.Repeat("y", 300)
	for _, line := range strings.Split(renderToolResult(toolTestCall("run_bash", `{}`), output, nil, 0, 60), "\n") {
		if style.VisibleWidth(line) > 60 {
			t.Fatalf("line wider than 60 cells: %q", line)
		}
	}
}

func TestRenderToolResultInterruptedAndTruncatedTree(t *testing.T) {
	withToolTestColor(t, false)
	if got := renderToolResult(toolTestCall("run_bash", `{"command":"x"}`), "", context.Canceled, 0, 80); got != "  | Interrupted" {
		t.Errorf("cancelled call: %q", got)
	}
	tree := "Directory tree for .:\na/\nb/\n(truncated after 2 line(s))"
	if got := renderToolResult(toolTestCall("tree_dir", `{"path":"."}`), tree, nil, 0, 80); !strings.HasPrefix(got, "  | Listed 2 lines\n") {
		t.Errorf("truncated tree: %q", got)
	}
}

func TestRenderToolHeaderLongFlagsFit(t *testing.T) {
	withToolTestColor(t, true)
	c := toolTestCall("run_bash", `{"command":"go test ./... -run SomethingLong","workdir":"internal/some/very/long/nested/directory/path/here"}`)
	for _, width := range []int{40, 20} {
		if got := style.VisibleWidth(renderToolHeader(c, width)); got > width {
			t.Errorf("width %d: header is %d cells", width, got)
		}
	}
}

func TestLogToolCallPlainIsNotClipped(t *testing.T) {
	withToolTestColor(t, false)
	command := "echo " + strings.Repeat("a", 120) + " && rm -rf /tmp/x"
	if got := renderToolHeader(toolTestCall("run_bash", `{"command":"`+command+`"}`), toolRenderWidth()); !strings.Contains(got, "&& rm -rf /tmp/x") {
		t.Errorf("plain header was clipped: %q", got)
	}
}
