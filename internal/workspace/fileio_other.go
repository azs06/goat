//go:build !darwin && !linux

package workspace

import "os"

func hardLinkCount(info os.FileInfo) uint64 {
	return 1
}
