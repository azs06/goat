//go:build !unix

package workspace

import (
	"errors"
	"os/exec"
)

func configureProcessGroup(cmd *exec.Cmd) {}

// Without process groups, goat cannot tell what a command left running.
func groupAlive(pgid int) bool { return false }

func stopGroup(pgid int) error { return errors.ErrUnsupported }

// GroupProcesses is not supported here.
func GroupProcesses(pgid int) ([]GroupProcess, error) { return nil, errors.ErrUnsupported }
