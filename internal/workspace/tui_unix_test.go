//go:build unix

package workspace

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRunTUITypesAndReadsTheScreen(t *testing.T) {
	t.Chdir(t.TempDir())
	out, err := RunTUI(context.Background(), TUIRun{
		Command: `read -p 'name? ' name; echo "hello $name"`,
		Steps:   []TUIStep{{WaitFor: "name?"}, {Keys: "goat<Enter>", WaitFor: "hello goat"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name? goat\nhello goat") || !strings.Contains(out, "(100x30, exited with status 0)") {
		t.Fatalf("got:\n%s", out)
	}
}

func TestRunTUISnapshotsAndClosesWhatStillRuns(t *testing.T) {
	t.Chdir(t.TempDir())
	out, err := RunTUI(context.Background(), TUIRun{
		Command: "cat",
		Cols:    40,
		Rows:    5,
		Steps:   []TUIStep{{Keys: "one<Enter>", Snapshot: true}, {Keys: "two<Enter>"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"(40x5, still running after the steps; goat closed it)", "After step 1:\none\none\n\nFinal screen:\none\none\ntwo\ntwo", "(cursor at line 5, column 1)"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in:\n%s", want, out)
		}
	}
}

func TestRunTUISendsCursorKeysInTheProgramsMode(t *testing.T) {
	t.Chdir(t.TempDir())
	// The program turns on application cursor keys, as full-screen programs do, then
	// shows the bytes it gets for the Up key.
	out, err := RunTUI(context.Background(), TUIRun{
		Command: `printf '\033[?1h'; IFS= read -rsn3 key; printf 'got %q\n' "$key"`,
		Steps:   []TUIStep{{Keys: "<Up>", WaitFor: "got"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `got $'\EOA'`) {
		t.Fatalf("expected ESC O A, got:\n%s", out)
	}
}

func TestRunTUIFailsWhenTextNeverAppears(t *testing.T) {
	t.Chdir(t.TempDir())
	_, err := RunTUI(context.Background(), TUIRun{Command: "echo ready; sleep 5", Steps: []TUIStep{{WaitFor: "done", WaitMS: 300}}})
	if err == nil || !strings.Contains(err.Error(), `step 1: "done" did not appear within 300ms`) || !strings.Contains(err.Error(), "\nready") {
		t.Fatalf("got %v", err)
	}

	_, err = RunTUI(context.Background(), TUIRun{Command: "echo bye", Steps: []TUIStep{{WaitFor: "hello"}}})
	if err == nil || !strings.Contains(err.Error(), `the program exited without showing "hello"`) {
		t.Fatalf("got %v", err)
	}

	_, err = RunTUI(context.Background(), TUIRun{Command: "true", Steps: []TUIStep{{WaitMS: 500}, {Keys: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "the program exited before step 2") {
		t.Fatalf("got %v", err)
	}
}

func TestRunTUIStopsAtItsTimeoutAndOnCancel(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := tuiTimeout
	tuiTimeout = 500 * time.Millisecond
	t.Cleanup(func() { tuiTimeout = previous })
	start := time.Now()
	_, err := RunTUI(context.Background(), TUIRun{Command: "sleep 30", Steps: []TUIStep{{WaitMS: 20000}}})
	if err == nil || !strings.Contains(err.Error(), "timed out after 500ms") || time.Since(start) > 10*time.Second {
		t.Fatalf("got %v after %s", err, time.Since(start))
	}

	tuiTimeout = previous
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	_, err = RunTUI(ctx, TUIRun{Command: "sleep 30", Steps: []TUIStep{{WaitMS: 20000}}})
	if err == nil || !strings.Contains(err.Error(), "interrupted by the user") {
		t.Fatalf("got %v", err)
	}
}

func TestRunTUIKeepsCredentialsAndGoatsStateAway(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("OPENAI_API_KEY", "sk-secret")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	out, err := RunTUI(context.Background(), TUIRun{Command: `echo "key=[$OPENAI_API_KEY] state=[$XDG_STATE_HOME] term=[$TERM]"`, Cols: 300})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "key=[]") || strings.Contains(out, os.Getenv("XDG_STATE_HOME")) || !strings.Contains(out, "goat-tui-state-") || !strings.Contains(out, "term=[xterm-256color]") {
		t.Fatalf("got:\n%s", out)
	}

	if _, err := RunTUI(context.Background(), TUIRun{Command: "pwd", Workdir: "../elsewhere"}); err == nil {
		t.Fatal("expected a workdir outside the workspace to be refused")
	}
}
