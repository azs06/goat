package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/azs06/goat/internal/subagent"
	"github.com/azs06/goat/internal/tools"
)

// /approve off reaches the subagents, even after goat was started with --yes; it also
// ends the "always" answered to any of them.
func TestApproveOffAsksAgainForSubagents(t *testing.T) {
	withColor(t, false)
	cfg := &config{approver: &bashApprover{}}
	// As --yes or GOAT_AUTO_APPROVE_BASH sets it.
	cfg.approver.autoApprove.Store(true)
	cfg.approver.sessionAutoApprove.Store(true)
	h := newAgentHost(cfg, subagent.Config{}, false)
	cfg.agents = h
	t.Cleanup(func() { h.m.StopAll(3 * time.Second) })
	a, err := h.m.Spawn(subagent.SpawnOptions{Label: "general", Mode: "general", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	approve := h.childApprover(a)
	if err := approve(context.Background(), "rm -rf build", "."); err != nil {
		t.Fatalf("with auto-approval on the command runs: %v", err)
	}

	captureTerminal(t, func() {
		if err := commandApprove(context.Background(), cfg, []string{"off"}); err != nil {
			t.Fatal(err)
		}
	})
	if err := approve(context.Background(), "rm -rf build", "."); !errors.Is(err, errBashCommandDeclined) {
		t.Fatalf("after /approve off the subagent's command must be asked about: %v", err)
	}

	// "always" answered to the agent ends with /approve off too.
	a.AutoApprove.Store(true)
	captureTerminal(t, func() { _ = commandApprove(context.Background(), cfg, []string{"off"}) })
	if a.AutoApprove.Load() {
		t.Fatal("/approve off should end the agent's always")
	}

	captureTerminal(t, func() { _ = commandApprove(context.Background(), cfg, []string{"on"}) })
	if err := approve(context.Background(), "rm -rf build", "."); err != nil {
		t.Fatalf("/approve on reaches the subagent: %v", err)
	}
}

// An approval waits while the user is typing a message in the attach view: the rest of
// the line would become its answer. Once the line is emptied it is asked.
func TestAttachApprovalWaitsWhileTextIsTyped(t *testing.T) {
	_, cfg, h, a := attachTestSetup(t)
	lines := &scriptedLines{t: t, lines: []string{"y\n"}}
	cfg.input = newLineReader(lines)
	// Asking resumes the capture, which then reads the script's end.
	setActiveKeyCapture(&keyCapture{input: cfg.input, parent: context.Background()})
	answered := make(chan error, 1)
	attachForTest(t, context.Background(), h, cfg, a, func() {
		typeQueueKeys("now al")
		go func() {
			answered <- h.m.Broker.Request(context.Background(), &subagent.ApprovalRequest{AgentID: a.ID, Label: a.Label, Command: "make test"})
		}()
		time.Sleep(300 * time.Millisecond)
		if lines.reads.Load() != 0 || h.m.Broker.Len() != 1 {
			t.Errorf("the approval was asked while text was typed")
		}
		typeQueueKeys(strings.Repeat("\x7f", len("now al")))
		select {
		case err := <-answered:
			if err != nil {
				t.Errorf("the approval should be answered once the line is empty: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the approval was not asked once the line was emptied")
		}
		enqueue("/back")
		nudgeLineQueued()
	})
}

// An agent's approval is not asked while the user has text typed at the prompt, even
// when another agent finishing made the prompt give way: what the user goes on typing
// would become the answer.
func TestIdleApprovalWaitsWhileTextIsTyped(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[bg]]": {
			newSSE("g1").toolUse("tg1", "run_bash", `{"command":"echo bg > bg.txt"}`).stop("tool_use", ""),
			newSSE("g2").text("bg done").stop("end_turn", ""),
		},
	})
	cfg, h := agentTestConfig(t, fake, subagent.Config{}, true)
	cfg.input = newLineReader(&scriptedLines{t: t, lines: []string{"y\n"}})
	if _, err := h.Spawn(context.Background(), tools.SpawnRequest{Description: "background job", Prompt: "[[bg]] write it", Mode: tools.AgentModeGeneral, Background: true}); err != nil {
		t.Fatal(err)
	}
	a, _ := h.m.Get("a1")
	waitFor(t, "the approval request", func() bool { return a.State() == subagent.WaitingApproval })
	cfg.input.partial = "half a sentence"
	captureTerminal(t, func() { serveIdleApprovals(cfg) })
	if a.State() != subagent.WaitingApproval || cfg.input.partial != "half a sentence" {
		t.Fatalf("the approval was asked over typed text: %v %q", a.State(), cfg.input.partial)
	}
}
