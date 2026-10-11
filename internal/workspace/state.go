package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// goat keeps each workspace's history, session, prompt history, operation log and undo
// snapshots outside the workspace, in a directory of its own under the user's state
// directory, as other coding agents do: $XDG_STATE_HOME/goat/projects/<workspace>, or
// ~/.local/state/goat/projects/<workspace>. Inside goat these files are still named by
// paths starting with StateDirName, such as ".goat/history.jsonl", which
// ResolveStatePath maps there; that keeps the paths recorded in older operation logs
// working.

// StateHome is the directory holding every workspace's state, or "" when there is no
// home directory.
func StateHome() string {
	// The XDG spec says a relative XDG_STATE_HOME is invalid and should be ignored.
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "goat")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "goat")
}

// StateRoot is the state directory of the workspace goat runs in, the current
// directory.
func StateRoot() (string, error) {
	home := StateHome()
	if home == "" {
		return "", errors.New("there is no home directory to keep goat's state in; set XDG_STATE_HOME")
	}
	workspaceRoot, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("determine workspace root: %w", err)
	}
	return filepath.Join(home, "projects", stateDirKey(workspaceRoot)), nil
}

// stateDirKey names a workspace's state directory: its path with every character but
// letters, digits, dots and underscores turned into "-", as other agents name theirs,
// then a short hash of the path, so "a-b" and "a/b" never share one.
func stateDirKey(workspaceRoot string) string {
	readable := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' {
			return r
		}
		return '-'
	}, workspaceRoot)
	// Keep the name well under the 255 bytes a file name may have, keeping the end of
	// the path, which says the most.
	if len(readable) > 120 {
		readable = readable[len(readable)-120:]
	}
	sum := sha256.Sum256([]byte(workspaceRoot))
	return readable + "-" + hex.EncodeToString(sum[:4])
}

// ResolveStatePath maps a path in goat's state, such as ".goat/history.jsonl", to the
// file in the workspace's state directory. It returns that file's path twice, the
// second time for showing to the user. Tools that act for the model use ResolvePath,
// which never reaches the state directory.
func ResolveStatePath(path string) (string, string, error) {
	cleanPath := filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	rest, ok := strings.CutPrefix(cleanPath, StateDirName+"/")
	if !ok || rest == "" || escapesRoot(rest) {
		return "", "", fmt.Errorf("%q is not in goat's state", path)
	}
	// The directory is made here, before anything can be written to it with looser
	// permissions.
	root, err := EnsureStateRoot()
	if err != nil {
		return "", "", err
	}
	resolved := filepath.Join(root, filepath.FromSlash(rest))
	return resolved, resolved, nil
}

// EnsureStateRoot creates the workspace's state directory, readable only by the user:
// the history holds the conversation, and command output in it can hold secrets.
func EnsureStateRoot() (string, error) {
	root, err := StateRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	return root, nil
}

// MigrateLegacyState moves the files of a .goat directory that older versions of goat
// kept in the workspace into the workspace's state directory, and removes .goat once it
// is empty. A file the state directory already has stays where it is, and so does
// .goat then. It returns the state directory and what was moved and left.
func MigrateLegacyState() (root string, moved, left []string, err error) {
	workspaceRoot, err := os.Getwd()
	if err != nil {
		return "", nil, nil, err
	}
	legacy := filepath.Join(workspaceRoot, StateDirName)
	info, err := os.Lstat(legacy)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil, nil, nil
	}
	if err != nil {
		return "", nil, nil, err
	}
	if !info.IsDir() {
		return "", nil, nil, fmt.Errorf("%s is not a directory; goat leaves it alone", StateDirName)
	}
	root, err = EnsureStateRoot()
	if err != nil {
		return "", nil, nil, err
	}
	entries, err := os.ReadDir(legacy)
	if err != nil {
		return root, nil, nil, err
	}
	for _, entry := range entries {
		from, to := filepath.Join(legacy, entry.Name()), filepath.Join(root, entry.Name())
		if _, err := os.Lstat(to); err == nil {
			left = append(left, entry.Name())
			continue
		}
		if err := moveStateEntry(from, to); err != nil {
			left = append(left, entry.Name())
			continue
		}
		moved = append(moved, entry.Name())
	}
	if len(left) == 0 {
		if err := os.Remove(legacy); err != nil {
			return root, moved, left, err
		}
	}
	return root, moved, left, nil
}

// moveStateEntry renames a file or directory, or copies and then removes it when the
// rename crosses file systems.
func moveStateEntry(from, to string) error {
	if err := os.Rename(from, to); err == nil {
		return nil
	}
	if err := copyStateEntry(from, to); err != nil {
		os.RemoveAll(to)
		return err
	}
	return os.RemoveAll(from)
}

// copyStateEntry copies regular files and directories, keeping their permissions.
// Anything else, such as a symlink, is refused rather than followed.
func copyStateEntry(from, to string) error {
	info, err := os.Lstat(from)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		if err := os.MkdirAll(to, info.Mode().Perm()|0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(from)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyStateEntry(filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	case info.Mode().IsRegular():
		source, err := os.Open(from)
		if err != nil {
			return err
		}
		defer source.Close()
		target, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(target, source); err != nil {
			target.Close()
			return err
		}
		return target.Close()
	default:
		return fmt.Errorf("%s is not a regular file or directory", from)
	}
}
