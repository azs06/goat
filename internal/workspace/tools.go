package workspace

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// bashToolTimeout is a variable so tests can shorten it.
var bashToolTimeout = 30 * time.Second

const (
	// bashPipeWaitDelay is how long run_bash keeps reading output after the shell exits,
	// in case a background process it started still holds the output pipe open.
	bashPipeWaitDelay   = 2 * time.Second
	defaultFindLimit    = 200
	defaultGrepLimit    = 100
	defaultTreeLimit    = 200
	maxSearchFileSize   = 1 << 20
	maxReadFileSize     = 1 << 20
	maxDiffPreviewLines = 200
	// maxDiffCells bounds the LCS table behind a diff preview (about 16 MB of int32s).
	maxDiffCells = 4_000_000

	// StateDirName holds goat's history, operation log, and undo snapshots.
	StateDirName = ".goat"
)

var errSearchLimitReached = errors.New("search limit reached")

// isMissing reports whether err means a path does not exist, including when one of its
// parents is now a file rather than a directory.
func isMissing(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR)
}

func escapesRoot(relPath string) bool {
	return relPath == ".." || strings.HasPrefix(relPath, ".."+string(os.PathSeparator))
}

// IsStateDirName also matches spellings that name the same directory on
// case-insensitive filesystems and on Windows, which ignores trailing dots and spaces.
func IsStateDirName(name string) bool {
	return strings.EqualFold(strings.TrimRight(name, ". "), StateDirName)
}

// isSecretFileName reports files that conventionally hold credentials, such as a
// project's .env. File tools refuse them, so their contents never
// reach the model without the user running a command for it. Templates meant to be
// shared (.env.example and the like) stay readable.
func isSecretFileName(name string) bool {
	lower := strings.ToLower(strings.TrimRight(name, ". "))
	if lower == ".envrc" || lower == ".env" {
		return true
	}
	if !strings.HasPrefix(lower, ".env.") {
		return false
	}
	switch strings.TrimPrefix(lower, ".env.") {
	case "example", "sample", "template", "dist", "defaults":
		return false
	}
	return true
}

// protectedDirs are directories file tools never open or search, such as goat's config
// directory with the user's keys, which a workspace such as the home directory contains.
var protectedDirs []string

// ProtectDir keeps file tools out of dir and everything in it, including through a
// symlink or another path to it.
func ProtectDir(dir string) {
	if dir == "" {
		return
	}
	if abs, err := filepath.Abs(dir); err == nil {
		protectedDirs = append(protectedDirs, abs)
	}
}

// ResetProtectedDirs forgets every protected directory, for tests.
func ResetProtectedDirs() { protectedDirs = nil }

// inProtectedDir reports a path inside a protected directory, by name or as the same
// file.
func inProtectedDir(path string) bool {
	for _, dir := range protectedDirs {
		if rel, err := filepath.Rel(dir, path); err == nil && !escapesRoot(rel) {
			return true
		}
		if isInsideExistingDir(path, dir) {
			return true
		}
	}
	return false
}

func errProtectedDir(relPath string) error {
	return fmt.Errorf("%s is in goat's config directory, which holds the user's keys, so file tools do not open it", filepath.ToSlash(relPath))
}

func isStatePath(relPath string) bool {
	first, _, _ := strings.Cut(filepath.ToSlash(relPath), "/")
	return IsStateDirName(first)
}

// isInsideExistingDir reports whether path is dir or lies inside it, comparing file
// identity so that symlinks, case variants and short names cannot hide the match.
func isInsideExistingDir(path, dir string) bool {
	dirInfo, err := os.Stat(dir)
	if err != nil || !dirInfo.IsDir() {
		return false
	}

	for current := path; ; current = filepath.Dir(current) {
		if info, err := os.Stat(current); err == nil && os.SameFile(info, dirInfo) {
			return true
		}
		if filepath.Dir(current) == current {
			return false
		}
	}
}

// sameExistingFile reports whether both paths exist and are the same file.
func sameExistingFile(firstResolvedPath, secondResolvedPath string) bool {
	firstInfo, err := os.Stat(firstResolvedPath)
	if err != nil {
		return false
	}
	secondInfo, err := os.Stat(secondResolvedPath)
	return err == nil && os.SameFile(firstInfo, secondInfo)
}

