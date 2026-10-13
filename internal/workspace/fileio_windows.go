package workspace

import (
	"os"
	"syscall"
)

// hardLinkCount asks Windows for the file's link count, which its FileInfo lacks.
func hardLinkCount(path string, _ os.FileInfo) uint64 {
	file, err := os.Open(path)
	if err != nil {
		return 1
	}
	defer file.Close()
	var data syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &data); err != nil {
		return 1
	}
	return uint64(data.NumberOfLinks)
}
