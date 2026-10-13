//go:build unix

package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
)

// TUIAvailable reports whether run_tui can run here: it needs Unix pseudo-terminals.
const TUIAvailable = true

const (
	// tuiSettleIdle is how long the program must go without output, and goat without
	// typing, before goat counts the screen as settled: before typing, and before the
	// final screen is read.
	tuiSettleIdle = 200 * time.Millisecond
	// tuiSettleMax is the longest goat waits for that: for a program that never stops
	// drawing (a clock, a spinner), or one that shows nothing until it gets input. A
	// login shell can take a while to read its profile, so until the program first
	// writes something, goat keeps waiting for that, up to this long.
	tuiSettleMax = 3 * time.Second
	// tuiPoll is how often goat looks at the screen while waiting.
	tuiPoll = 25 * time.Millisecond
	// tuiExitGrace is how long a program gets to exit after the last step typed keys,
	// which are often the ones that tell it to quit, before goat reads the final screen.
	tuiExitGrace = 2 * time.Second
)

// RunTUI runs command in a pseudo-terminal of run.Cols by run.Rows inside the workspace,
// with credentials removed from its environment, types each step's keys and returns the
// screen. The program gets a scratch XDG_STATE_HOME, so a goat run this way keeps its
// history and session away from the goat running it. Whatever is still running after
// the steps is hung up on and then killed.
func RunTUI(ctx context.Context, run TUIRun) (string, error) {
	if err := checkTUIRun(&run); err != nil {
		return "", err
	}
	if strings.TrimSpace(run.Workdir) == "" {
		run.Workdir = "."
	}
	resolvedDir, relDir, err := ResolvePath(run.Workdir)
	if err != nil {
		return "", err
	}
	stateDir, err := os.MkdirTemp("", "goat-tui-state-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stateDir)

	runCtx, cancel := context.WithTimeout(ctx, tuiTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "bash", "-lc", run.Command)
	cmd.Dir = resolvedDir
	cmd.Env = append(CommandEnvironment(os.Environ()),
		"TERM=xterm-256color",
		"COLUMNS="+strconv.Itoa(run.Cols),
		"LINES="+strconv.Itoa(run.Rows),
		"XDG_STATE_HOME="+stateDir,
	)
	configureProcessGroup(cmd)
	// goat ends the whole terminal session itself once the steps stop, timed out or
	// not (endSession); killing only the shell's group here first would leave the jobs
	// of an interactive shell running, no longer attached to the terminal.
	cmd.Cancel = func() error { return nil }
	terminal, ttyName, err := startInTerminal(cmd, run.Cols, run.Rows)
	if err != nil {
		return "", fmt.Errorf("start %q in a terminal: %w", run.Command, err)
	}
	trackRunning(cmd.Process.Pid)
	defer untrackRunning(cmd.Process.Pid)

	session := &tuiSession{
		screen: vt10x.New(vt10x.WithSize(run.Cols, run.Rows), vt10x.WithWriter(terminal)),
		input:  terminal,
		exited: make(chan struct{}),
		read:   make(chan struct{}),
	}
	session.lastActivity.Store(time.Now().UnixNano())
	go session.copyOutput(terminal)
	go func() {
		session.waitErr = cmd.Wait()
		close(session.exited)
	}()

	report, stepErr := session.runSteps(runCtx, run.Steps)

	still := !session.hasExited()
	endSession(cmd.Process.Pid, ttyName, session.exited)
	terminal.Close()
	// A process that left the group can still hold the terminal open and keep the read
	// going; goat does not wait for it.
	select {
	case <-session.read:
	case <-time.After(time.Second):
	}

	header := fmt.Sprintf("Terminal screen for %q in %s (%dx%d", run.Command, relDir, run.Cols, run.Rows)
	switch {
	case still:
		header += ", still running after the steps; goat closed it"
	case session.waitErr == nil:
		header += ", exited with status 0"
	default:
		var exitErr *exec.ExitError
		if errors.As(session.waitErr, &exitErr) && exitErr.ExitCode() >= 0 {
			header += fmt.Sprintf(", exited with status %d", exitErr.ExitCode())
		} else {
			header += ", " + session.waitErr.Error()
		}
	}
	result := header + "):\n" + report

	switch {
	case ctx.Err() != nil:
		return "", fmt.Errorf("run_tui interrupted by the user\n%s", result)
	case runCtx.Err() != nil:
		return "", fmt.Errorf("run_tui timed out after %s and was killed\n%s", tuiTimeout, result)
	case stepErr != nil:
		return "", fmt.Errorf("%w\n%s", stepErr, result)
	}
	return result, nil
}

// tuiSession is a program running in the emulated terminal.
type tuiSession struct {
	screen vt10x.Terminal
	input  *os.File
	// lastActivity is when the program last wrote anything or goat last typed, in Unix
	// nanoseconds; wrote is whether the program has written anything yet.
	lastActivity atomic.Int64
	wrote        atomic.Bool
	exited       chan struct{}
	waitErr      error
	// read is closed once the program's output stops: it exited, or goat closed the
	// terminal.
	read chan struct{}
}

// copyOutput feeds the program's output to the screen until it ends. A character whose
// UTF-8 bytes are split between two reads waits for the rest: the emulator would drop
// the part it got.
func (s *tuiSession) copyOutput(output io.Reader) {
	defer close(s.read)
	buf := make([]byte, 32*1024)
	var pending []byte
	for {
		n, err := output.Read(buf)
		if n > 0 {
			data := append(pending, buf[:n]...)
			whole := completeRunes(data)
			s.screen.Write(data[:whole])
			pending = append([]byte(nil), data[whole:]...)
			s.lastActivity.Store(time.Now().UnixNano())
			s.wrote.Store(true)
		}
		if err != nil {
			if len(pending) > 0 {
				s.screen.Write(pending)
			}
			return
		}
	}
}

// completeRunes is how much of p ends on a whole UTF-8 character; what follows is the
// start of one whose other bytes have not arrived yet.
func completeRunes(p []byte) int {
	for i := len(p) - 1; i >= 0 && i >= len(p)-utf8.UTFMax; i-- {
		if utf8.RuneStart(p[i]) {
			if utf8.FullRune(p[i:]) {
				return len(p)
			}
			return i
		}
	}
	return len(p)
}

func (s *tuiSession) hasExited() bool {
	select {
	case <-s.exited:
		return true
	default:
		return false
	}
}

// runSteps takes the steps in order and returns the screens to report: each step's
// snapshot, then the final screen. It stops at the first step that fails or once the
// program has exited, and returns why.
func (s *tuiSession) runSteps(ctx context.Context, steps []TUIStep) (string, error) {
	var report strings.Builder
	var stepErr error
	for i, step := range steps {
		if step.Keys != "" {
			s.settle(ctx)
			if s.hasExited() {
				stepErr = fmt.Errorf("the program exited before step %d could type %q", i+1, step.Keys)
				break
			}
			if err := s.typeKeys(step.Keys); err != nil {
				stepErr = fmt.Errorf("step %d: %w", i+1, err)
				break
			}
		}
		if err := s.wait(ctx, step); err != nil {
			stepErr = fmt.Errorf("step %d: %w", i+1, err)
			break
		}
		if ctx.Err() != nil {
			break
		}
		if step.Snapshot {
			s.settle(ctx)
			fmt.Fprintf(&report, "After step %d:\n%s\n\n", i+1, s.text())
		}
	}
	if stepErr == nil && len(steps) > 0 && steps[len(steps)-1].Keys != "" {
		select {
		case <-s.exited:
		case <-ctx.Done():
		case <-time.After(tuiExitGrace):
		}
	}
	s.settle(ctx)
	if report.Len() > 0 {
		report.WriteString("Final screen:\n")
	}
	report.WriteString(s.text())
	s.screen.Lock()
	cursor, visible := s.screen.Cursor(), s.screen.CursorVisible()
	s.screen.Unlock()
	if visible {
		fmt.Fprintf(&report, "\n(cursor at line %d, column %d)", cursor.Y+1, cursor.X+1)
	}
	return report.String(), stepErr
}

func (s *tuiSession) typeKeys(keys string) error {
	parsed, err := parseTUIKeys(keys)
	if err != nil {
		return err
	}
	for _, key := range parsed {
		s.screen.Lock()
		appCursor := s.screen.Mode()&vt10x.ModeAppCursor != 0
		s.screen.Unlock()
		if _, err := s.input.Write([]byte(key.encode(appCursor))); err != nil {
			return fmt.Errorf("type keys: %w", err)
		}
		s.lastActivity.Store(time.Now().UnixNano())
		if key.name != "" {
			// A named key often changes the mode the next one is sent in.
			time.Sleep(tuiPoll)
		}
	}
	return nil
}

// wait does a step's waiting: until its wait_for text is on the screen, or for wait_ms.
func (s *tuiSession) wait(ctx context.Context, step TUIStep) error {
	limit := time.Duration(step.WaitMS) * time.Millisecond
	if step.WaitFor == "" {
		if limit > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(limit):
			}
		}
		return nil
	}
	if limit == 0 {
		limit = defaultTUIWait
	}
	deadline := time.Now().Add(limit)
	for {
		if strings.Contains(s.text(), step.WaitFor) {
			return nil
		}
		if s.hasExited() {
			// Let the last output arrive before deciding.
			s.settle(ctx)
			if strings.Contains(s.text(), step.WaitFor) {
				return nil
			}
			return fmt.Errorf("the program exited without showing %q", step.WaitFor)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%q did not appear within %s", step.WaitFor, limit)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(tuiPoll):
		}
	}
}

