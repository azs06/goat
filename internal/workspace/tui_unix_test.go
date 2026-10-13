//go:build unix

package workspace

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hinshun/vt10x"
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

func TestRunTUIEndsTheJobsOfAnInteractiveShell(t *testing.T) {
	t.Chdir(t.TempDir())
	// bash -i puts a background job in a process group of its own; cancelling the call
	// must end it along with the shell.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jobs := make(chan string, 1)
	go func() {
		out, err := RunTUI(ctx, TUIRun{
			Command: `HISTFILE=/dev/null bash --norc --noprofile -i`,
			Steps: []TUIStep{
				{Keys: `sleep 4242 & echo job=started<Enter>`, WaitFor: "job=started", WaitMS: 5000},
				{WaitMS: 20000},
			},
		})
		jobs <- fmt.Sprint(out, err)
	}()

	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for pid == 0 && time.Now().Before(deadline) {
		out, _ := exec.Command("pgrep", "-f", "sleep 4242").Output()
		if fields := strings.Fields(string(out)); len(fields) > 0 {
			pid, _ = strconv.Atoi(fields[0])
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pid == 0 {
		cancel()
		t.Fatalf("the background job never started:\n%s", <-jobs)
	}
	cancel()
	<-jobs
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("the shell's background job %d outlived the call", pid)
}

func TestRunTUIStopsWhileTypingIntoAProgramThatStoppedReading(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := tuiTimeout
	tuiTimeout = 3 * time.Second
	t.Cleanup(func() { tuiTimeout = previous })
	// In raw mode the terminal's input buffer fills and then blocks the writer; the
	// program never reads it.
	marker := fmt.Sprintf("sleep %d", 4000+os.Getpid()%1000)
	start := time.Now()
	_, err := RunTUI(context.Background(), TUIRun{
		Command: `stty raw -echo; echo ready; ` + marker,
		Steps:   []TUIStep{{Keys: strings.Repeat("x", 200_000)}},
	})
	if err == nil || !strings.Contains(err.Error(), "timed out after 3s") || time.Since(start) > 10*time.Second {
		t.Fatalf("got %v after %s", err, time.Since(start))
	}
	if out, _ := exec.Command("pgrep", "-f", marker).Output(); len(strings.TrimSpace(string(out))) > 0 {
		exec.Command("pkill", "-f", marker).Run()
		t.Fatalf("%q outlived the call: %s", marker, out)
	}
}

func TestRunTUIEndsTheJobsAShellLeftWhenItExited(t *testing.T) {
	t.Chdir(t.TempDir())
	// A background job of an interactive shell that exits normally loses the terminal
	// but stays in the shell's session.
	marker := fmt.Sprintf("sleep %d", 5000+os.Getpid()%1000)
	out, err := RunTUI(context.Background(), TUIRun{
		Command: `HISTFILE=/dev/null bash --norc --noprofile -i`,
		Steps: []TUIStep{
			{Keys: marker + ` & echo job=started<Enter>`, WaitFor: "job=started", WaitMS: 5000},
			{Keys: `exit<Enter>`},
		},
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "exited with status") {
		t.Fatalf("expected the shell to exit by itself:\n%s", out)
	}
	if out, _ := exec.Command("pgrep", "-f", marker).Output(); len(strings.TrimSpace(string(out))) > 0 {
		exec.Command("pkill", "-f", marker).Run()
		t.Fatalf("the job %q outlived its shell: %s", marker, out)
	}
}

// byteReader gives out one byte per Read, as a terminal may split a character's bytes.
type byteReader struct{ data []byte }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

func TestTUIOutputKeepsCharactersSplitBetweenReads(t *testing.T) {
	session := &tuiSession{screen: vt10x.New(vt10x.WithSize(20, 2)), read: make(chan struct{})}
	session.copyOutput(&byteReader{data: []byte("é ❯ 🐐 ok")})
	if got := session.text(); !strings.Contains(got, "é ❯ 🐐 ok") {
		t.Fatalf("got %q", got)
	}
	for _, test := range []struct {
		in   string
		want int
	}{{"ab", 2}, {"a\xc3", 1}, {"a\xe2\x9d", 1}, {"\xe2\x9d\xaf", 3}, {"a\xff", 2}, {"", 0}} {
		if got := completeRunes([]byte(test.in)); got != test.want {
			t.Errorf("completeRunes(%q) = %d, want %d", test.in, got, test.want)
		}
	}
}

func TestStopAllBackgroundEndsATerminalSessionsJobs(t *testing.T) {
	t.Chdir(t.TempDir())
	// goat closing mid-call (SIGTERM, SIGHUP) stops what is running through
	// StopAllBackground; a run_tui shell's jobs are in groups of their own.
	marker := fmt.Sprintf("sleep %d", 6000+os.Getpid()%1000)
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunTUI(context.Background(), TUIRun{
			Command: `HISTFILE=/dev/null bash --norc --noprofile -i`,
			Steps: []TUIStep{
				{Keys: marker + ` & echo job=started<Enter>`, WaitFor: "job=started", WaitMS: 5000},
				{WaitFor: "never shown", WaitMS: 30000},
			},
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if out, _ := exec.Command("pgrep", "-f", marker).Output(); len(strings.TrimSpace(string(out))) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the job never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	StopAllBackground()
	if out, _ := exec.Command("pgrep", "-f", marker).Output(); len(strings.TrimSpace(string(out))) > 0 {
		exec.Command("pkill", "-f", marker).Run()
		t.Fatalf("the job %q outlived goat's shutdown: %s", marker, out)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run_tui did not return after its session was ended")
	}
}
