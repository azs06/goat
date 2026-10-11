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

// stopGracePeriod is how long stopped groups get to exit after SIGTERM before they are
// killed. It is a variable so tests can shorten it.
var stopGracePeriod = 3 * time.Second

// groupAlive reports whether a group goat recorded still has processes in it. EPERM
// means one does, running as another user. The shell that led the group has exited and
// been waited for, so a process leading a group with that ID again means the ID now
// belongs to another group, which goat must never signal.
func groupAlive(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	if err := syscall.Kill(-pgid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	if leaderGroup, err := syscall.Getpgid(pgid); err == nil && leaderGroup == pgid {
		return false
	}
	return true
}

// runningGroupAlive is groupAlive for the group of a command still running, whose
// shell is its leader.
func runningGroupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return pgid > 1 && (err == nil || errors.Is(err, syscall.EPERM))
}

// stopGroups sends SIGTERM to every group, waits up to stopGracePeriod for them all to
// exit, then sends SIGKILL to what is left. It returns the groups still there after
// that.
func stopGroups(pgids []int) []int {
	alive := func(pgid int) bool { return groupAlive(pgid) || isRunning(pgid) && runningGroupAlive(pgid) }
	var waiting []int
	for _, pgid := range pgids {
		if pgid > 1 && alive(pgid) {
			syscall.Kill(-pgid, syscall.SIGTERM)
			waiting = append(waiting, pgid)
		}
	}
	waiting = waitForGroups(waiting, alive, stopGracePeriod)
	for _, pgid := range waiting {
		syscall.Kill(-pgid, syscall.SIGKILL)
	}
	return waitForGroups(waiting, alive, time.Second)
}

func isRunning(pgid int) bool {
	background.mu.Lock()
	defer background.mu.Unlock()
	return background.running[pgid]
}

// waitForGroups waits up to timeout for the groups to exit and returns those left.
func waitForGroups(pgids []int, alive func(int) bool, timeout time.Duration) []int {
	deadline := time.Now().Add(timeout)
	for {
		var left []int
		for _, pgid := range pgids {
			if alive(pgid) {
				left = append(left, pgid)
			}
		}
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}
		pgids = left
		time.Sleep(25 * time.Millisecond)
	}
}

func errStillRunning(pgid int) error {
	return fmt.Errorf("process group %d is still running after SIGKILL", pgid)
}

// GroupProcesses lists the processes left in a background group, from ps.
func GroupProcesses(pgid int) ([]GroupProcess, error) {
	// ps -g selects by process group on macOS but by session on Linux, so every
	// process is listed and filtered here. "args" is the name every ps knows,
	// BusyBox's included.
	out, err := exec.Command("ps", "-A", "-o", "pid=,pgid=,etime=,args=").Output()
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
