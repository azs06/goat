package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/subagent"
	"github.com/azs06/goat/internal/tools"
)

// These tests cover what the user does in the mid-turn attach view to an agent whose
// turn ended while main still follows it: main must get that turn's report whatever
// the user does before /back.

// finishKid lets the held kid finish its spawn turn and waits until it is idle with
// report.
func finishKid(t *testing.T, a *subagent.Agent, hold chan struct{}, report string) {
	t.Helper()
	close(hold)
	waitFor(t, "the kid's report", func() bool { return a.State() == subagent.Idle && a.Report() == report })
}

// spawnResult is main's tool result for its call callID, from its request n.
func spawnResult(t *testing.T, fake *routedAnthropic, n int) string {
	t.Helper()
	reqs := fake.requests("[[main]]")
	if len(reqs) <= n {
		t.Fatalf("main made %d requests", len(reqs))
	}
	return fmt.Sprint(toolResultsOf(t, reqs[n]))
}

func TestMidTurnDismissKeepsTheFollowedAgentsReport(t *testing.T) {
	cfg, h, fake, holds := midTurnSetup(t, kidScripts(
		newSSE("k1").text("kid secret report").stop("end_turn", ""),
	), "[[kid]]")

	var err error
	out := captureTerminal(t, func() {
		result, keys, a := attachMidTurn(t, context.Background(), cfg, h, fake)
		finishKid(t, a, holds["[[kid]]"], "kid secret report")
		// The view takes lines in order, so the dismiss has run before /back.
		io.WriteString(keys, "/agents dismiss a1\r/back\r")
		err = <-result
		stopKeyCapture()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(style.StripANSI(out), "a1 is running, or has messages or a report not yet delivered") {
		t.Errorf("the dismiss should be refused:\n%s", style.StripANSI(out))
	}
	if results := spawnResult(t, fake, 1); !strings.Contains(results, "kid secret report") || strings.Contains(results, "without a report") {
		t.Errorf("main should get the kid's report: %s", results)
	}
	if _, err := h.m.Get("a1"); err != nil {
		t.Fatalf("the followed agent was dismissed before main took its report: %v", err)
	}
	// Once main has the report, the agent can go.
	if err := h.m.Dismiss("a1"); err != nil {
		t.Errorf("the agent should be dismissable once main has its report: %v", err)
	}
}

func TestMidTurnDismissFinishedKeepsAGroupMembersReport(t *testing.T) {
	cfg, h, fake, holds := midTurnSetup(t, map[string][]string{
		"[[main]]": {
			newSSE("m1").
				toolUse("tm1", tools.SpawnAgent, `{"description":"one","prompt":"[[c1]] look"}`).
				toolUse("tm2", tools.SpawnAgent, `{"description":"two","prompt":"[[c2]] look"}`).
				stop("tool_use", ""),
			newSSE("m2").text("Main done.").stop("end_turn", ""),
		},
		"[[c1]]": {newSSE("c1").text("report one").stop("end_turn", "")},
		"[[c2]]": {newSSE("c2").text("report two").stop("end_turn", "")},
	}, "[[c1]]", "[[c2]]")

	var err error
	out := captureTerminal(t, func() {
		input, keys := keyPipe(t)
		cfg.input = input
		startKeyCapture(context.Background(), input)
		t.Cleanup(stopKeyCapture)
		result := runPromptAsync(context.Background(), cfg, "[[main]] ask both")
		waitFor(t, "both requests", func() bool {
			return len(fake.requests("[[c1]]")) == 1 && len(fake.requests("[[c2]]")) == 1
		})
		a2 := agentByID(t, h, "a2")
		// Main pauses in the attach view to a1 while a2 finishes.
		turnQueue.mu.Lock()
		turnQueue.items = append(turnQueue.items, "/agents a1")
		turnQueue.mu.Unlock()
		nudgeLineQueued()
		waitFor(t, "the attach view", func() bool { return h.m.Attached() == "a1" })
		close(holds["[[c2]]"])
		waitFor(t, "a2's report", func() bool { return a2.State() == subagent.Idle && a2.Report() == "report two" })
		io.WriteString(keys, "/agents dismiss finished\r/back\r")
		waitFor(t, "the attach view to close", func() bool { return h.m.Attached() == "" })
		close(holds["[[c1]]"])
		err = <-result
		stopKeyCapture()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(style.StripANSI(out), "No finished subagent to dismiss.") {
		t.Errorf("nothing should be dismissed:\n%s", style.StripANSI(out))
	}
	results := spawnResult(t, fake, 1)
	if !strings.Contains(results, "report one") || !strings.Contains(results, "report two") || strings.Contains(results, "without a report") {
		t.Errorf("main should get both reports: %s", results)
	}
	if _, err := h.m.Get("a2"); err != nil {
		t.Fatalf("a2 was dismissed before main took its report: %v", err)
	}
	if ids := h.m.DismissFinished(); len(ids) != 2 {
		t.Errorf("both agents should be dismissable once main has their reports: %v", ids)
	}
}

func TestMidTurnMessageToAFinishedFollowedAgentWaitsForMain(t *testing.T) {
	cfg, h, fake, holds := midTurnSetup(t, kidScripts(
		newSSE("k1").text("kid report").stop("end_turn", ""),
		newSSE("k2").text("chit chat answer").stop("end_turn", ""),
	), "[[kid]]")

	var err error
	var a *subagent.Agent
	out := captureTerminal(t, func() {
		var result <-chan error
		var keys *io.PipeWriter
		result, keys, a = attachMidTurn(t, context.Background(), cfg, h, fake)
		finishKid(t, a, holds["[[kid]]"], "kid report")
		io.WriteString(keys, "what did you mean?\r")
		waitFor(t, "the message held in the inbox", func() bool { return a.Info().Inbox == 1 })
		if n := len(fake.requests("[[kid]]")); n != 1 || a.State() != subagent.Idle {
			t.Errorf("the message should wait for main to take the report: %d kid requests, %s", n, a.State())
		}
		io.WriteString(keys, "/back\r")
		err = <-result
		stopKeyCapture()
		// Main has the report; the held message starts the user's turn.
		waitFor(t, "the user's turn", func() bool { return a.State() == subagent.Idle && a.Report() == "chit chat answer" })
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(style.StripANSI(out), "a1's report is on its way to main; the message starts a turn once main has it.") {
		t.Errorf("the attach view should say the message waits:\n%s", style.StripANSI(out))
	}
	results := spawnResult(t, fake, 1)
	if !strings.Contains(results, "kid report") || strings.Contains(results, "chit chat answer") {
		t.Errorf("main should get the spawn's report, not the user's chat: %s", results)
	}
	if kid := fake.requests("[[kid]]"); len(kid) != 2 || !strings.Contains(kid[1].raw, "what did you mean?") {
		t.Errorf("the held message should reach the kid after main took the report")
	}
	if n := h.m.NoticeCount(); n != 0 {
		t.Errorf("the user's turn is not reported to the model: %d notices", n)
	}
}

func TestMidTurnStopOfAFinishedFollowedAgentKeepsItsReport(t *testing.T) {
	cfg, h, fake, holds := midTurnSetup(t, kidScripts(
		newSSE("k1").text("kid report").stop("end_turn", ""),
	), "[[kid]]")

	var err error
	captureTerminal(t, func() {
		result, keys, a := attachMidTurn(t, context.Background(), cfg, h, fake)
		finishKid(t, a, holds["[[kid]]"], "kid report")
		io.WriteString(keys, "/agents stop a1\r")
		waitFor(t, "the stop", func() bool { return a.State() == subagent.Stopped })
		io.WriteString(keys, "/back\r")
		err = <-result
		stopKeyCapture()
	})
	if err != nil {
		t.Fatal(err)
	}
	if results := spawnResult(t, fake, 1); !strings.Contains(results, "kid report") || strings.Contains(results, "stopped") {
		t.Errorf("main should get the finished turn's report: %s", results)
	}
}
