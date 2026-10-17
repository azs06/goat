package tools

import (
	"os"
	"testing"

	"github.com/azs06/goat/internal/skills"
)

// TestMain keeps the tests' operation logs and snapshots out of the user's own state
// directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "goat-test-state-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_STATE_HOME", dir)
	// Only the workspace's own skills, never the user's.
	skills.HomeDir = func() (string, error) { return "", nil }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
