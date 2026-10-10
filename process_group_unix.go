//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
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
