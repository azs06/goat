//go:build darwin || linux

package main

import (
	"os"
	"syscall"
)

func hardLinkCount(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Nlink)
	}
	return 1
}
