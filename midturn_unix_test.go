//go:build unix

package main

import (
	"context"
	"sync/atomic"
	"syscall"
	"testing"

	"go.uber.org/goleak"
)

// TestNotifyInterruptRedirectsCtrlC sends real SIGINTs: one while a redirect is set
// goes to it and leaves the context alone; the next, after it is removed, cancels the
// context. Stopping leaves no goroutine behind.
func TestNotifyInterruptRedirectsCtrlC(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	ctx, stop := notifyInterrupt(context.Background())
	var redirected atomic.Int32
	removeRedirect := redirectInterrupt(func() { redirected.Add(1) })
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the redirect", func() bool { return redirected.Load() == 1 })
	if ctx.Err() != nil {
		t.Fatal("a redirected Ctrl-C cancelled the context")
	}
	removeRedirect()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	if redirected.Load() != 1 {
		t.Fatal("the removed redirect got a Ctrl-C")
	}
	stop()
	stop()
	goleak.VerifyNone(t, ignore)
}
