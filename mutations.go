package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultOperationsPath = ".goat/operations.json"
	defaultSnapshotsRoot  = ".goat/snapshots"
	maxStateDiffEntries   = 40
)

type operationRecord struct {
	ID           string   `json:"id"`
	Timestamp    string   `json:"timestamp"`
	Kind         string   `json:"kind"`
	Paths        []string `json:"paths"`
	SnapshotPath string   `json:"snapshot_path"`
	UndoneAt     string   `json:"undone_at,omitempty"`
}

type capturedPathState struct {
	IsDir bool
	Mode  fs.FileMode
	Data  []byte
}

func rawWriteFile(filePath, content string) error {
	resolvedPath, _, err := resolveToolPath(filePath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(resolvedPath), 0755); err != nil {
		return err
	}

	return os.WriteFile(resolvedPath, []byte(content), 0644)
}

func removeWorkspacePathIfExists(filePath string) error {
	resolvedPath, _, err := resolveToolPath(filePath)
	if err != nil {
		return err
	}

	if _, err := os.Stat(resolvedPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	return os.RemoveAll(resolvedPath)
}

func normalizeOperationPaths(paths []string) ([]string, error) {
	seen := map[string]struct{}{}
	var normalized []string

	for _, candidate := range paths {
		if strings.TrimSpace(candidate) == "" {
			return nil, fmt.Errorf("path is required")
		}

		_, relPath, err := resolveToolPath(candidate)
		if err != nil {
			return nil, err
		}

		relPath = filepath.ToSlash(relPath)
		if _, ok := seen[relPath]; ok {
			continue
		}
		seen[relPath] = struct{}{}
		normalized = append(normalized, relPath)
	}

	sort.Strings(normalized)
	return normalized, nil
}

func operationSnapshotPath(id string) string {
	return filepath.ToSlash(filepath.Join(defaultSnapshotsRoot, id, "before"))
}

func newOperationRecord(kind string, paths []string) (operationRecord, error) {
	normalizedPaths, err := normalizeOperationPaths(paths)
	if err != nil {
		return operationRecord{}, err
	}

	id := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	return operationRecord{
		ID:           id,
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
		Kind:         kind,
		Paths:        normalizedPaths,
		SnapshotPath: operationSnapshotPath(id),
	}, nil
}

func createOperationSnapshot(record operationRecord) error {
	resolvedSnapshotRoot, _, err := resolveToolPath(record.SnapshotPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(resolvedSnapshotRoot, 0755); err != nil {
		return err
	}

	for _, filePath := range record.Paths {
		if err := snapshotWorkspacePath(filePath, resolvedSnapshotRoot); err != nil {
			return err
		}
	}

	return nil
}

func cleanupOperationSnapshot(record operationRecord) {
	resolvedSnapshotRoot, _, err := resolveToolPath(record.SnapshotPath)
	if err != nil {
		return
	}
	_ = os.RemoveAll(filepath.Dir(resolvedSnapshotRoot))
}

func snapshotWorkspacePath(filePath, resolvedSnapshotRoot string) error {
	resolvedPath, relPath, err := resolveToolPath(filePath)
	if err != nil {
		return err
	}

	info, err := os.Lstat(resolvedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	destinationPath := filepath.Join(resolvedSnapshotRoot, filepath.FromSlash(relPath))
	return copyResolvedPath(resolvedPath, destinationPath, info)
}

func copyResolvedPath(sourceResolvedPath, destinationResolvedPath string, info fs.FileInfo) error {
	if info.IsDir() {
		return copyResolvedDirectory(sourceResolvedPath, destinationResolvedPath)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported file type at %s", filepath.Clean(sourceResolvedPath))
	}
	return copyResolvedRegularFile(sourceResolvedPath, destinationResolvedPath, info.Mode())
}

func copyResolvedRegularFile(sourceResolvedPath, destinationResolvedPath string, mode fs.FileMode) error {
	data, err := os.ReadFile(sourceResolvedPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destinationResolvedPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(destinationResolvedPath, data, mode.Perm())
}

func copyResolvedDirectory(sourceResolvedPath, destinationResolvedPath string) error {
	return filepath.Walk(sourceResolvedPath, func(currentPath string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file type at %s", filepath.Clean(currentPath))
		}

		relPath, err := filepath.Rel(sourceResolvedPath, currentPath)
		if err != nil {
			return err
		}
		destinationPath := destinationResolvedPath
		if relPath != "." {
			destinationPath = filepath.Join(destinationResolvedPath, relPath)
		}

		if info.IsDir() {
			return os.MkdirAll(destinationPath, info.Mode().Perm())
		}

		return copyResolvedRegularFile(currentPath, destinationPath, info.Mode())
	})
}

func withRecordedMutation(kind string, paths []string, mutate func() error) error {
	record, err := newOperationRecord(kind, paths)
	if err != nil {
		return err
	}
	if err := createOperationSnapshot(record); err != nil {
		return err
	}

	if err := mutate(); err != nil {
		cleanupOperationSnapshot(record)
		return err
	}

	if err := appendOperationRecord(record); err != nil {
		return err
	}

	return nil
}

func loadOperationRecords() ([]operationRecord, error) {
	resolvedPath, _, err := resolveToolPath(defaultOperationsPath)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}

	var records []operationRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("decode operations: %w", err)
	}

	return records, nil
}

func saveOperationRecords(records []operationRecord) error {
	resolvedPath, _, err := resolveToolPath(defaultOperationsPath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(resolvedPath), 0755); err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("encode operations: %w", err)
	}
	encoded = append(encoded, '\n')

	return os.WriteFile(resolvedPath, encoded, 0644)
}

