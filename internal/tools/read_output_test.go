package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/azs06/goat/internal/provider"
)

func readOutput(t *testing.T, source OutputSource, args string) (string, error) {
	t.Helper()
	return Execute(context.Background(), provider.ToolCall{Name: "read_output", Arguments: args}, Options{PlanMode: true, Outputs: source})
}

func TestReadOutputReadsAPartAtATime(t *testing.T) {
	var lines []string
	for i := 1; i <= 1000; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	source := func(n int) (string, string, error) {
		if n != 2 {
			return "", "", errors.New("no such output")
		}
		return "Bash(make)", strings.Join(lines, "\n"), nil
	}

	out, err := readOutput(t, source, `{"output": 2}`)
	if err != nil || !strings.HasPrefix(out, "Output 2, Bash(make) (lines 1-300 of 1000):\nline 1\n") || !strings.HasSuffix(out, "line 300\n(more follows; continue with offset 301)") {
		t.Fatalf("default part: %v\n%s", err, out)
	}
	out, err = readOutput(t, source, `{"output": 2, "offset": 999, "limit": 50}`)
	if err != nil || out != "Output 2, Bash(make) (lines 999-1000 of 1000):\nline 999\nline 1000" {
		t.Fatalf("last part: %v %q", err, out)
	}
	if _, err := readOutput(t, source, `{"output": 2, "offset": 1001}`); err == nil || !strings.Contains(err.Error(), "past the end") {
		t.Fatalf("offset past the end: %v", err)
	}
	if _, err := readOutput(t, source, `{"output": 3}`); err == nil || !strings.Contains(err.Error(), "no such output") {
		t.Fatalf("unknown output: %v", err)
	}
	if _, err := readOutput(t, nil, `{"output": 1}`); err == nil || !strings.Contains(err.Error(), "not kept") {
		t.Fatalf("no source: %v", err)
	}
}

func TestReadOutputCapsItsLines(t *testing.T) {
	var lines []string
	for i := 1; i <= 2000; i++ {
		lines = append(lines, fmt.Sprintf("%d", i))
	}
	source := func(int) (string, string, error) { return "Bash(seq)", strings.Join(lines, "\n"), nil }
	out, err := readOutput(t, source, `{"output": 1, "limit": 1000}`)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(out, "\n")
	want := fmt.Sprintf("(more follows; continue with offset %d)", ReadOutputMaxLines+1)
	if len(got) != ReadOutputMaxLines+2 || got[len(got)-2] != fmt.Sprint(ReadOutputMaxLines) || got[len(got)-1] != want {
		t.Fatalf("expected %d lines ending at line %d, got %d lines ending %q", ReadOutputMaxLines, ReadOutputMaxLines, len(got)-2, got[len(got)-2:])
	}
}

func TestReadOutputStaysWithinItsBudget(t *testing.T) {
	long := strings.Repeat("x", 5000)
	source := func(int) (string, string, error) {
		return "Bash(x)", strings.Join([]string{long, long, long, strings.Repeat("y", 3*readOutputCharBudget)}, "\n"), nil
	}
	out, err := readOutput(t, source, `{"output": 1}`)
	if err != nil || !strings.Contains(out, "(lines 1-2 of 4)") || !strings.HasSuffix(out, "continue with offset 3)") {
		t.Fatalf("expected two of the long lines, got %v %.200q", err, out)
	}
	out, _ = readOutput(t, source, `{"output": 1, "offset": 4}`)
	if len(out) > readOutputCharBudget+200 || !strings.Contains(out, "line cut after") {
		t.Fatalf("expected a single huge line to be cut, got %d bytes", len(out))
	}
}

func TestRunBashTakesItsOwnTimeout(t *testing.T) {
	t.Chdir(t.TempDir())
	run := func(args string) (string, error) {
		return Execute(context.Background(), provider.ToolCall{Name: "run_bash", Arguments: args}, Options{})
	}
	if _, err := run(`{"command": "sleep 5", "timeout": 1}`); err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Fatalf("expected the 1 second timeout, got %v", err)
	}
	// Ctrl-C still stops a command given a long timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := Execute(ctx, provider.ToolCall{Name: "run_bash", Arguments: `{"command": "sleep 5", "timeout": 600}`}, Options{}); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("expected the command to be interrupted, got %v", err)
	}
	// A timeout past the maximum is held to it rather than refused.
	if out, err := run(`{"command": "echo ok", "timeout": 100000}`); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("got %q %v", out, err)
	}
}