// ResolveStatePath resolves a path inside the workspace, including goat's own state
// directory. Tools that act for the model must use ResolvePath instead.
func ResolveStatePath(path string) (string, string, error) {
	if strings.TrimSpace(path) == "" {
		return "", "", fmt.Errorf("path is required")
	}
	cleanPath := filepath.Clean(strings.TrimSpace(path))
	if filepath.IsAbs(cleanPath) {
		return "", "", fmt.Errorf("path must be relative to the workspace")
	}

	workspaceRoot, err := os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("determine workspace root: %w", err)
	}

	resolvedPath := filepath.Join(workspaceRoot, cleanPath)
	relPath, err := filepath.Rel(workspaceRoot, resolvedPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve path: %w", err)
	}
	if escapesRoot(relPath) {
		return "", "", fmt.Errorf("path escapes the workspace")
	}

	return resolvedPath, relPath, nil
}

// ResolvePath resolves a path the model asked for. Besides staying inside the
// workspace lexically, the path must not reach outside it, or into goat's state
// directory, through a symlink.
func ResolvePath(path string) (string, string, error) {
	resolvedPath, relPath, err := ResolveStatePath(path)
	if err != nil {
		return "", "", err
	}
	if isStatePath(relPath) {
		return "", "", fmt.Errorf("%s is reserved for goat's internal state", StateDirName)
	}
	if isSecretFileName(filepath.Base(relPath)) {
		return "", "", errSecretFile(relPath)
	}

	workspaceRoot, err := os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("determine workspace root: %w", err)
	}
	realRelPath, err := realPathRelativeToRoot(workspaceRoot, resolvedPath)
	if err != nil {
		return "", "", err
	}
	if escapesRoot(realRelPath) {
		return "", "", fmt.Errorf("path escapes the workspace through a symlink")
	}
	if isStatePath(realRelPath) || isInsideExistingDir(resolvedPath, filepath.Join(workspaceRoot, StateDirName)) {
		return "", "", fmt.Errorf("%s is reserved for goat's internal state", StateDirName)
	}
	if isSecretFileName(filepath.Base(realRelPath)) {
		return "", "", errSecretFile(relPath)
	}
	if inProtectedDir(resolvedPath) {
		return "", "", errProtectedDir(relPath)
	}

	return resolvedPath, relPath, nil
}

func errSecretFile(relPath string) error {
	return fmt.Errorf("%s may hold credentials, so file tools do not open it; ask the user if its contents are needed", filepath.ToSlash(relPath))
}

// realPathRelativeToRoot resolves symlinks in the longest existing prefix of
// resolvedPath and returns where the path really points, relative to the real root.
func realPathRelativeToRoot(workspaceRoot, resolvedPath string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}

	existing := resolvedPath
	var missing []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !isMissing(err) {
			return "", err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		missing = append([]string{filepath.Base(existing)}, missing...)
		existing = parent
	}

	realExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks: %w", err)
	}

	return filepath.Rel(realRoot, filepath.Join(append([]string{realExisting}, missing...)...))
}

func workspaceRelativePath(resolvedPath string) (string, error) {
	workspaceRoot, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("determine workspace root: %w", err)
	}

	relPath, err := filepath.Rel(workspaceRoot, resolvedPath)
	if err != nil {
		return "", fmt.Errorf("resolve relative path: %w", err)
	}

	return filepath.ToSlash(relPath), nil
}

func shouldSkipDir(name string) bool {
	return name == ".git" || IsStateDirName(name)
}