func appendOperationRecord(record operationRecord) error {
	records, err := loadOperationRecords()
	if err != nil {
		return err
	}

	records = append(records, record)
	return saveOperationRecords(records)
}

func latestUndoableOperationRecord() (operationRecord, int, []operationRecord, error) {
	records, err := loadOperationRecords()
	if err != nil {
		return operationRecord{}, -1, nil, err
	}

	for index := len(records) - 1; index >= 0; index-- {
		if strings.TrimSpace(records[index].UndoneAt) != "" {
			continue
		}
		return records[index], index, records, nil
	}

	return operationRecord{}, -1, records, nil
}

func previewUndoLastChange() (string, error) {
	record, index, _, err := latestUndoableOperationRecord()
	if err != nil {
		return "", err
	}
	if index < 0 {
		return "No recorded operations available to undo.", nil
	}

	currentState, err := captureWorkspaceState(record.Paths)
	if err != nil {
		return "", err
	}
	previousState, err := captureSnapshotState(record)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Preview for undo_last_change on %s (%s):\n%s", record.Kind, strings.Join(record.Paths, ", "), renderStateDiff(currentState, previousState)), nil
}

func undoLastChange() (string, error) {
	record, index, records, err := latestUndoableOperationRecord()
	if err != nil {
		return "", err
	}
	if index < 0 {
		return "No recorded operations available to undo.", nil
	}

	if err := restoreOperationSnapshot(record); err != nil {
		return "", err
	}

	records[index].UndoneAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveOperationRecords(records); err != nil {
		return "", err
	}

	return fmt.Sprintf("Undid %s for %s.", record.Kind, strings.Join(record.Paths, ", ")), nil
}

