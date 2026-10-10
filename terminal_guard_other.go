//go:build !darwin && !linux

package main

import "os"

type terminalInputGuard struct{}

func beginTerminalInputGuard() (*terminalInputGuard, error) {
	return &terminalInputGuard{}, nil
}

func flushInputBuffer(fd int) error {
	return nil
}

func (g *terminalInputGuard) Close() error {
	return nil
}

func (g *terminalInputGuard) suspend() error {
	return nil
}

func (g *terminalInputGuard) resume() error {
	return nil
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func stdoutColumns() int {
	return 0
}
