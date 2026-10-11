package main

import (
	"context"
	"path/filepath"
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

func TestConfigDirCommandsAlwaysAskFromNearby(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	approver := &bashApprover{autoApprove: true}
	for _, run := range []struct{ command, workdir string }{
		{"cat .env", filepath.Join(home, ".config", "goat")},
		{"cat goat/.env", filepath.Join(home, ".config")},
		{"cat ~/.config//goat/.env", "."},
		{"cat ~/.config/./goat/.env", "."},
	} {
		if err := approver.approve(context.Background(), run.command, run.workdir); err == nil {
			t.Errorf("%q in %s: expected the command to need approval", run.command, run.workdir)
		}
	}
	if err := approver.approve(context.Background(), "cat .env", t.TempDir()); err != nil {
		t.Errorf("a project's own .env is not goat's: %v", err)
	}
}
