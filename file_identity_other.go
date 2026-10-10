//go:build !unix

package main

import "os"

func fileIdentity(info os.FileInfo) string {
	return ""
}
