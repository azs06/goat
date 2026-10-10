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

	guard := &terminalInputGuard{fd: fd, termios: original}
	if err := guard.disableEcho(); err != nil {
		return nil, err
	}
	guard.disabled = true
	return guard, nil
}

func stdinIsTerminal() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), ioctlReadTermios)
	return err == nil
}

func (g *terminalInputGuard) disableEcho() error {
	updated := *g.termios
	updated.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(g.fd, ioctlWriteTermios, &updated); err != nil {
		return fmt.Errorf("disable terminal echo: %w", err)
	}
	return nil
}

// suspend discards input typed while echo was off and restores the original terminal
// state so the user can answer a prompt; resume turns echo off again.
func (g *terminalInputGuard) suspend() error {
	if g == nil || !g.disabled {
		return nil
	}

	_ = flushInputBuffer(g.fd)
	if err := unix.IoctlSetTermios(g.fd, ioctlWriteTermios, g.termios); err != nil {
		return fmt.Errorf("restore terminal state: %w", err)
	}
	return nil
}

func (g *terminalInputGuard) resume() error {
	if g == nil || !g.disabled {
		return nil
	}
	return g.disableEcho()
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
