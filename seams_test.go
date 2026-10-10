package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/style"
)

func seamConfig(t *testing.T, p provider.Provider) *config {
	t.Helper()
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, false, true)
	return &config{
		model:           provider.Model{Provider: provider.Anthropic, ID: "claude-opus-5-5"},
		reasoningEffort: provider.EffortMedium,
		providers:       map[string]provider.Provider{provider.Anthropic: p},
		approver:        &bashApprover{},
	}
}

func TestHandleInputRunsPromptsAndCommands(t *testing.T) {
	fake := newFakeAnthropic(t,
		newSSE("msg_1").text("Hello from the model.").stop("end_turn", ""),
		newSSE("msg_2").text("Retried.").stop("end_turn", ""),
	)
	cfg := seamConfig(t, fake.provider(true))
	sessionSaver = func(string, savedSession) error { return nil }
	t.Cleanup(func() { sessionSaver = writeSessionFile })

	out := captureStdout(t, func() {
		if err := handleInput(cfg, "say hello"); err != nil {
			t.Fatal(err)
		}
		if err := handleInput(cfg, "/retry"); err != nil {
			t.Fatal(err)
		}
		if err := handleInput(cfg, "/status"); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"Hello from the model.", "Retried.", "Model:", "Cost:", "Session:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(fake.request(1).raw, "say hello") {
		t.Error("expected /retry to send the last prompt again")
	}
	if err := handleInput(cfg, "/nosuchcommand"); err == nil {
		t.Error("expected an unknown command to fail")
	}
}

func TestSimpleCommands(t *testing.T) {
	cfg := seamConfig(t, nil)
	recordHistory(historyEntry{Type: "user", Content: "x"})
	out := captureStdout(t, func() {
		for _, run := range []func() error{
			func() error { return commandHelp(context.Background(), cfg, nil) },
			func() error { return commandTools(context.Background(), cfg, nil) },
			func() error { return commandHistory(context.Background(), cfg, nil) },
			func() error { return commandApprove(context.Background(), cfg, []string{"on"}) },
			func() error { return commandApprove(context.Background(), cfg, nil) },
			func() error { return commandApprove(context.Background(), cfg, []string{"off"}) },
		} {
			if err := run(); err != nil {
				t.Fatal(err)
			}
		}
	})
	for _, want := range []string{"Conversation", "grep_files", ".goat/history.jsonl", "without asking"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if cfg.approver.autoApprove {
		t.Error("expected /approve off to turn auto-approve off")
	}
	if err := commandApprove(context.Background(), cfg, []string{"maybe"}); err == nil {
		t.Error("expected /approve with a bad value to fail")
	}
}

func TestFooterParts(t *testing.T) {
	cfg := seamConfig(t, nil)
	cfg.approver.autoApprove = true
	parts := strings.Join(footerParts(cfg), " | ")
	if !strings.HasPrefix(parts, "anthropic:claude-opus-5-5 | effort medium") || !strings.Contains(parts, "auto-approve on") || !strings.HasSuffix(parts, "? for shortcuts") {
		t.Fatalf("footer %q", parts)
	}
	chromeWithColor(t, true)
	if footer := style.StripANSI(renderFooter(footerParts(cfg), 40)); strings.Contains(footer, "…") || style.VisibleWidth(footer) >= 40 {
		t.Fatalf("expected parts dropped to fit, got %q", footer)
	}
}

func TestSpinnerDrawsTheQueuedInput(t *testing.T) {
	resetTurnQueue(t)
	chromeWithColor(t, true)
	turnQueue.capturing = true
	turnQueue.items = []string{"queued thing"}
	turnQueue.editor.setText("typing")

	out := captureStdout(t, func() {
		startSpinner("Thinking")
		time.Sleep(30 * time.Millisecond)
		redrawSpinner()
		stopSpinner()
	})
	plain := style.StripANSI(out)
	for _, want := range []string{"Thinking", "queued thing", "typing"} {
		if !strings.Contains(plain, want) {
			t.Errorf("spinner block lacks %q: %q", want, plain)
		}
	}
	// Each redraw moves up over the 3-line block before drawing; the stop clears it.
	if !strings.Contains(out, "\x1b[2A\r\x1b[J") || !strings.HasSuffix(out, "\x1b[2A\r\x1b[J") {
		t.Errorf("expected the block to be erased in place: %q", out)
	}
}
