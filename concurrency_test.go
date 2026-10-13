package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/terminal"
)

// hammer runs each of work in its own goroutines many times over, for the race
// detector to check.
func hammer(work ...func(i int)) {
	var wg sync.WaitGroup
	for _, w := range work {
		for g := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range 200 {
					w(g*200 + i)
				}
			}()
		}
	}
	wg.Wait()
}

func TestInputWaitTotalIsSafeToShare(t *testing.T) {
	before := inputWaitTotal.Load()
	hammer(
		func(int) { inputWaitTotal.Add(int64(time.Microsecond)) },
		func(int) { _ = time.Duration(inputWaitTotal.Load()) },
	)
	if got := inputWaitTotal.Load() - before; got != int64(800*time.Microsecond) {
		t.Fatalf("expected 800µs added, got %s", time.Duration(got))
	}
}

func TestAutoApproveIsSafeToShare(t *testing.T) {
	approver := &bashApprover{}
	hammer(
		func(i int) { approver.autoApprove.Store(i%2 == 0) },
		func(int) { _ = approver.approve(context.Background(), "ls", ".") },
	)
	approver.autoApprove.Store(true)
	if err := approver.approve(context.Background(), "ls", "."); err != nil {
		t.Fatalf("expected auto-approval, got %v", err)
	}
}

func TestVerifyCommandsAreSafeToShare(t *testing.T) {
	_, workDir := verifyConfigTest(t)
	hammer(
		func(i int) { setVerifyCommands(map[string]string{workDir: "make test", "/elsewhere": "npm test"}) },
		func(int) { verifyCommand() },
		func(int) { lookupVerifyCommand(workDir) },
		func(int) { _ = snapshotVerifyCommands() },
	)
	if command, _ := verifyCommand(); command != "make test" {
		t.Fatalf("got %q", command)
	}
}

func TestProviderCacheIsSafeToShare(t *testing.T) {
	c := &config{providers: map[string]provider.Provider{provider.OpenAI: nil}}
	hammer(
		func(int) {
			if _, err := c.provider(provider.OpenAI); err != nil {
				t.Error(err)
			}
		},
		func(int) { c.cachedProvider(provider.DeepSeek) },
		func(int) { c.forgetProviders(func(name string) bool { return name == provider.DeepSeek }) },
	)
	if _, cached := c.providers[provider.OpenAI]; !cached {
		t.Fatal("expected the cached client to stay")
	}
}

func TestActiveTerminalStateIsSafeToShare(t *testing.T) {
	savedCapture, savedGuard := currentKeyCapture(), currentInputGuard()
	t.Cleanup(func() {
		setActiveKeyCapture(savedCapture)
		setActiveInputGuard(savedGuard)
	})
	capture, guard := &keyCapture{}, &terminal.InputGuard{}
	hammer(
		func(i int) {
			if i%2 == 0 {
				setActiveKeyCapture(capture)
			} else {
				setActiveKeyCapture(nil)
			}
		},
		func(i int) {
			if i%2 == 0 {
				setActiveInputGuard(guard)
			} else {
				setActiveInputGuard(nil)
			}
		},
		func(int) { _ = currentKeyCapture() },
		func(int) { _ = currentInputGuard() },
	)
}
