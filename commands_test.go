package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/azs06/goat/internal/workspace"
)

func TestUndoSkipGivesUpOnAnOperation(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	if err := workspace.WriteFile("a.txt", "1"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := workspace.WriteFile("b.txt", "2"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := commandUndo(context.Background(), &config{}, []string{"skip"}); err != nil {
		t.Fatalf("undo skip: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "b.txt")); err != nil {
		t.Fatalf("expected skip to leave b.txt alone, got %v", err)
	}
	if _, err := workspace.UndoLastChange(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected a.txt to be undone after the skip, got %v", err)
	}
	if err := commandUndo(context.Background(), &config{}, []string{"bogus"}); err == nil {
		t.Fatal("expected an unknown undo option to fail")
	}
}
