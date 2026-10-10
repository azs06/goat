//go:build !darwin && !linux

package main

import (
	"errors"
	"os"
)

type terminalInputGuard struct{}

func beginTerminalInputGuard(cbreak bool) (*terminalInputGuard, error) {
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

func enterCbreakMode() (func(), error) {
	return nil, errors.New("cbreak mode is not supported on this platform")
}

func stdoutRows() int {
	return 0
}
