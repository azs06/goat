//go:build !unix

package main

import "os/exec"

func configureProcessGroup(cmd *exec.Cmd) {}
