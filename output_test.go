package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

func outputTestHistory(t *testing.T) {
	t.Helper()
	workspace := t.TempDir()
	chdirForTest(t, workspace)
	for _, entry := range []historyEntry{
		{Type: "user", Content: "look around"},
		{Type: "tool_call", ToolName: "run_bash", Arguments: `{"command":"seq 3"}`, ResponseID: "r1"},
		{Type: "tool_call", ToolName: "read_file", Arguments: `{"path":"a.txt"}`, ResponseID: "r1"},
		{Type: "tool_output", ToolName: "run_bash", Content: "Bash output\n1\n2\n3", ResponseID: "r1", Status: "ok"},
		{Type: "tool_output", ToolName: "read_file", Content: "File contents for a.txt:\nhello ```fence```", ResponseID: "r1", Status: "ok"},
		{Type: "assistant", Content: "All done.\nTwo lines.", ResponseID: "r2"},
	} {
		recordHistory(entry)
	}
}

func TestRecentToolRunsAndOutput(t *testing.T) {
	outputTestHistory(t)
	entries, _ := loadConversationHistoryEntries()
	runs := recentToolRuns(entries)
	if len(runs) != 2 || runs[0].call.ToolName != "read_file" || !strings.Contains(runs[1].output.Content, "1\n2\n3") {
		t.Fatalf("unexpected runs %+v", runs)
	}

	cfg := &config{}
	out := captureStdout(t, func() {
		if err := commandOutput(context.Background(), cfg, []string{"2"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "seq 3") || !strings.Contains(out, "1\n2\n3") {
		t.Fatalf("expected the full bash output, got %q", out)
	}
	if err := commandOutput(context.Background(), cfg, []string{"9"}); err == nil {
		t.Fatal("expected an out-of-range number to fail")
	}
}

func TestCopyAndExport(t *testing.T) {
	outputTestHistory(t)
	var copied string
	previous := clipboardWriter
	clipboardWriter = func(text string) (string, error) { copied = text; return "test", nil }
	t.Cleanup(func() { clipboardWriter = previous })

	cfg := &config{}
	captureStdout(t, func() {
		if err := commandCopy(context.Background(), cfg, nil); err != nil {
			t.Fatal(err)
		}
		if err := commandExport(context.Background(), cfg, []string{"notes/talk.md"}); err != nil {
			t.Fatal(err)
		}
	})
	if copied != "All done.\nTwo lines." {
		t.Fatalf("copied %q", copied)
	}
	data, err := os.ReadFile("notes/talk.md")
	if err != nil {
		t.Fatal(err)
	}
	md := string(data)
	for _, want := range []string{"## You\n\nlook around", "## goat\n\nAll done.", "**Bash(seq 3)**", "````\nFile contents"} {
		if !strings.Contains(md, want) {
			t.Errorf("export lacks %q:\n%s", want, md)
		}
	}
	if err := commandExport(context.Background(), cfg, []string{".goat/x.md"}); err == nil {
		t.Error("expected exporting into .goat to fail")
	}
}

// captureStdout returns what fn prints to stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	done := make(chan string)
	go func() {
		data, _ := io.ReadAll(reader)
		done <- string(data)
	}()
	defer func() { os.Stdout = original }()
	fn()
	writer.Close()
	os.Stdout = original
	return <-done
}
