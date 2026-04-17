package main

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
	"time"
)

const (
	bashToolTimeout     = 30 * time.Second
	defaultFindLimit    = 200
	defaultGrepLimit    = 100
	defaultTreeLimit    = 200
	maxSearchFileSize   = 1 << 20
	maxDiffPreviewLines = 200
)

var errSearchLimitReached = errors.New("search limit reached")

func resolveToolPath(path string) (string, string, error) {
	cleanPath := filepath.Clean(strings.TrimSpace(path))
	if cleanPath == "" {
		return "", "", fmt.Errorf("path is required")
	}
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
	if relPath == ".." || strings.HasPrefix(relPath, ".."+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("path escapes the workspace")
	}

	return resolvedPath, relPath, nil
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
	return name == ".git" || name == ".goat"
}

func readFile(filePath string) (string, error) {
	resolvedPath, _, err := resolveToolPath(filePath)
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func readFileIfExists(filePath string) (string, bool, error) {
	resolvedPath, _, err := resolveToolPath(filePath)
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
	resolvedPath, _, err := resolveToolPath(filePath)
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
	resolvedPath, _, err := resolveToolPath(filePath)
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

func writeFile(filePath, content string) error {
	resolvedPath, _, err := resolveToolPath(filePath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(resolvedPath), 0755); err != nil {
		return err
	}

	return os.WriteFile(resolvedPath, []byte(content), 0644)
}

func previewWriteFile(filePath, content string) (string, error) {
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

func editFile(filePath, oldText, newText string, replaceAll bool) (int, error) {
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

	if err := writeFile(filePath, updatedContent); err != nil {
		return 0, err
	}

	return replaced, nil
}

func previewEditFile(filePath, oldText, newText string, replaceAll bool) (string, error) {
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
	resolvedPath, _, err := resolveToolPath(filePath)
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
	resolvedPath, _, err := resolveToolPath(filePath)
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

func copyFile(sourcePath, destinationPath string, overwrite bool) error {
	if filepath.Clean(sourcePath) == filepath.Clean(destinationPath) {
		return fmt.Errorf("source and destination must be different")
	}

	info, err := ensureRegularFile(sourcePath)
	if err != nil {
		return err
	}
	if err := destinationPathAvailable(destinationPath, overwrite); err != nil {
		return err
	}

	data, err := readFileBytes(sourcePath)
	if err != nil {
		return err
	}

	resolvedDestination, _, err := resolveToolPath(destinationPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolvedDestination), 0755); err != nil {
		return err
	}

	return os.WriteFile(resolvedDestination, data, info.Mode().Perm())
}

func previewCopyFile(sourcePath, destinationPath string, overwrite bool) (string, error) {
	if filepath.Clean(sourcePath) == filepath.Clean(destinationPath) {
		return "", fmt.Errorf("source and destination must be different")
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

func moveFile(sourcePath, destinationPath string, overwrite bool) error {
	if filepath.Clean(sourcePath) == filepath.Clean(destinationPath) {
		return fmt.Errorf("source and destination must be different")
	}

	if _, err := ensureRegularFile(sourcePath); err != nil {
		return err
	}
	if err := destinationPathAvailable(destinationPath, overwrite); err != nil {
		return err
	}

	resolvedSource, _, err := resolveToolPath(sourcePath)
	if err != nil {
		return err
	}
	resolvedDestination, _, err := resolveToolPath(destinationPath)
	if err != nil {
		return err
	}
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

	if err := copyFile(sourcePath, destinationPath, overwrite); err != nil {
		return err
	}
	return deleteFile(sourcePath)
}

func previewMoveFile(sourcePath, destinationPath string, overwrite bool) (string, error) {
	if filepath.Clean(sourcePath) == filepath.Clean(destinationPath) {
		return "", fmt.Errorf("source and destination must be different")
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

func deleteFile(filePath string) error {
	if _, err := ensureRegularFile(filePath); err != nil {
		return err
	}

	resolvedPath, _, err := resolveToolPath(filePath)
	if err != nil {
		return err
	}
	return os.Remove(resolvedPath)
}

func previewDeleteFile(filePath string) (string, error) {
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

func readDir(dirPath string) ([]string, error) {
	resolvedPath, _, err := resolveToolPath(dirPath)
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

func findFiles(rootPath, pattern string, limit int) ([]string, bool, error) {
	if limit <= 0 {
		limit = defaultFindLimit
	}

	resolvedRoot, _, err := resolveToolPath(rootPath)
	if err != nil {
		return nil, false, err
	}

	var matches []string
	err = filepath.WalkDir(resolvedRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		relPath, err := workspaceRelativePath(path)
		if err != nil {
			return err
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

func grepFiles(rootPath, query string, caseSensitive bool, limit int) ([]string, bool, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, false, fmt.Errorf("missing query")
	}
	if limit <= 0 {
		limit = defaultGrepLimit
	}

	resolvedRoot, _, err := resolveToolPath(rootPath)
	if err != nil {
		return nil, false, err
	}

	normalizedQuery := query
	if !caseSensitive {
		normalizedQuery = strings.ToLower(query)
	}

	var matches []string
	err = filepath.WalkDir(resolvedRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxSearchFileSize {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil
		}

		relPath, err := workspaceRelativePath(path)
		if err != nil {
			return err
		}

		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 1024), maxSearchFileSize)
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			line := scanner.Text()
			candidate := line
			if !caseSensitive {
				candidate = strings.ToLower(line)
			}
			if !strings.Contains(candidate, normalizedQuery) {
				continue
			}

			matches = append(matches, fmt.Sprintf("%s:%d:%s", relPath, lineNumber, line))
			if len(matches) >= limit {
				return errSearchLimitReached
			}
		}
		if err := scanner.Err(); err != nil {
			return err
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

func treeDir(rootPath string, limit int) ([]string, bool, error) {
	if limit <= 0 {
		limit = defaultTreeLimit
	}

	resolvedRoot, relRoot, err := resolveToolPath(rootPath)
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
				if err := walk(filepath.Join(dirPath, entry.Name()), nextPrefix); err != nil {
					return err
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
	dp := make([][]int, len(oldLines)+1)
	for i := range dp {
		dp[i] = make([]int, len(newLines)+1)
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

	var ops []diffOp
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

func runBashCommand(ctx context.Context, command, workdir string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", fmt.Errorf("missing command")
	}

	if strings.TrimSpace(workdir) == "" {
		workdir = "."
	}

	resolvedDir, relDir, err := resolveToolPath(workdir)
	if err != nil {
		return "", err
	}

	runCtx, cancel := context.WithTimeout(ctx, bashToolTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "bash", "-lc", command)
	cmd.Dir = resolvedDir

	output, err := cmd.CombinedOutput()
	trimmedOutput := strings.TrimRight(string(output), "\n")
	if trimmedOutput == "" {
		trimmedOutput = "(no output)"
	}

	result := fmt.Sprintf("Bash output for %q in %s:\n%s", command, relDir, trimmedOutput)

	if runCtx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("command timed out after %s\n%s", bashToolTimeout, result)
	}
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, result)
	}

	return result, nil
}
