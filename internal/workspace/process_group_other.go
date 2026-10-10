//go:build !unix

package workspace

import "os/exec"

func configureProcessGroup(cmd *exec.Cmd) {}
