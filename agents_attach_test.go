package main

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azs06/goat/internal/subagent"
)

// attachForTest opens the attach view of a with key capture on, runs during while it
// is open, and returns once it closes.
func attachForTest(t *testing.T, ctx context.Context, h *agentHost, cfg *config, a *subagent.Agent, during func()) {
	t.Helper()
	done := make(chan error, 1)
	captureTerminal(t, func() {
		go func() { done <- h.attach(ctx, cfg, a) }()
		waitFor(t, "the attach view", func() bool {
			turnQueue.mu.Lock()
			defer turnQueue.mu.Unlock()
			return turnQueue.emptyHint != ""
		})
		during()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
}

// A line half typed for main when the attach view opens (Ctrl-G) is set aside: what is
// typed in the view goes to the agent alone, and the draft comes back for main.
func TestAttachSetsAsideTheDraftForMain(t *testing.T) {
	runner, cfg, h, a := attachTestSetup(t)
	turnQueue.mu.Lock()
	turnQueue.editor.setText("draft for main")
	turnQueue.mu.Unlock()
	attachForTest(t, context.Background(), h, cfg, a, func() {
		turnQueue.mu.Lock()
		text := turnQueue.editorText()
		turnQueue.mu.Unlock()
		if text != "" {
			t.Errorf("the attach view's input holds main's draft: %q", text)
		}
		enqueue("hello")
		nudgeLineQueued()
		waitFor(t, "the message", func() bool { return len(runner.seen()) == 2 })
		enqueue("/back")
		nudgeLineQueued()
	})
	if seen := runner.seen(); len(seen) != 2 || seen[1] != "hello" {
		t.Fatalf("the agent should get only what was typed in the view: %q", seen)
	}
	turnQueue.mu.Lock()
	text := turnQueue.editorText()
	turnQueue.mu.Unlock()
	if text != "draft for main" {
		t.Fatalf("the draft should come back for main: %q", text)
	}
}

// Ctrl-C out of the attach view sends nothing: lines queued for main wait in the
// prompt, as after any other Ctrl-C.
func TestCtrlCOutOfAttachKeepsMainsLinesInThePrompt(t *testing.T) {
	runner, cfg, h, a := attachTestSetup(t)
	enqueue("deploy it")
	ctx, cancel := context.WithCancel(context.Background())
	attachForTest(t, ctx, h, cfg, a, cancel)
	if len(runner.seen()) != 1 {
		t.Fatalf("a line meant for main went to the agent: %q", runner.seen())
	}
	if text, ok := nextQueuedInput(); ok {
		t.Fatalf("Ctrl-C should not leave the line queued to send: %q", text)
	}
	turnQueue.mu.Lock()
	text := turnQueue.editorText()
	turnQueue.mu.Unlock()
	if text != "deploy it" {
		t.Fatalf("the line should wait in the input: %q", text)
	}
}

// Lines queued for main before the attach view opened stay main's.
func TestAttachLeavesLinesQueuedForMain(t *testing.T) {
	withColor(t, true)
	resetTurnQueue(t)
	saved := currentKeyCapture()
	setActiveKeyCapture(&keyCapture{})
	t.Cleanup(func() { setActiveKeyCapture(saved) })
	runner := &echoRunner{}
	cfg, h := uiTestHost(t, runner.run)
	a := spawnTest(t, h, "helper", "first")
	waitFor(t, "a1 idle", func() bool { return a.State() == subagent.Idle })

	enqueue("now fix the test")
	done := make(chan error, 1)
	captureTerminal(t, func() {
		go func() { done <- h.attach(context.Background(), cfg, a) }()
		waitFor(t, "the attach view", func() bool {
			turnQueue.mu.Lock()
			defer turnQueue.mu.Unlock()
			return len(turnQueue.items) == 0
		})
		time.Sleep(20 * time.Millisecond)
		enqueue("/back")
		nudgeLineQueued()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	if seen := runner.seen(); len(seen) != 1 {
		t.Fatalf("a line meant for main went to the agent: %q", seen)
	}
	if text, ok := nextQueuedInput(); !ok || text != "now fix the test" {
		t.Fatalf("the line should still go to main: %q %v", text, ok)
	}
}

// waitChan is closed once cond holds.
func waitChan(cond func() bool) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for !cond() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if cond() {
			close(done)
		}
	}()
	return done
}

// An agent event while a paste is coming in waits for the paste to end: the rest of it
// would otherwise be typed into the next prompt and its newline would send the line.
func TestIdlePromptDoesNotGiveWayInTheMiddleOfAPaste(t *testing.T) {
	withColor(t, true)
	noCbreak(t)
	input, keys := keyPipe(t)
	changed := make(chan struct{}, 1)
	var checks, fire, footers atomic.Int32
	events := &promptEvents{
		changed: changed,
		interrupt: func(empty bool) bool {
			checks.Add(1)
			return fire.Load() == 1
		},
		footer: func() string { footers.Add(1); return "FOOTER" },
	}
	result := editInBackground(t, context.Background(), input, events)
	waitFor(t, "the prompt", func() bool { return checks.Load() == 1 })
	io.WriteString(keys, pasteStart+"rm -rf build")
	// Wake the prompt until a wake finds it inside the paste: interrupt is only asked
	// outside one, so a redraw with no check means the paste has begun.
	inPaste := false
	for range 50 {
		before, redraws := checks.Load(), footers.Load()
		changed <- struct{}{}
		waitFor(t, "the redraw", func() bool { return footers.Load() > redraws })
		if checks.Load() == before {
			inPaste = true
			break
		}
	}
	if !inPaste {
		t.Fatal("the paste never began")
	}
	// Now an event arrives with the paste still coming in.
	fire.Store(1)
	redraws := footers.Load()
	changed <- struct{}{}
	select {
	case r := <-result:
		t.Fatalf("the prompt gave way mid-paste: %v, partial %q", r.err, input.partial)
	case <-time.After(5 * time.Second):
		t.Fatal("the wake was not handled")
	case <-waitChan(func() bool { return footers.Load() > redraws }):
	}
	fire.Store(0)
	io.WriteString(keys, "\rsecond"+pasteEnd+"\r")
	if r := <-result; r.err != nil || r.text != "rm -rf build\nsecond" {
		t.Fatalf("got %v %q", r.err, r.text)
	}
}

func attachTestSetup(t *testing.T) (*echoRunner, *config, *agentHost, *subagent.Agent) {
	withColor(t, true)
	resetTurnQueue(t)
	saved := currentKeyCapture()
	setActiveKeyCapture(&keyCapture{})
	t.Cleanup(func() { setActiveKeyCapture(saved) })
	runner := &echoRunner{}
	cfg, h := uiTestHost(t, runner.run)
	a := spawnTest(t, h, "helper", "first")
	waitFor(t, "a1 idle", func() bool { return a.State() == subagent.Idle })
	return runner, cfg, h, a
}
