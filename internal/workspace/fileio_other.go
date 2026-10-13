//go:build !darwin && !linux && !windows

package workspace

import "os"

func hardLinkCount(string, os.FileInfo) uint64 {
	return 1
}
