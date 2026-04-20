//go:build !darwin && !linux

package main

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
