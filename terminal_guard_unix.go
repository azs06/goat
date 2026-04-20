//go:build darwin || linux

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type terminalInputGuard struct {
	fd       int
	termios  *unix.Termios
	disabled bool
}

func beginTerminalInputGuard() (*terminalInputGuard, error) {
	fd := int(os.Stdin.Fd())
	original, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return &terminalInputGuard{}, nil
	}

	updated := *original
	updated.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, &updated); err != nil {
		return nil, fmt.Errorf("disable terminal echo: %w", err)
	}

	return &terminalInputGuard{
		fd:       fd,
		termios:  original,
		disabled: true,
	}, nil
}

func (g *terminalInputGuard) Close() error {
	if g == nil || !g.disabled {
		return nil
	}

	_ = flushInputBuffer(g.fd)
	if err := unix.IoctlSetTermios(g.fd, ioctlWriteTermios, g.termios); err != nil {
		return fmt.Errorf("restore terminal state: %w", err)
	}
	return nil
}
