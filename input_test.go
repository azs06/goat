package main

import (
	"context"
	"testing"
)

func TestStateDirCommandsAlwaysAsk(t *testing.T) {
	approver := &bashApprover{autoApprove: true}
	if err := approver.approve(context.Background(), "ls", "."); err != nil {
		t.Fatalf("expected an ordinary command to run, got %v", err)
	}
	if err := approver.approve(context.Background(), "rm -rf .goat/snapshots", "."); err == nil {
		t.Fatal("expected a command touching .goat to need approval")
	}
}
