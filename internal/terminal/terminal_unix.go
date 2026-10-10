//go:build darwin || linux

package terminal

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// InputGuard keeps what the user types during a turn from echoing into the
// transcript. A guard for a non-terminal stdin does nothing.
type InputGuard struct {
	fd       int
	termios  *unix.Termios
	disabled bool
	// cbreak also turns off line buffering, so keys typed during the turn can be read
	// one at a time.
	cbreak bool
}

// BeginInputGuard turns off echo on stdin, and with cbreak also line buffering, until
// the guard is closed.
func BeginInputGuard(cbreak bool) (*InputGuard, error) {
	fd := int(os.Stdin.Fd())
	original, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return &InputGuard{}, nil
	}

	guard := &InputGuard{fd: fd, termios: original, cbreak: cbreak}
	if err := guard.disableEcho(); err != nil {
		return nil, err
	}
	guard.disabled = true
	return guard, nil
}

// StdinIsTerminal reports whether stdin is a terminal.
func StdinIsTerminal() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), ioctlReadTermios)
	return err == nil
}

func (g *InputGuard) disableEcho() error {
	updated := *g.termios
	updated.Lflag &^= unix.ECHO
	if g.cbreak {
		updated.Lflag &^= unix.ICANON
		updated.Iflag &^= unix.ICRNL
		updated.Cc[unix.VMIN] = 1
		updated.Cc[unix.VTIME] = 0
	}
	if err := unix.IoctlSetTermios(g.fd, ioctlWriteTermios, &updated); err != nil {
		return fmt.Errorf("disable terminal echo: %w", err)
	}
	return nil
}

// Suspend discards input not yet read and restores the original terminal state so the
// user can answer a prompt; Resume turns echo off again.
func (g *InputGuard) Suspend() error {
	if g == nil || !g.disabled {
		return nil
	}

	_ = flushInputBuffer(g.fd)
	if err := unix.IoctlSetTermios(g.fd, ioctlWriteTermios, g.termios); err != nil {
		return fmt.Errorf("restore terminal state: %w", err)
	}
	return nil
}

// Resume undoes Suspend.
func (g *InputGuard) Resume() error {
	if g == nil || !g.disabled {
		return nil
	}
	return g.disableEcho()
}

// Close discards input typed during the turn and restores the original terminal
// state.
func (g *InputGuard) Close() error {
	return g.Suspend()
}

// Rows is the height of the terminal stdout writes to, or 0 if unknown.
func Rows() int {
	size, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	return int(size.Row)
}

// Columns is the width of the terminal stdout writes to, or 0 if unknown.
func Columns() int {
	size, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	return int(size.Col)
}

// EnterCbreak turns off line buffering and echo on stdin so the prompt editor sees
// each key as it is typed. Enter arrives as "\r", told apart from Ctrl-J's "\n". ISIG
// stays on, so Ctrl-C still raises SIGINT. The returned function restores the previous
// state.
func EnterCbreak() (func(), error) {
	fd := int(os.Stdin.Fd())
	original, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return nil, err
	}
	updated := *original
	updated.Lflag &^= unix.ICANON | unix.ECHO
	updated.Iflag &^= unix.ICRNL
	updated.Cc[unix.VMIN] = 1
	updated.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, &updated); err != nil {
		return nil, fmt.Errorf("enter cbreak mode: %w", err)
	}
	return func() {
		if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, original); err != nil {
			Warnf("failed to restore terminal input state: %v", err)
		}
	}, nil
}
