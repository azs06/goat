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
	"runtime"
	"strings"
)

// goat keeps each workspace's history, session, prompt history, operation log and undo
// snapshots outside the workspace, in a directory of its own under the user's state
// directory, as other coding agents do: $XDG_STATE_HOME/goat/projects/<workspace>, or
// ~/.local/state/goat/projects/<workspace>. Inside goat these files are still named by
// paths starting with StateDirName, such as ".goat/history.jsonl", which
// ResolveStatePath maps there; that keeps the paths recorded in older operation logs
// working.

// StateHome is the directory holding every workspace's state. Without a home
// directory, as in some containers, it is in the temporary directory, per user.
func StateHome() string {
	// The XDG spec says a relative XDG_STATE_HOME is invalid and should be ignored.
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "goat")
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return filepath.Join(os.TempDir(), fmt.Sprintf("goat-state-%d", os.Getuid()))
	}
	return filepath.Join(home, ".local", "state", "goat")
}

// StateRoot is the state directory of the workspace goat runs in, the current
// directory.
func StateRoot() (string, error) {
	workspaceRoot, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("determine workspace root: %w", err)
	}
	return filepath.Join(StateHome(), "projects", stateDirKey(canonicalWorkspace(workspaceRoot))), nil
}

// canonicalWorkspace is the one spelling of a workspace's path its state directory is
// named after, however goat got there: through symlinks (/tmp is /private/tmp on
// macOS), and, where file names ignore case by default (macOS, Windows), in any case.
func canonicalWorkspace(root string) string {
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		root = strings.ToLower(root)
	}
	return root
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

// Migration is what MigrateLegacyState did.
type Migration struct {
	// Root is the workspace's state directory.
	Root string
	// Moved are the entries of .goat moved there.
	Moved []string
	// Clashed are the entries left in .goat because the state directory has files of
	// the same names.
	Clashed []string
	// Failed are the entries that could not be moved, and why.
	Failed map[string]error
}

// MigrateLegacyState moves the files of a .goat directory that older versions of goat
// kept in the workspace into the workspace's state directory, and removes .goat once it
// is empty. It never replaces or removes a file it did not make, so two goats starting
// in the same workspace at once lose nothing.
func MigrateLegacyState() (Migration, error) {
	var result Migration
	workspaceRoot, err := os.Getwd()
	if err != nil {
		return result, err
	}
	legacy := filepath.Join(workspaceRoot, StateDirName)
	info, err := os.Lstat(legacy)
	if errors.Is(err, fs.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if !info.IsDir() {
		return result, fmt.Errorf("%s is not a directory; goat leaves it alone", StateDirName)
	}
	if result.Root, err = EnsureStateRoot(); err != nil {
		return result, err
	}
	entries, err := os.ReadDir(legacy)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		name := entry.Name()
		switch moved, err := moveStateEntry(filepath.Join(legacy, name), filepath.Join(result.Root, name)); {
		case errors.Is(err, errStateClash):
			result.Clashed = append(result.Clashed, name)
		case err != nil:
			if result.Failed == nil {
				result.Failed = map[string]error{}
			}
			result.Failed[name] = err
		case moved:
			result.Moved = append(result.Moved, name)
		}
	}
	// Removed only once empty; another goat may be moving the rest.
	os.Remove(legacy)
	return result, nil
}

var errStateClash = errors.New("the state directory already has a file of that name")

// moveStateEntry moves a file or directory to to, which must not exist. A rename that
// crosses file systems becomes a copy to a temporary name next to to, renamed into
// place, then removing from. moved is false when from had gone, moved by another goat.
func moveStateEntry(from, to string) (moved bool, err error) {
	if _, err := os.Lstat(to); err == nil {
		return false, errStateClash
	}
	renameErr := os.Rename(from, to)
	if renameErr == nil {
		return true, nil
	}
	if _, err := os.Lstat(from); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	temp := filepath.Join(filepath.Dir(to), fmt.Sprintf(".migrating-%s-%d", filepath.Base(to), os.Getpid()))
	if err := copyStateEntry(from, temp); err != nil {
		removeCopy(temp)
		return false, fmt.Errorf("%v; copying it failed too: %w", renameErr, err)
	}
	if _, err := os.Lstat(to); err == nil {
		removeCopy(temp)
		return false, errStateClash
	}
	if err := os.Rename(temp, to); err != nil {
		removeCopy(temp)
		return false, err
	}
	if err := os.RemoveAll(from); err != nil {
		return true, fmt.Errorf("copied, but the original could not be removed: %w", err)
	}
	return true, nil
}

// removeCopy removes a copy this goat made, making its directories writable first, since
// the copy keeps read-only ones read-only.
func removeCopy(path string) {
	filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(p, 0o700)
		}
		return nil
	})
	os.RemoveAll(path)
}

// copyStateEntry copies regular files and directories with their exact permissions, as
// undo restores them from snapshots. Anything else, such as a symlink, is refused
// rather than followed. A directory gets its permissions once its contents are in.
func copyStateEntry(from, to string) error {
	info, err := os.Lstat(from)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		if err := os.Mkdir(to, 0o700); err != nil {
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
		return os.Chmod(to, info.Mode().Perm())
	case info.Mode().IsRegular():
		source, err := os.Open(from)
		if err != nil {
			return err
		}
		defer source.Close()
		target, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(target, source); err != nil {
			target.Close()
			return err
		}
		if err := target.Close(); err != nil {
			return err
		}
		return os.Chmod(to, info.Mode().Perm())
	default:
		return fmt.Errorf("%s is not a regular file or directory", from)
	}
}
