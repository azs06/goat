//go:build linux

package terminal

import "golang.org/x/sys/unix"

const (
	ioctlReadTermios  = unix.TCGETS
	ioctlWriteTermios = unix.TCSETS
)

func flushInputBuffer(fd int) error {
	return unix.IoctlSetPointerInt(fd, unix.TCFLSH, unix.TCIFLUSH)
}
