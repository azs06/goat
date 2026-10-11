//go:build !windows

package main

import (
	"context"
	"os/exec"
)

// shellCommand runs command with sh, as a line typed at a shell prompt.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}
