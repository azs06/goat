//go:build unix

package workspace

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity returns a string that identifies the file itself rather than its name,
// or "" when it cannot be determined. The link count is included so that a new file
// that reused a freed inode number, and gained hard links of its own, does not match.
func fileIdentity(info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d", stat.Dev, stat.Ino, stat.Nlink)
}
