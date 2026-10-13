package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/azs06/goat/internal/workspace"
)

// TestMain keeps the tests' history, sessions and snapshots out of the user's own state
// directory. goat run as a child process gets its own through runGoat.
func TestMain(m *testing.M) {
	if os.Getenv("GOAT_TEST_MAIN") != "" {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "goat-test-state-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_STATE_HOME", dir)
	// A verify command from the shell would run after every test turn that changes files.
	os.Unsetenv("GOAT_VERIFY")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// statePath is where goat keeps one of its files for the current directory, such as
// statePath("history.jsonl").
func statePath(t *testing.T, name string) string {
	t.Helper()
	root, err := workspace.StateRoot()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, name)
}