func readFile(filePath string) (string, error) {
	resolvedPath, _, err := ResolvePath(filePath)
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ReadFileForModel reads a file for read_file, refusing files too large to be useful
// once the output is truncated for the model.
func ReadFileForModel(filePath string) (string, error) {
	resolvedPath, _, err := ResolvePath(filePath)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(resolvedPath)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory; use read_dir or tree_dir", filepath.Clean(filePath))
	}
	if info.Size() > maxReadFileSize {
		return "", fmt.Errorf("%s is %d bytes, over the %d byte read_file limit; use grep_files or run_bash (for example sed -n '1,200p') to read part of it", filepath.Clean(filePath), info.Size(), maxReadFileSize)
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func readFileIfExists(filePath string) (string, bool, error) {
	resolvedPath, _, err := ResolvePath(filePath)
	if err != nil {
		return "", false, err
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}

	return string(data), true, nil
}

func readFileBytes(filePath string) ([]byte, error) {
	resolvedPath, _, err := ResolvePath(filePath)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func readFileBytesIfExists(filePath string) ([]byte, bool, error) {
	resolvedPath, _, err := ResolvePath(filePath)
	if err != nil {
		return nil, false, err
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}

	return data, true, nil
}

// WriteFile creates or replaces a workspace file, creating its parent directories,
// and records the change for undo.
func WriteFile(filePath, content string) error {
	if strings.TrimSpace(filePath) == "" {
		return fmt.Errorf("missing path")
	}

	if resolvedPath, _, err := ResolvePath(filePath); err != nil {
		return err
	} else if info, err := os.Stat(resolvedPath); err == nil && info.IsDir() {
		return fmt.Errorf("%s is a directory", filepath.Clean(filePath))
	}

	return withRecordedMutation("write_file", []string{filePath}, func() error {
		return rawWriteFile(filePath, content)
	})
}

// PreviewWriteFile describes what WriteFile would do, as a diff, without changing
// anything.
func PreviewWriteFile(filePath, content string) (string, error) {
	existingContent, exists, err := readFileIfExists(filePath)
	if err != nil {
		return "", err
	}

	cleanPath := filepath.ToSlash(filepath.Clean(filePath))
	oldLabel := "/dev/null"
	if exists {
		oldLabel = cleanPath
	}

	return fmt.Sprintf("Preview for write_file on %s:\n%s", cleanPath, buildTextDiff(oldLabel, cleanPath, existingContent, content)), nil
}

func applyEdit(content, oldText, newText string, replaceAll bool) (string, int, error) {
	if oldText == "" {
		return "", 0, fmt.Errorf("old_text must not be empty")
	}

	matchCount := strings.Count(content, oldText)
	if matchCount == 0 {
		return "", 0, fmt.Errorf("old_text not found")
	}
	if matchCount > 1 && !replaceAll {
		return "", 0, fmt.Errorf("old_text matched %d times; set replace_all to true to replace every match", matchCount)
	}

	updatedContent := content
	replaced := 1
	if replaceAll {
		updatedContent = strings.ReplaceAll(content, oldText, newText)
		replaced = matchCount
	} else {
		updatedContent = strings.Replace(content, oldText, newText, 1)
	}

	return updatedContent, replaced, nil
}

// EditFile replaces oldText with newText in a workspace file and returns how many
// replacements it made. Unless replaceAll is set, oldText must match exactly once.
func EditFile(filePath, oldText, newText string, replaceAll bool) (int, error) {
	if strings.TrimSpace(filePath) == "" {
		return 0, fmt.Errorf("missing path")
	}

	content, err := readFile(filePath)
	if err != nil {
		return 0, err
	}

	updatedContent, replaced, err := applyEdit(content, oldText, newText, replaceAll)
	if err != nil {
		if strings.Contains(err.Error(), "old_text not found") {
			return 0, fmt.Errorf("old_text not found in %s", filepath.Clean(filePath))
		}
		if strings.Contains(err.Error(), "old_text matched") {
			return 0, fmt.Errorf("%s in %s", err.Error(), filepath.Clean(filePath))
		}
		return 0, err
	}

	if err := withRecordedMutation("edit_file", []string{filePath}, func() error {
		return rawWriteFile(filePath, updatedContent)
	}); err != nil {
		return 0, err
	}

	return replaced, nil
}

// PreviewEditFile shows the diff EditFile would make without changing anything.
func PreviewEditFile(filePath, oldText, newText string, replaceAll bool) (string, error) {
	if strings.TrimSpace(filePath) == "" {
		return "", fmt.Errorf("missing path")
	}

	content, err := readFile(filePath)
	if err != nil {
		return "", err
	}

	updatedContent, replaced, err := applyEdit(content, oldText, newText, replaceAll)
	if err != nil {
		if strings.Contains(err.Error(), "old_text not found") {
			return "", fmt.Errorf("old_text not found in %s", filepath.Clean(filePath))
		}
		if strings.Contains(err.Error(), "old_text matched") {
			return "", fmt.Errorf("%s in %s", err.Error(), filepath.Clean(filePath))
		}
		return "", err
	}

	cleanPath := filepath.ToSlash(filepath.Clean(filePath))
	preview := buildTextDiff(cleanPath, cleanPath, content, updatedContent)
	return fmt.Sprintf("Preview for edit_file on %s:\nReplacements: %d\n%s", cleanPath, replaced, preview), nil
}

func ensureRegularFile(filePath string) (os.FileInfo, error) {
	resolvedPath, _, err := ResolvePath(filePath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(resolvedPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Clean(filePath))
	}
	return info, nil
}

func destinationPathAvailable(filePath string, overwrite bool) error {
	resolvedPath, _, err := ResolvePath(filePath)
	if err != nil {
		return err
	}

	info, err := os.Stat(resolvedPath)
	if err == nil {
		if info.IsDir() {
			return fmt.Errorf("destination %s is a directory", filepath.Clean(filePath))
		}
		if !overwrite {
			return fmt.Errorf("destination %s already exists", filepath.Clean(filePath))
		}
		return nil
	}
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func rawCopyFile(sourcePath, destinationPath string) error {
	info, err := ensureRegularFile(sourcePath)
	if err != nil {
		return err
	}

	data, err := readFileBytes(sourcePath)
	if err != nil {
		return err
	}

	resolvedDestination, _, err := ResolvePath(destinationPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolvedDestination), 0755); err != nil {
		return err
	}

	return os.WriteFile(resolvedDestination, data, info.Mode().Perm())
}

// distinctTransferPaths rejects a source and destination that are the same directory
// entry spelled differently, such as a.txt and A.txt on a case-insensitive filesystem.
// Overwriting the destination would otherwise delete the source. Distinct hard links and
// symlinks are separate entries and are allowed.
func distinctTransferPaths(sourcePath, destinationPath string) error {
	if filepath.Clean(sourcePath) == filepath.Clean(destinationPath) {
		return fmt.Errorf("source and destination must be different")
	}

	resolvedSource, _, err := ResolvePath(sourcePath)
	if err != nil {
		return err
	}
	resolvedDestination, _, err := ResolvePath(destinationPath)
	if err != nil {
		return err
	}
	if sameDirectoryEntry(resolvedSource, resolvedDestination) {
		return fmt.Errorf("source %s and destination %s are the same file", filepath.Clean(sourcePath), filepath.Clean(destinationPath))
	}
	return nil
}

func sameDirectoryEntry(firstResolvedPath, secondResolvedPath string) bool {
	if !sameExistingFile(firstResolvedPath, secondResolvedPath) {
		return false
	}
	for _, candidate := range []string{firstResolvedPath, secondResolvedPath} {
		if info, err := os.Lstat(candidate); err != nil || info.Mode()&fs.ModeSymlink != 0 {
			return false
		}
	}
	normalize := func(name string) string { return strings.TrimRight(name, ". ") }
	return sameExistingFile(filepath.Dir(firstResolvedPath), filepath.Dir(secondResolvedPath)) &&
		strings.EqualFold(normalize(filepath.Base(firstResolvedPath)), normalize(filepath.Base(secondResolvedPath)))
}

// CopyFile copies a workspace file, refusing to replace an existing destination unless
// overwrite is set, and records the change for undo.
func CopyFile(sourcePath, destinationPath string, overwrite bool) error {
	if err := distinctTransferPaths(sourcePath, destinationPath); err != nil {
		return err
	}

	if _, err := ensureRegularFile(sourcePath); err != nil {
		return err
	}
	if err := destinationPathAvailable(destinationPath, overwrite); err != nil {
		return err
	}

	return withRecordedMutation("copy_file", []string{destinationPath}, func() error {
		return rawCopyFile(sourcePath, destinationPath)
	})
}

// PreviewCopyFile describes what CopyFile would do without changing anything.
func PreviewCopyFile(sourcePath, destinationPath string, overwrite bool) (string, error) {
	if err := distinctTransferPaths(sourcePath, destinationPath); err != nil {
		return "", err
	}

	if _, err := ensureRegularFile(sourcePath); err != nil {
		return "", err
	}
	if err := destinationPathAvailable(destinationPath, overwrite); err != nil {
		return "", err
	}

	sourceData, err := readFileBytes(sourcePath)
	if err != nil {
		return "", err
	}
	destinationData, exists, err := readFileBytesIfExists(destinationPath)
	if err != nil {
		return "", err
	}

	destinationLabel := filepath.ToSlash(filepath.Clean(destinationPath))
	oldLabel := "/dev/null"
	if exists {
		oldLabel = destinationLabel
	}

	preview := previewDataChange(oldLabel, destinationLabel, destinationData, sourceData)
	return fmt.Sprintf("Preview for copy_file from %s to %s:\n%s", filepath.ToSlash(filepath.Clean(sourcePath)), destinationLabel, preview), nil
}

// MoveFile moves or renames a workspace file, refusing to replace an existing
// destination unless overwrite is set, and records the change for undo.
func MoveFile(sourcePath, destinationPath string, overwrite bool) error {
	if err := distinctTransferPaths(sourcePath, destinationPath); err != nil {
		return err
	}

	if _, err := ensureRegularFile(sourcePath); err != nil {
		return err
	}
	if err := destinationPathAvailable(destinationPath, overwrite); err != nil {
		return err
	}

	resolvedSource, _, err := ResolvePath(sourcePath)
	if err != nil {
		return err
	}
	resolvedDestination, _, err := ResolvePath(destinationPath)
	if err != nil {
		return err
	}

	return withRecordedMutation("move_file", []string{sourcePath, destinationPath}, func() error {
		if err := os.MkdirAll(filepath.Dir(resolvedDestination), 0755); err != nil {
			return err
		}
		if overwrite {
			if err := os.Remove(resolvedDestination); err != nil && !os.IsNotExist(err) {
				return err
			}
		}

		if err := os.Rename(resolvedSource, resolvedDestination); err == nil {
			return nil
		}

		if err := rawCopyFile(sourcePath, destinationPath); err != nil {
			return err
		}
		return os.Remove(resolvedSource)
	})
}

// PreviewMoveFile describes what MoveFile would do without changing anything.
func PreviewMoveFile(sourcePath, destinationPath string, overwrite bool) (string, error) {
	if err := distinctTransferPaths(sourcePath, destinationPath); err != nil {
		return "", err
	}

	if _, err := ensureRegularFile(sourcePath); err != nil {
		return "", err
	}
	if err := destinationPathAvailable(destinationPath, overwrite); err != nil {
		return "", err
	}

	sourceData, err := readFileBytes(sourcePath)
	if err != nil {
		return "", err
	}
	destinationData, exists, err := readFileBytesIfExists(destinationPath)
	if err != nil {
		return "", err
	}

	destinationLabel := filepath.ToSlash(filepath.Clean(destinationPath))
	oldLabel := "/dev/null"
	if exists {
		oldLabel = destinationLabel
	}

	preview := previewDataChange(oldLabel, destinationLabel, destinationData, sourceData)
	return fmt.Sprintf("Preview for move_file from %s to %s:\n%s\nSource file %s would be removed after the move.", filepath.ToSlash(filepath.Clean(sourcePath)), destinationLabel, preview, filepath.ToSlash(filepath.Clean(sourcePath))), nil
}

// DeleteFile removes a workspace file and records the change for undo.
func DeleteFile(filePath string) error {
	if _, err := ensureRegularFile(filePath); err != nil {
		return err
	}

	resolvedPath, _, err := ResolvePath(filePath)
	if err != nil {
		return err
	}

	return withRecordedMutation("delete_file", []string{filePath}, func() error {
		return os.Remove(resolvedPath)
	})
}

// PreviewDeleteFile describes what DeleteFile would remove without changing anything.
func PreviewDeleteFile(filePath string) (string, error) {
	if _, err := ensureRegularFile(filePath); err != nil {
		return "", err
	}

	data, err := readFileBytes(filePath)
	if err != nil {
		return "", err
	}

	cleanPath := filepath.ToSlash(filepath.Clean(filePath))
	preview := previewDataChange(cleanPath, "/dev/null", data, nil)
	return fmt.Sprintf("Preview for delete_file on %s:\n%s", cleanPath, preview), nil
}

// skipUnreadable lets a recursive search continue past entries it cannot read, such as
// directories without permission. Only a failure on the search root is an error.
func skipUnreadable(path, root string, d fs.DirEntry, walkErr error) error {
	if path == root {
		return walkErr
	}
	if d != nil && d.IsDir() {
		return filepath.SkipDir
	}
	return nil
}

// ReadDir lists a workspace directory's entries, directories marked with a trailing
// slash.
func ReadDir(dirPath string) ([]string, error) {
	resolvedPath, _, err := ResolvePath(dirPath)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(resolvedPath)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func hasGlobMeta(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

func matchesPattern(pattern, fileName, relPath string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return true
	}

	candidates := []string{fileName, relPath, filepath.ToSlash(relPath)}
	for _, candidate := range candidates {
		matched, err := filepath.Match(pattern, candidate)
		if err == nil && matched {
			return true
		}
	}

	if !hasGlobMeta(pattern) {
		normalizedPattern := strings.ToLower(pattern)
		for _, candidate := range candidates {
			if strings.Contains(strings.ToLower(candidate), normalizedPattern) {
				return true
			}
		}
	}

	return false
}

// FindFiles finds files by name; unless includeIgnored is set, files .gitignore
// excludes are skipped.
func FindFiles(rootPath, pattern string, limit int, includeIgnored bool) ([]string, bool, error) {
	if limit <= 0 {
		limit = defaultFindLimit
	}

	resolvedRoot, _, err := ResolvePath(rootPath)
	if err != nil {
		return nil, false, err
	}

	var ignore *ignoreFilter
	if !includeIgnored {
		ignore = loadIgnoreFilter()
	}
	var matches []string
	err = filepath.WalkDir(resolvedRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return skipUnreadable(path, resolvedRoot, d, walkErr)
		}
		relPath, err := workspaceRelativePath(path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) || (path != resolvedRoot && ignore.ignored(relPath, true)) || inProtectedDir(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if ignore.ignored(relPath, false) {
			return nil
		}
		if !matchesPattern(pattern, d.Name(), relPath) {
			return nil
		}

		matches = append(matches, relPath)
		if len(matches) >= limit {
			return errSearchLimitReached
		}
		return nil
	})

	truncated := false
	if errors.Is(err, errSearchLimitReached) {
		truncated = true
		err = nil
	}
	if err != nil {
		return nil, false, err
	}

	sort.Strings(matches)
	return matches, truncated, nil
}

// GrepOptions configures GrepFiles.
type GrepOptions struct {
	Query          string
	CaseSensitive  bool
	Regex          bool   // Query is a Go regular expression rather than plain text.
	Glob           string // Limits the files searched, such as *.go or cmd/*.go.
	IncludeIgnored bool   // Also search files .gitignore excludes.
	Limit          int    // Maximum matches; zero means the default.
}

// GrepFiles searches file contents line by line. Unless IncludeIgnored is set,
// files .gitignore excludes are skipped.
func GrepFiles(rootPath string, opts GrepOptions) ([]string, bool, error) {
	query := strings.TrimSpace(opts.Query)
	if query == "" {
		return nil, false, fmt.Errorf("missing query")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultGrepLimit
	}

	resolvedRoot, _, err := ResolvePath(rootPath)
	if err != nil {
		return nil, false, err
	}
	matchLine, err := lineMatcher(query, opts.Regex, opts.CaseSensitive)
	if err != nil {
		return nil, false, err
	}
	var ignore *ignoreFilter
	if !opts.IncludeIgnored {
		ignore = loadIgnoreFilter()
	}

	var matches []string
	err = filepath.WalkDir(resolvedRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return skipUnreadable(path, resolvedRoot, d, walkErr)
		}
		relPath, err := workspaceRelativePath(path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) || (path != resolvedRoot && ignore.ignored(relPath, true)) || inProtectedDir(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if ignore.ignored(relPath, false) || !matchesGlob(opts.Glob, relPath) {
			return nil
		}

		// Symlinks may point outside the workspace, so their targets are not searched,
		// and files holding credentials are never shown.
		if d.Type()&fs.ModeSymlink != 0 || isSecretFileName(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() > maxSearchFileSize {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil
		}

		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 1024), maxSearchFileSize)
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			line := scanner.Text()
			if !matchLine(line) {
				continue
			}

			matches = append(matches, fmt.Sprintf("%s:%d:%s", relPath, lineNumber, line))
			if len(matches) >= limit {
				return errSearchLimitReached
			}
		}
		return nil
	})

	truncated := false
	if errors.Is(err, errSearchLimitReached) {
		truncated = true
		err = nil
	}
	if err != nil {
		return nil, false, err
	}

	return matches, truncated, nil
}

