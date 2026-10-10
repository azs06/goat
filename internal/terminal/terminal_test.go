package terminal

import (
	"os"
	"testing"
)

// withPipeStdin replaces stdin with a pipe for the rest of the test, so the results do
// not depend on how the tests were started.
func withPipeStdin(t *testing.T) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = previous
		r.Close()
		w.Close()
	})
}

func TestGuardOnAPipeDoesNothing(t *testing.T) {
	withPipeStdin(t)
	if StdinIsTerminal() {
		t.Fatal("a pipe is not a terminal")
	}
	guard, err := BeginInputGuard(true)
	if err != nil {
		t.Fatalf("BeginInputGuard: %v", err)
	}
	for name, step := range map[string]func() error{"Suspend": guard.Suspend, "Resume": guard.Resume, "Close": guard.Close} {
		if err := step(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestNilGuardIsSafe(t *testing.T) {
	var guard *InputGuard
	if err := guard.Suspend(); err != nil {
		t.Errorf("Suspend: %v", err)
	}
	if err := guard.Resume(); err != nil {
		t.Errorf("Resume: %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestCbreakNeedsATerminal(t *testing.T) {
	withPipeStdin(t)
	if restore, err := EnterCbreak(); err == nil {
		restore()
		t.Fatal("expected cbreak mode to fail on a pipe")
	}
}
