package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain keeps the tests' operation logs and snapshots out of the user's own state
// directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "goat-test-state-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_STATE_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// statePath is where goat keeps one of its files for the current directory, such as
// statePath(t, "snapshots").
func statePath(t *testing.T, name string) string {
	t.Helper()
	root, err := StateRoot()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, name)
}