func restoreOperationSnapshot(record operationRecord) error {
	paths := append([]string(nil), record.Paths...)
	sort.Slice(paths, func(i, j int) bool {
		return len(paths[i]) > len(paths[j])
	})
	for _, filePath := range paths {
		if err := removeWorkspacePathIfExists(filePath); err != nil {
			return err
		}
	}

	resolvedSnapshotRoot, _, err := resolveToolPath(record.SnapshotPath)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(resolvedSnapshotRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	workspaceRoot, err := os.Getwd()
	if err != nil {
		return err
	}

	for _, entry := range entries {
		sourcePath := filepath.Join(resolvedSnapshotRoot, entry.Name())
		destinationPath := filepath.Join(workspaceRoot, entry.Name())
		info, err := os.Lstat(sourcePath)
		if err != nil {
			return err
		}
		if err := copyResolvedPath(sourcePath, destinationPath, info); err != nil {
			return err
		}
	}

	return nil
}

func captureWorkspaceState(paths []string) (map[string]capturedPathState, error) {
	normalizedPaths, err := normalizeOperationPaths(paths)
	if err != nil {
		return nil, err
	}

	state := map[string]capturedPathState{}
	for _, filePath := range normalizedPaths {
		resolvedPath, relPath, err := resolveToolPath(filePath)
		if err != nil {
			return nil, err
		}
		if err := collectResolvedPathState(resolvedPath, filepath.ToSlash(relPath), state); err != nil {
			return nil, err
		}
	}

	return state, nil
}

func captureSnapshotState(record operationRecord) (map[string]capturedPathState, error) {
	resolvedSnapshotRoot, _, err := resolveToolPath(record.SnapshotPath)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(resolvedSnapshotRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]capturedPathState{}, nil
		}
		return nil, err
	}

	state := map[string]capturedPathState{}
	for _, entry := range entries {
		sourcePath := filepath.Join(resolvedSnapshotRoot, entry.Name())
		if err := collectResolvedPathState(sourcePath, filepath.ToSlash(entry.Name()), state); err != nil {
			return nil, err
		}
	}

	return state, nil
}

