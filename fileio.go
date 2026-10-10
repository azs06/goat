package main

import (
	"os"
	"path/filepath"
)

// writeFileAtomic replaces a file's contents so that a crash or a full disk never leaves
// it half written: the data goes to a temporary file next to it, which is synced and
// renamed over the original. An existing file keeps its permissions, and a symlink is
// followed, so the link stays and its target changes. A file with other hard links is
// written in place instead, since renaming would detach it from them.
func writeFileAtomic(path string, data []byte, newMode os.FileMode) error {
	target := path
	mode := newMode
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if resolved, err := filepath.EvalSymlinks(path); err == nil {
				target = resolved
				if info, err = os.Stat(resolved); err != nil {
					return os.WriteFile(path, data, newMode)
				}
			}
		}
		mode = info.Mode().Perm()
		if hardLinkCount(info) > 1 {
			return os.WriteFile(target, data, mode)
		}
	}

	temp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".goat-*")
	if err != nil {
		// A directory goat cannot create files in may still allow writing the file.
		return os.WriteFile(target, data, mode)
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(temp.Name(), target)
}
