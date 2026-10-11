//go:build unix

package workspace

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// configureProcessGroup runs cmd in a new session, which is also a new process group,
// so cancelling it kills every process it started, not just the shell. Without a
// controlling terminal, commands that prompt on /dev/tty (sudo, ssh passphrases) fail
// right away instead of stopping in the background until the timeout.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

// stopGracePeriod is how long a stopped group gets to exit after SIGTERM before it is
// killed. It is a variable so tests can shorten it.
var stopGracePeriod = 3 * time.Second

// groupAlive reports whether any process is left in the process group. EPERM means
// one is, running as another user.
func groupAlive(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// stopGroup sends SIGTERM to the group, then SIGKILL if it is still there after
// stopGracePeriod.
func stopGroup(pgid int) error {
	if pgid <= 1 {
		return errUnknownGroup
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if waitForGroupExit(pgid, stopGracePeriod) {
		return nil
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if waitForGroupExit(pgid, time.Second) {
		return nil
	}
	return fmt.Errorf("process group %d is still running after SIGKILL", pgid)
}

func waitForGroupExit(pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for groupAlive(pgid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
	return true
}

// GroupProcesses lists the processes left in a background group, from ps.
func GroupProcesses(pgid int) ([]GroupProcess, error) {
	// ps -g selects by process group on macOS but by session on Linux, so every
	// process is listed and filtered here.
	out, err := exec.Command("ps", "-A", "-o", "pid=,pgid=,etime=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	var processes []GroupProcess
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		group, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil || group != pgid {
			continue
		}
		command := strings.TrimSpace(line)
		for range 3 {
			_, command, _ = strings.Cut(strings.TrimSpace(command), " ")
		}
		processes = append(processes, GroupProcess{PID: pid, Elapsed: fields[2], Command: strings.TrimSpace(command)})
	}
	return processes, nil
}
