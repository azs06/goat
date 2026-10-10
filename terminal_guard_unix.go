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
	// cbreak also turns off line buffering, so keys typed during the turn can be read
	// one at a time.
	cbreak bool
}

func beginTerminalInputGuard(cbreak bool) (*terminalInputGuard, error) {
	fd := int(os.Stdin.Fd())
	original, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return &terminalInputGuard{}, nil
	}

	guard := &terminalInputGuard{fd: fd, termios: original, cbreak: cbreak}
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
	if g.cbreak {
		updated.Lflag &^= unix.ICANON
		updated.Cc[unix.VMIN] = 1
		updated.Cc[unix.VTIME] = 0
	}
	if err := unix.IoctlSetTermios(g.fd, ioctlWriteTermios, &updated); err != nil {
		return fmt.Errorf("disable terminal echo: %w", err)
	}
	return nil
}

// suspend discards input not yet read and restores the original terminal
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

// stdoutColumns is the width of the terminal stdout writes to, or 0 if unknown.
func stdoutColumns() int {
	size, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	return int(size.Col)
}

// enterCbreakMode turns off line buffering and echo on stdin so the prompt editor sees
// each key as it is typed. ISIG stays on, so Ctrl-C still raises SIGINT. The returned
// function restores the previous state.
func enterCbreakMode() (func(), error) {
	fd := int(os.Stdin.Fd())
	original, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return nil, err
	}
	updated := *original
	updated.Lflag &^= unix.ICANON | unix.ECHO
	updated.Cc[unix.VMIN] = 1
	updated.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, &updated); err != nil {
		return nil, fmt.Errorf("enter cbreak mode: %w", err)
	}
	return func() {
		if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, original); err != nil {
			warnf("failed to restore terminal input state: %v", err)
		}
	}, nil
}
