//go:build !darwin && !linux

package main

import "os"

func hardLinkCount(info os.FileInfo) uint64 {
	return 1
}
