package tools

import (
	"os"
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
