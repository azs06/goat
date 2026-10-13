package subagent

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func request(ctx context.Context, b *ApprovalBroker, cmd string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- b.Request(ctx, &ApprovalRequest{Command: cmd}) }()
	return done
}

func result(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("request did not return")
		return nil
	}
}

func TestBrokerWithdrawsACancelledRequest(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	b := NewApprovalBroker()
	cause := errors.New("stopped")
	ctx, cancel := context.WithCancelCause(context.Background())
	done := request(ctx, b, "rm -rf build")
	eventually(t, "the request", func() bool { return b.Len() == 1 })
	cancel(cause)
	if err := result(t, done); !errors.Is(err, cause) {
		t.Fatalf("got %v", err)
	}
	if b.Len() != 0 || b.Next() != nil {
		t.Fatal("request not withdrawn")
	}
}

func TestBrokerAnswerAfterTheAgentLeftDoesNotBlock(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	b := NewApprovalBroker()
	ctx, cancel := context.WithCancel(context.Background())
	done := request(ctx, b, "make")
	eventually(t, "the request", func() bool { return b.Len() == 1 })
	req := b.Next()
	cancel()
	if err := result(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	req.Answer(nil)
	req.Answer(errors.New("twice"))
}

func TestBrokerServesInOrderAndSkipsCancelled(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	b := NewApprovalBroker()
	var dones []<-chan error
	for i, cmd := range []string{"one", "two", "three"} {
		dones = append(dones, request(context.Background(), b, cmd))
		eventually(t, cmd, func() bool { return b.Len() == i+1 })
	}
	select {
	case <-b.Pending():
	default:
		t.Fatal("no pending nudge")
	}
	// A request whose turn ended but has not withdrawn yet is skipped.
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	b.mu.Lock()
	b.queue = append([]*ApprovalRequest{{Command: "gone", Ctx: gone, reply: make(chan error, 1)}}, b.queue...)
	b.mu.Unlock()
	declined := errors.New("no")
	for i, want := range []string{"one", "two", "three"} {
		req := b.Next()
		if req == nil || req.Command != want {
			t.Fatalf("request %d: %+v", i, req)
		}
		if i == 1 {
			req.Answer(declined)
		} else {
			req.Answer(nil)
		}
	}
	if b.Next() != nil {
		t.Fatal("queue not empty")
	}
	for i, done := range dones {
		err := result(t, done)
		if (i == 1 && !errors.Is(err, declined)) || (i != 1 && err != nil) {
			t.Fatalf("request %d: %v", i, err)
		}
	}
}

func TestBrokerDrainAllAnswersEveryRequest(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	b := NewApprovalBroker()
	first := request(context.Background(), b, "a")
	second := request(context.Background(), b, "b")
	eventually(t, "both requests", func() bool { return b.Len() == 2 })
	b.DrainAll(ErrShutdown)
	if err := result(t, first); !errors.Is(err, ErrShutdown) {
		t.Fatal(err)
	}
	if err := result(t, second); !errors.Is(err, ErrShutdown) {
		t.Fatal(err)
	}
	if b.Len() != 0 {
		t.Fatal("not drained")
	}
}
