package main

import (
	"context"
	"strings"
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

func TestConfigDirCommandsAlwaysAsk(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/srv/cfg")
	approver := &bashApprover{autoApprove: true}
	for _, command := range []string{
		"cat ~/.config/goat/.env",
		"cat $HOME/.config/goat/.env",
		`type %USERPROFILE%\.config\goat\.env`,
		"cat $XDG_CONFIG_HOME/goat/.env",
		"ls /srv/cfg/goat",
	} {
		if err := approver.approve(context.Background(), command, "."); err == nil || !strings.Contains(err.Error(), "API keys") {
			t.Errorf("%q: expected the command to need approval, got %v", command, err)
		}
	}
	if err := approver.approve(context.Background(), "go test ./...", "."); err != nil {
		t.Errorf("an ordinary command should still run: %v", err)
	}
}