// TreeDir lists a directory tree up to limit entries and reports whether it stopped
// at the limit.
func TreeDir(rootPath string, limit int) ([]string, bool, error) {
	if limit <= 0 {
		limit = defaultTreeLimit
	}

	resolvedRoot, relRoot, err := ResolvePath(rootPath)
	if err != nil {
		return nil, false, err
	}

	info, err := os.Stat(resolvedRoot)
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() {
		return nil, false, fmt.Errorf("%s is not a directory", filepath.Clean(rootPath))
	}

	rootLabel := filepath.ToSlash(relRoot)
	if rootLabel == "." {
		rootLabel = "."
	}
	lines := []string{rootLabel + "/"}
	truncated := false

	ignore := loadIgnoreFilter()
	var walk func(string, string) error
	walk = func(dirPath, prefix string) error {
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			return err
		}

		filtered := make([]os.DirEntry, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() && shouldSkipDir(entry.Name()) {
				continue
			}
			if rel, err := workspaceRelativePath(filepath.Join(dirPath, entry.Name())); err == nil && ignore.ignored(rel, entry.IsDir()) {
				continue
			}
			filtered = append(filtered, entry)
		}
		sort.Slice(filtered, func(i, j int) bool {
			return filtered[i].Name() < filtered[j].Name()
		})

		for i, entry := range filtered {
			connector := "├── "
			nextPrefix := prefix + "│   "
			if i == len(filtered)-1 {
				connector = "└── "
				nextPrefix = prefix + "    "
			}

			name := entry.Name()
			if entry.IsDir() {
				name += "/"
			}
			lines = append(lines, prefix+connector+name)
			if len(lines) >= limit {
				truncated = true
				return errSearchLimitReached
			}
			if entry.IsDir() {
				line := len(lines) - 1
				if err := walk(filepath.Join(dirPath, entry.Name()), nextPrefix); errors.Is(err, errSearchLimitReached) {
					return err
				} else if err != nil {
					// Keep listing the rest of the tree past directories that cannot be read.
					lines[line] += " [unreadable]"
				}
			}
		}

		return nil
	}

	err = walk(resolvedRoot, "")
	if errors.Is(err, errSearchLimitReached) {
		err = nil
	}
	if err != nil {
		return nil, false, err
	}

	return lines, truncated, nil
}

