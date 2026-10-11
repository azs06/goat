package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/azs06/goat/internal/style"
)

func TestShellEscapeRunsAndGoesWithTheNextMessage(t *testing.T) {
	fake := newFakeAnthropic(t,
		newSSE("msg_1").text("Seen it.").stop("end_turn", ""),
		newSSE("msg_2").text("Again.").stop("end_turn", ""),
	)
	cfg := seamConfig(t, fake.provider(true))
	withColor(t, false)
	cfg.interactive = true
	sessionSaver = func(string, savedSession) error { return nil }
	t.Cleanup(func() { sessionSaver = writeSessionFile })

	out := captureStdout(t, func() {
		if err := handleInput(cfg, "!echo shell-said-hi; false"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "shell-said-hi") || !strings.Contains(out, "exit status 1") {
		t.Fatalf("expected the output and the exit status:\n%s", out)
	}
	if fake.requestCount() != 0 {
		t.Fatal("a ! command must not call the model")
	}
	if part := pendingShellFooterPart(cfg); part != "1 shell result for the next message" {
		t.Fatalf("footer part %q", part)
	}

	if err := handleInput(cfg, "why did it fail?"); err != nil {
		t.Fatal(err)
	}
	first := fake.request(0).raw
	if !strings.Contains(first, "shell-said-hi") || !strings.Contains(first, "ran these shell commands") || !strings.Contains(first, "why did it fail?") {
		t.Fatalf("expected the shell output before the prompt: %s", first)
	}
	if err := handleInput(cfg, "and now?"); err != nil {
		t.Fatal(err)
	}
	// The second request replays the first message, note included, but adds no new one.
	if n := strings.Count(fake.request(1).raw, "ran these shell commands"); n != 1 || len(cfg.pendingShell) != 0 {
		t.Fatalf("the shell output goes with one message only; found %d notes", n)
	}

	// /output shows the run like a tool call.
	entries, _ := loadConversationHistoryEntries()
	runs := recentToolRuns(entries)
	if len(runs) == 0 || runs[0].call.Status != "user" || !strings.Contains(runs[0].output.Content, "shell-said-hi") {
		t.Fatalf("expected the run in history, got %+v", runs)
	}
}

func TestShellEscapeNeedsATerminalOrAutoApproval(t *testing.T) {
	cfg := seamConfig(t, newFakeAnthropic(t).provider(true))
	if err := handleInput(cfg, "!echo nope"); err == nil || !strings.Contains(err.Error(), "-y") {
		t.Fatalf("expected piped ! to be refused, got %v", err)
	}
	if err := handleInput(cfg, "!"); err == nil {
		t.Fatal("expected an empty command to be refused")
	}
	cfg.approver.autoApprove = true
	out := captureStdout(t, func() {
		if err := handleInput(cfg, "!echo allowed"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "allowed") || strings.ContainsAny(out, "\x1b\r") {
		t.Fatalf("got %q", out)
	}
}

func TestOneShotPromptIsNeverAShellCommand(t *testing.T) {
	fake := newFakeAnthropic(t, newSSE("msg_1").text("ok").stop("end_turn", ""))
	cfg := seamConfig(t, fake.provider(true))
	cfg.oneShot = true
	cfg.approver.autoApprove = true
	sessionSaver = func(string, savedSession) error { return nil }
	t.Cleanup(func() { sessionSaver = writeSessionFile })
	captureStdout(t, func() {
		if err := handleInput(cfg, "!echo from-a-file"); err != nil {
			t.Fatal(err)
		}
	})
	if fake.requestCount() != 1 || !strings.Contains(fake.request(0).raw, "!echo from-a-file") {
		t.Fatal("expected a -p prompt starting with ! to go to the model")
	}
}

func TestShellEscapeStopsOnCtrlC(t *testing.T) {
	cfg := seamConfig(t, newFakeAnthropic(t).provider(true))
	withColor(t, false)
	cfg.interactive = true
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	var err error
	out := captureStdout(t, func() { err = runShellEscape(ctx, cfg, "sleep 30") })
	if !errors.Is(err, context.Canceled) || !strings.Contains(out, "Interrupted") || time.Since(started) > 10*time.Second {
		t.Fatalf("got %v after %s:\n%s", err, time.Since(started), out)
	}
	if len(cfg.pendingShell) != 1 || !strings.Contains(cfg.pendingShell[0], "Stopped by the user") {
		t.Fatalf("got %q", cfg.pendingShell)
	}
}

func TestPromptEditorShowsTheShellHint(t *testing.T) {
	withColor(t, true)
	e := &promptEditor{}
	typeKeys(t, e, "!", "l", "s")
	plain := style.StripANSI(e.render("FOOTER", 60))
	if strings.Contains(plain, "FOOTER") || !strings.Contains(plain, "workspace shell") {
		t.Fatalf("expected the shell hint in place of the footer: %q", plain)
	}
	if w := style.VisibleWidth(renderShellHint(30)); w > 29 {
		t.Fatalf("hint is %d wide", w)
	}
}

func TestShellOutputIsNotSentTwiceAfterAStoppedReply(t *testing.T) {
	fake := newFakeAnthropic(t,
		newSSE("msg_1").text("Cut off").stop("max_tokens", ""),
		newSSE("msg_2").text("ok").stop("end_turn", ""),
	)
	cfg := seamConfig(t, fake.provider(true))
	withColor(t, false)
	cfg.interactive = true
	sessionSaver = func(string, savedSession) error { return nil }
	t.Cleanup(func() { sessionSaver = writeSessionFile })
	captureStdout(t, func() {
		if err := handleInput(cfg, "!echo marker-77"); err != nil {
			t.Fatal(err)
		}
		handleInput(cfg, "first")
		if err := handleInput(cfg, "second"); err != nil {
			t.Fatal(err)
		}
	})
	if n := strings.Count(fake.request(1).raw, "ran these shell commands"); n != 1 {
		t.Fatalf("expected the note once, in the replayed first message; found %d", n)
	}
}
