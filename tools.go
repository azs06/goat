package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const bashToolTimeout = 30 * time.Second

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

func writeFile(filePath, content string) error {
	resolvedPath, _, err := resolveToolPath(filePath)
	if err != nil {
		return err
	}

	return os.WriteFile(resolvedPath, []byte(content), 0644)
}

func editFile(filePath, oldText, newText string, replaceAll bool) (int, error) {
	if strings.TrimSpace(filePath) == "" {
		return 0, fmt.Errorf("missing path")
	}
	if oldText == "" {
		return 0, fmt.Errorf("old_text must not be empty")
	}

	content, err := readFile(filePath)
	if err != nil {
		return 0, err
	}

	matchCount := strings.Count(content, oldText)
	if matchCount == 0 {
		return 0, fmt.Errorf("old_text not found in %s", filepath.Clean(filePath))
	}
	if matchCount > 1 && !replaceAll {
		return 0, fmt.Errorf("old_text matched %d times in %s; set replace_all to true to replace every match", matchCount, filepath.Clean(filePath))
	}

	updatedContent := content
	replaced := 1
	if replaceAll {
		updatedContent = strings.ReplaceAll(content, oldText, newText)
		replaced = matchCount
	} else {
		updatedContent = strings.Replace(content, oldText, newText, 1)
	}

	if err := writeFile(filePath, updatedContent); err != nil {
		return 0, err
	}

	return replaced, nil
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
	return names, nil
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