// settle waits until the program has written something and then gone tuiSettleIdle
// without writing (or goat typing), for up to tuiSettleMax, or until it has exited and
// its output is read.
func (s *tuiSession) settle(ctx context.Context) {
	deadline := time.Now().Add(tuiSettleMax)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if s.wrote.Load() && time.Since(time.Unix(0, s.lastActivity.Load())) >= tuiSettleIdle {
			return
		}
		select {
		case <-s.read:
			return
		case <-ctx.Done():
			return
		case <-time.After(tuiPoll):
		}
	}
}

func (s *tuiSession) text() string {
	return screenText(s.screen.String())
}

// startInTerminal starts cmd as the leader of a new session whose controlling terminal
// is a new pseudo-terminal of cols by rows. It returns the terminal's controlling side
// and the name of the program's side, such as /dev/ttys004.
func startInTerminal(cmd *exec.Cmd, cols, rows int) (*os.File, string, error) {
	terminal, tty, err := pty.Open()
	if err != nil {
		return nil, "", err
	}
	defer tty.Close()
	if err := pty.Setsize(terminal, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		terminal.Close()
		return nil, "", err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	if err := cmd.Start(); err != nil {
		terminal.Close()
		return nil, "", err
	}
	return terminal, tty.Name(), nil
}

// endSession ends whatever still runs on the terminal, the way closing a terminal
// window would: SIGHUP to every process group on it, then SIGKILL to the groups left
// after a second. A job-control shell (bash -i) puts each background job in a group of
// its own, so the group leader leads is not enough. The others are found by the
// terminal they are attached to, before anything is signalled, since a job loses its
// terminal once the shell leading the session exits.
func endSession(leader int, tty string, exited <-chan struct{}) {
	groups := terminalGroups(leader, tty)
	alive := func(pgid int) bool {
		err := syscall.Kill(-pgid, 0)
		return err == nil || errors.Is(err, syscall.EPERM)
	}
	for _, pgid := range groups {
		if alive(pgid) {
			syscall.Kill(-pgid, syscall.SIGHUP)
		}
	}
	left := waitForGroups(groups, alive, time.Second)
	for _, pgid := range left {
		syscall.Kill(-pgid, syscall.SIGKILL)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
	}
}

// terminalGroups lists the process groups attached to the terminal named tty, leader's
// first. Without ps (or a ps without -t), it is leader's alone.
func terminalGroups(leader int, tty string) []int {
	groups := []int{leader}
	out, err := exec.Command("ps", "-t", strings.TrimPrefix(tty, "/dev/"), "-o", "pgid=").Output()
	if err != nil {
		return groups
	}
	for _, field := range strings.Fields(string(out)) {
		pgid, err := strconv.Atoi(field)
		if err == nil && pgid > 1 && !slices.Contains(groups, pgid) {
			groups = append(groups, pgid)
		}
	}
	return groups
}
