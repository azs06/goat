//go:build unix

package main

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/azs06/goat/internal/workspace"
)

// startBackgroundCommand runs a command through run_bash's runner that leaves a sleep
// running in the background, and returns the sleep's pid.
func startBackgroundCommand(t *testing.T, seconds string) int {
	t.Helper()
	if _, err := workspace.RunBash(context.Background(), "sleep "+seconds+" >/dev/null 2>&1 & echo $! > bg.pid", "."); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("bg.pid")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

// waitForBackground waits for the recorded commands to settle at want, since a login
// profile can leave a short-lived helper behind each command.
func waitForBackground(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(workspace.BackgroundGroups()) != want {
		if time.Now().After(deadline) {
			t.Fatalf("expected %d background commands, got %+v", want, workspace.BackgroundGroups())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestProcessesCommandListsAndStopsWhatGoatLeftRunning(t *testing.T) {
	cfg := seamConfig(t, newFakeAnthropic(t).provider(true))
	withColor(t, false)
	t.Cleanup(func() { workspace.StopAllBackground() })

	out := captureStdout(t, func() {
		if err := commandProcesses(context.Background(), cfg, nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "Nothing goat started is running") || backgroundFooterPart() != "" {
		t.Fatalf("got %q", out)
	}

	first := startBackgroundCommand(t, "31")
	second := startBackgroundCommand(t, "32")
	waitForBackground(t, 2)
	if part := backgroundFooterPart(); part != "2 commands running (/ps)" {
		t.Fatalf("footer part %q", part)
	}

	// Without a terminal, /ps lists them, with their processes, for /ps stop.
	out = captureStdout(t, func() {
		if err := commandProcesses(context.Background(), cfg, nil); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"1. sleep 31", "2. sleep 32", "/ps stop <n>"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing lacks %q:\n%s", want, out)
		}
	}
	if strings.ContainsAny(out, "\x1b\r") {
		t.Errorf("plain listing has escapes: %q", out)
	}

	for _, args := range [][]string{{"stop"}, {"stop", "3"}, {"kill", "1"}} {
		if err := commandProcesses(context.Background(), cfg, args); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}

	out = captureStdout(t, func() {
		if err := commandProcesses(context.Background(), cfg, []string{"stop", "2"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "Stopped sleep 32") || syscall.Kill(second, 0) == nil || syscall.Kill(first, 0) != nil {
		t.Fatalf("expected only the second to stop:\n%s", out)
	}

	stopBackgroundOnExit(io.Discard)
	if syscall.Kill(first, 0) == nil || len(workspace.BackgroundGroups()) != 0 {
		t.Fatal("expected exiting to stop the rest")
	}
}

func TestPipedPSReportsNothingRunning(t *testing.T) {
	out, _, code := runGoat(t, t.TempDir(), "/ps\n/exit\n", []string{"OPENAI_API_KEY=test"})
	if code != 0 || !strings.Contains(out, "Nothing goat started is running in the background.") || strings.Contains(out, "\x1b") {
		t.Fatalf("exit %d:\n%q", code, out)
	}
}