func collectResolvedPathState(resolvedPath, keyRoot string, state map[string]capturedPathState) error {
	info, err := os.Lstat(resolvedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if info.IsDir() {
		return filepath.Walk(resolvedPath, func(currentPath string, currentInfo os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !currentInfo.IsDir() && !currentInfo.Mode().IsRegular() {
				return fmt.Errorf("unsupported file type at %s", filepath.Clean(currentPath))
			}

			relPath, err := filepath.Rel(resolvedPath, currentPath)
			if err != nil {
				return err
			}
			key := keyRoot
			if relPath != "." {
				key = path.Join(keyRoot, filepath.ToSlash(relPath))
			}

			entry := capturedPathState{IsDir: currentInfo.IsDir(), Mode: currentInfo.Mode()}
			if currentInfo.Mode().IsRegular() {
				data, err := os.ReadFile(currentPath)
				if err != nil {
					return err
				}
				entry.Data = data
			}
			state[key] = entry
			return nil
		})
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported file type at %s", filepath.Clean(resolvedPath))
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return err
	}
	state[keyRoot] = capturedPathState{IsDir: false, Mode: info.Mode(), Data: data}
	return nil
}

func transplantStateRoot(state map[string]capturedPathState, oldRoot, newRoot string) map[string]capturedPathState {
	oldRoot = filepath.ToSlash(filepath.Clean(oldRoot))
	newRoot = filepath.ToSlash(filepath.Clean(newRoot))

	transplanted := make(map[string]capturedPathState, len(state))
	for key, entry := range state {
		mappedKey := newRoot
		if key != oldRoot {
			suffix := strings.TrimPrefix(key, oldRoot)
			suffix = strings.TrimPrefix(suffix, "/")
			if suffix != "" {
				mappedKey = path.Join(newRoot, suffix)
			}
		}
		transplanted[mappedKey] = entry
	}

	return transplanted
}

func renderStateDiff(oldState, newState map[string]capturedPathState) string {
	keySet := map[string]struct{}{}
	for key := range oldState {
		keySet[key] = struct{}{}
	}
	for key := range newState {
		keySet[key] = struct{}{}
	}

	keys := make([]string, 0, len(keySet))
	for key := range keySet {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var sections []string
	changedEntries := 0
	for _, key := range keys {
		oldEntry, oldOK := oldState[key]
		newEntry, newOK := newState[key]

		switch {
		case oldOK && newOK && oldEntry.IsDir && newEntry.IsDir:
			continue
		case oldOK && newOK && !oldEntry.IsDir && !newEntry.IsDir:
			if bytes.Equal(oldEntry.Data, newEntry.Data) {
				continue
			}
			changedEntries++
			if changedEntries > maxStateDiffEntries {
				sections = append(sections, "... state diff truncated ...")
				return strings.Join(sections, "\n\n")
			}
			sections = append(sections, previewDataChange(key, key, oldEntry.Data, newEntry.Data))
		case oldOK && !newOK:
			changedEntries++
			if changedEntries > maxStateDiffEntries {
				sections = append(sections, "... state diff truncated ...")
				return strings.Join(sections, "\n\n")
			}
			if oldEntry.IsDir {
				sections = append(sections, fmt.Sprintf("- %s", formatStateLabel(key, true)))
			} else {
				sections = append(sections, previewDataChange(key, "/dev/null", oldEntry.Data, nil))
			}
		case !oldOK && newOK:
			changedEntries++
			if changedEntries > maxStateDiffEntries {
				sections = append(sections, "... state diff truncated ...")
				return strings.Join(sections, "\n\n")
			}
			if newEntry.IsDir {
				sections = append(sections, fmt.Sprintf("+ %s", formatStateLabel(key, true)))
			} else {
				sections = append(sections, previewDataChange("/dev/null", key, nil, newEntry.Data))
			}
		case oldOK && newOK:
			changedEntries++
			if changedEntries > maxStateDiffEntries {
				sections = append(sections, "... state diff truncated ...")
				return strings.Join(sections, "\n\n")
			}
			var typeChange []string
			if oldEntry.IsDir {
				typeChange = append(typeChange, fmt.Sprintf("- %s", formatStateLabel(key, true)))
			} else {
				typeChange = append(typeChange, previewDataChange(key, "/dev/null", oldEntry.Data, nil))
			}
			if newEntry.IsDir {
				typeChange = append(typeChange, fmt.Sprintf("+ %s", formatStateLabel(key, true)))
			} else {
				typeChange = append(typeChange, previewDataChange("/dev/null", key, nil, newEntry.Data))
			}
			sections = append(sections, strings.Join(typeChange, "\n\n"))
		}
	}

	if len(sections) == 0 {
		return "(no changes)"
	}

	return strings.Join(sections, "\n\n")
}

func formatStateLabel(key string, isDir bool) string {
	label := key
	if label == "." {
		label = "./"
	}
	if isDir && !strings.HasSuffix(label, "/") {
		label += "/"
	}
	return label
}

func ensureDirectoryPath(dirPath string) (os.FileInfo, error) {
	resolvedPath, _, err := resolveToolPath(dirPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(resolvedPath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", filepath.Clean(dirPath))
	}

	return info, nil
}

func destinationPathAvailableForPath(filePath string, overwrite bool) error {
	resolvedPath, _, err := resolveToolPath(filePath)
	if err != nil {
		return err
	}

	if _, err := os.Stat(resolvedPath); err == nil {
		if !overwrite {
			return fmt.Errorf("destination %s already exists", filepath.Clean(filePath))
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	return nil
}

func pathsOverlap(firstResolvedPath, secondResolvedPath string) bool {
	if firstResolvedPath == secondResolvedPath {
		return true
	}

	relToSecond, err := filepath.Rel(firstResolvedPath, secondResolvedPath)
	if err == nil && relToSecond != ".." && !strings.HasPrefix(relToSecond, ".."+string(os.PathSeparator)) {
		return true
	}

	relToFirst, err := filepath.Rel(secondResolvedPath, firstResolvedPath)
	if err == nil && relToFirst != ".." && !strings.HasPrefix(relToFirst, ".."+string(os.PathSeparator)) {
		return true
	}

	return false
}

func validateDirectoryTransfer(sourcePath, destinationPath string, overwrite bool) (string, string, error) {
	if filepath.Clean(sourcePath) == filepath.Clean(destinationPath) {
		return "", "", fmt.Errorf("source and destination must be different")
	}
	if _, err := ensureDirectoryPath(sourcePath); err != nil {
		return "", "", err
	}
	if err := destinationPathAvailableForPath(destinationPath, overwrite); err != nil {
		return "", "", err
	}

	resolvedSourcePath, _, err := resolveToolPath(sourcePath)
	if err != nil {
		return "", "", err
	}
	resolvedDestinationPath, _, err := resolveToolPath(destinationPath)
	if err != nil {
		return "", "", err
	}
	if pathsOverlap(resolvedSourcePath, resolvedDestinationPath) {
		return "", "", fmt.Errorf("source and destination directories must not overlap")
	}

	return resolvedSourcePath, resolvedDestinationPath, nil
}

func copyDir(sourcePath, destinationPath string, overwrite bool) error {
	resolvedSourcePath, resolvedDestinationPath, err := validateDirectoryTransfer(sourcePath, destinationPath, overwrite)
	if err != nil {
		return err
	}

	return withRecordedMutation("copy_dir", []string{destinationPath}, func() error {
		if overwrite {
			if err := removeWorkspacePathIfExists(destinationPath); err != nil {
				return err
			}
		}
		return copyResolvedDirectory(resolvedSourcePath, resolvedDestinationPath)
	})
}

func previewCopyDir(sourcePath, destinationPath string, overwrite bool) (string, error) {
	if _, _, err := validateDirectoryTransfer(sourcePath, destinationPath, overwrite); err != nil {
		return "", err
	}

	sourceState, err := captureWorkspaceState([]string{sourcePath})
	if err != nil {
		return "", err
	}
	destinationState, err := captureWorkspaceState([]string{destinationPath})
	if err != nil {
		return "", err
	}

	desiredState := transplantStateRoot(sourceState, sourcePath, destinationPath)
	return fmt.Sprintf("Preview for copy_dir from %s to %s:\n%s", filepath.ToSlash(filepath.Clean(sourcePath)), filepath.ToSlash(filepath.Clean(destinationPath)), renderStateDiff(destinationState, desiredState)), nil
}

func moveDir(sourcePath, destinationPath string, overwrite bool) error {
	resolvedSourcePath, resolvedDestinationPath, err := validateDirectoryTransfer(sourcePath, destinationPath, overwrite)
	if err != nil {
		return err
	}

	return withRecordedMutation("move_dir", []string{sourcePath, destinationPath}, func() error {
		if overwrite {
			if err := removeWorkspacePathIfExists(destinationPath); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(resolvedDestinationPath), 0755); err != nil {
			return err
		}
		if err := os.Rename(resolvedSourcePath, resolvedDestinationPath); err == nil {
			return nil
		}
		if err := copyResolvedDirectory(resolvedSourcePath, resolvedDestinationPath); err != nil {
			return err
		}
		return removeWorkspacePathIfExists(sourcePath)
	})
}

func previewMoveDir(sourcePath, destinationPath string, overwrite bool) (string, error) {
	if _, _, err := validateDirectoryTransfer(sourcePath, destinationPath, overwrite); err != nil {
		return "", err
	}

	currentState, err := captureWorkspaceState([]string{sourcePath, destinationPath})
	if err != nil {
		return "", err
	}
	sourceState, err := captureWorkspaceState([]string{sourcePath})
	if err != nil {
		return "", err
	}

	desiredState := transplantStateRoot(sourceState, sourcePath, destinationPath)
	return fmt.Sprintf("Preview for move_dir from %s to %s:\n%s", filepath.ToSlash(filepath.Clean(sourcePath)), filepath.ToSlash(filepath.Clean(destinationPath)), renderStateDiff(currentState, desiredState)), nil
}

func deleteDir(dirPath string) error {
	if _, err := ensureDirectoryPath(dirPath); err != nil {
		return err
	}

	return withRecordedMutation("delete_dir", []string{dirPath}, func() error {
		return removeWorkspacePathIfExists(dirPath)
	})
}

func previewDeleteDir(dirPath string) (string, error) {
	if _, err := ensureDirectoryPath(dirPath); err != nil {
		return "", err
	}

	currentState, err := captureWorkspaceState([]string{dirPath})
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Preview for delete_dir on %s:\n%s", filepath.ToSlash(filepath.Clean(dirPath)), renderStateDiff(currentState, map[string]capturedPathState{})), nil
}
