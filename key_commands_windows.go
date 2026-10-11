package main

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

// shellCommand runs command with cmd.exe. cmd.exe does not unquote arguments the way
// other programs do, so the whole command line is given as it should be read
// (see os/exec.Command).
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = "cmd.exe"
	}
	cmd := exec.CommandContext(ctx, comspec)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `"` + comspec + `" /d /s /c "` + command + `"`}
	return cmd
}