// DiffLines returns a line diff of two texts. Each line starts with ' ' (unchanged),
// '-' (removed) or '+' (added) and has no trailing newline.
func DiffLines(oldText, newText string) []string {
	ops := computeDiffOps(splitDiffLines(oldText), splitDiffLines(newText))
	lines := make([]string, 0, len(ops))
	for _, op := range ops {
		lines = append(lines, string(op.kind)+strings.TrimSuffix(op.text, "\n"))
	}
	return lines
}

type diffOp struct {
	kind byte
	text string
}

func splitDiffLines(content string) []string {
	if content == "" {
		return nil
	}
	return strings.SplitAfter(content, "\n")
}

func computeDiffOps(oldLines, newLines []string) []diffOp {
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix && oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}

	ops := make([]diffOp, 0, len(oldLines)+len(newLines))
	for _, line := range oldLines[:prefix] {
		ops = append(ops, diffOp{kind: ' ', text: line})
	}
	ops = append(ops, computeLCSDiffOps(oldLines[prefix:len(oldLines)-suffix], newLines[prefix:len(newLines)-suffix])...)
	for _, line := range oldLines[len(oldLines)-suffix:] {
		ops = append(ops, diffOp{kind: ' ', text: line})
	}
	return ops
}

func computeLCSDiffOps(oldLines, newLines []string) []diffOp {
	var ops []diffOp
	if (len(oldLines)+1)*(len(newLines)+1) > maxDiffCells {
		// Too large for an exact diff: show the whole changed region as replaced.
		for _, line := range oldLines {
			ops = append(ops, diffOp{kind: '-', text: line})
		}
		for _, line := range newLines {
			ops = append(ops, diffOp{kind: '+', text: line})
		}
		return ops
	}

	dp := make([][]int32, len(oldLines)+1)
	for i := range dp {
		dp[i] = make([]int32, len(newLines)+1)
	}

	for i := len(oldLines) - 1; i >= 0; i-- {
		for j := len(newLines) - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	i, j := 0, 0
	for i < len(oldLines) && j < len(newLines) {
		switch {
		case oldLines[i] == newLines[j]:
			ops = append(ops, diffOp{kind: ' ', text: oldLines[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{kind: '-', text: oldLines[i]})
			i++
		default:
			ops = append(ops, diffOp{kind: '+', text: newLines[j]})
			j++
		}
	}
	for i < len(oldLines) {
		ops = append(ops, diffOp{kind: '-', text: oldLines[i]})
		i++
	}
	for j < len(newLines) {
		ops = append(ops, diffOp{kind: '+', text: newLines[j]})
		j++
	}

	return ops
}

func buildTextDiff(oldLabel, newLabel, oldContent, newContent string) string {
	if oldContent == newContent {
		return "(no changes)"
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "--- %s\n", oldLabel)
	fmt.Fprintf(&builder, "+++ %s\n", newLabel)

	ops := computeDiffOps(splitDiffLines(oldContent), splitDiffLines(newContent))
	changedLines := 0
	for _, op := range ops {
		if op.kind == ' ' {
			continue
		}
		changedLines++
		if changedLines > maxDiffPreviewLines {
			builder.WriteString("... diff truncated ...\n")
			break
		}

		line := strings.TrimSuffix(op.text, "\n")
		fmt.Fprintf(&builder, "%c%s\n", op.kind, line)
	}

	if changedLines == 0 {
		return "(no changes)"
	}

	return strings.TrimRight(builder.String(), "\n")
}

func isTextData(data []byte) bool {
	return bytes.IndexByte(data, 0) == -1
}

func previewDataChange(oldLabel, newLabel string, oldData, newData []byte) string {
	oldText := isTextData(oldData)
	newText := isTextData(newData)
	if oldText && newText {
		return buildTextDiff(oldLabel, newLabel, string(oldData), string(newData))
	}
	return fmt.Sprintf("Binary-safe preview only: %s (%d bytes) -> %s (%d bytes)", oldLabel, len(oldData), newLabel, len(newData))
}

// RunBash runs a shell command in workdir, which must be inside the workspace, with
// credentials removed from its environment, and returns its combined output.
func RunBash(ctx context.Context, command, workdir string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", fmt.Errorf("missing command")
	}

	if strings.TrimSpace(workdir) == "" {
		workdir = "."
	}

	resolvedDir, relDir, err := ResolvePath(workdir)
	if err != nil {
		return "", err
	}

	runCtx, cancel := context.WithTimeout(ctx, bashToolTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "bash", "-lc", command)
	cmd.Dir = resolvedDir
	cmd.Env = CommandEnvironment(os.Environ())
	configureProcessGroup(cmd)
	cmd.WaitDelay = bashPipeWaitDelay

	output, err := cmd.CombinedOutput()
	trimmedOutput := strings.TrimRight(string(output), "\n")
	if trimmedOutput == "" {
		trimmedOutput = "(no output)"
	}

	result := fmt.Sprintf("Bash output for %q in %s:\n%s", command, relDir, trimmedOutput)

	// Check the exit result first: a command that exited cleanly was not killed, even if
	// the deadline passed while waiting for a background process to release the output.
	switch {
	case err == nil:
		return result, nil
	case errors.Is(err, exec.ErrWaitDelay):
		// The shell finished but something it started in the background still holds the
		// output open; that process keeps running and its later output is not captured.
		return result + "\n(a background process is still running; redirect its output to a file to keep it)", nil
	case ctx.Err() != nil:
		return "", fmt.Errorf("command interrupted by the user\n%s", result)
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return "", fmt.Errorf("command timed out after %s and was killed\n%s", bashToolTimeout, result)
	default:
		return "", fmt.Errorf("%w\n%s", err, result)
	}
}
