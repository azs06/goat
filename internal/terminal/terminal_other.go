//go:build !darwin && !linux

package terminal

import (
	"errors"
	"os"
)

// InputGuard does nothing on this platform.
type InputGuard struct{}

// BeginInputGuard returns a guard that does nothing on this platform.
func BeginInputGuard(cbreak bool) (*InputGuard, error) {
	return &InputGuard{}, nil
}

// Suspend does nothing on this platform.
func (g *InputGuard) Suspend() error {
	return nil
}

// Resume does nothing on this platform.
func (g *InputGuard) Resume() error {
	return nil
}

// Close does nothing on this platform.
func (g *InputGuard) Close() error {
	return nil
}

// StdinIsTerminal reports whether stdin is a character device.
func StdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// Rows is unknown on this platform.
func Rows() int {
	return 0
}

// Columns is unknown on this platform.
func Columns() int {
	return 0
}

// EnterCbreak is not supported on this platform.
func EnterCbreak() (func(), error) {
	return nil, errors.New("cbreak mode is not supported on this platform")
}
