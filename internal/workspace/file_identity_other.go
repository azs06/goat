//go:build !unix

package workspace

import "os"

func fileIdentity(info os.FileInfo) string {
	return ""
}
