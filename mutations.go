package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	defaultOperationsPath = stateDirName + "/operations.json"
	defaultSnapshotsRoot  = stateDirName + "/snapshots"
	maxStateDiffEntries   = 40
	// maxRecordedOperations is how many operations stay undoable; older snapshots are deleted.
	maxRecordedOperations = 100
	// unreferencedSnapshotGracePeriod is how old an unrecorded snapshot must be before
	// it is treated as abandoned.
	unreferencedSnapshotGracePeriod = time.Hour
)

type operationRecord struct {
	ID        string   `json:"id"`
	Timestamp string   `json:"timestamp"`
	Kind      string   `json:"kind"`
	Paths     []string `json:"paths"`
	// CreatedParents maps a path to the topmost parent directory the operation had to
	// create for it, so undo can remove those directories again.
	CreatedParents map[string]string `json:"created_parents,omitempty"`
	// FileIdentities records which file each regular-file path named, so undo only
	// rewrites a file in place when that same file is still there.
	FileIdentities map[string]string `json:"file_identities,omitempty"`
	UndoneAt       string            `json:"undone_at,omitempty"`
	// Skipped marks an operation that undo gave up on without restoring it.
	Skipped bool `json:"skipped,omitempty"`
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

	if _, err := os.Lstat(resolvedPath); err != nil {
		if isMissing(err) {
			return nil
		}
		return err
	}

	return removeAllMakingWritable(resolvedPath)
}

// dirMode keeps the permission and special bits that os.Chmod can set.
func dirMode(mode fs.FileMode) fs.FileMode {
	return mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

// removeAllMakingWritable works like os.RemoveAll, but when read-only directories
// block the removal it makes them owner-writable and tries again. If that still fails,
// the directories that remain get their original modes back.
func removeAllMakingWritable(resolvedPath string) error {
	err := os.RemoveAll(resolvedPath)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	if info, statErr := os.Lstat(resolvedPath); statErr != nil || !info.IsDir() {
		return err
	}

	type changedDir struct {
		path string
		mode fs.FileMode
	}
	var changedDirs []changedDir
	_ = filepath.WalkDir(resolvedPath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || !d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil || info.Mode().Perm()&0700 == 0700 {
			return nil
		}
		if os.Chmod(path, dirMode(info.Mode())|0700) == nil {
			changedDirs = append(changedDirs, changedDir{path: path, mode: dirMode(info.Mode())})
		}
		return nil
	})

	if err := os.RemoveAll(resolvedPath); err != nil {
		// Restore children before parents, while the parents are still searchable.
		for i := len(changedDirs) - 1; i >= 0; i-- {
			if _, statErr := os.Lstat(changedDirs[i].path); statErr == nil {
				_ = os.Chmod(changedDirs[i].path, changedDirs[i].mode)
			}
		}
		return err
	}
	return nil
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
		if relPath == "." {
			return nil, fmt.Errorf("refusing to operate on the workspace root")
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

var operationIDPattern = regexp.MustCompile(`^[0-9]+$`)

var errSnapshotMissing = errors.New("the undo snapshot is missing")

// recordSnapshotPath derives a record's snapshot directory from its ID rather than
// trusting the path stored in operations.json, which is later deleted recursively.
func recordSnapshotPath(record operationRecord) (string, error) {
	if !operationIDPattern.MatchString(record.ID) {
		return "", fmt.Errorf("invalid operation id %q", record.ID)
	}
	return operationSnapshotPath(record.ID), nil
}

func operationSnapshotPath(id string) string {
	return filepath.ToSlash(filepath.Join(defaultSnapshotsRoot, id, "before"))
}

func newOperationRecord(kind string, paths []string) (operationRecord, error) {
	normalizedPaths, err := normalizeOperationPaths(paths)
	if err != nil {
		return operationRecord{}, err
	}

	createdParents := map[string]string{}
	fileIdentities := map[string]string{}
	for _, relPath := range normalizedPaths {
		top, err := topmostMissingParent(relPath)
		if err != nil {
			return operationRecord{}, err
		}
		if top != "" {
			createdParents[relPath] = top
		}
		if identity := regularFileIdentity(relPath); identity != "" {
			fileIdentities[relPath] = identity
		}
	}
	if len(createdParents) == 0 {
		createdParents = nil
	}
	if len(fileIdentities) == 0 {
		fileIdentities = nil
	}

	id := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	return operationRecord{
		ID:             id,
		Timestamp:      time.Now().UTC().Format(time.RFC3339Nano),
		Kind:           kind,
		Paths:          normalizedPaths,
		CreatedParents: createdParents,
		FileIdentities: fileIdentities,
	}, nil
}

// regularFileIdentity returns the identity of the regular file at relPath, or "".
func regularFileIdentity(relPath string) string {
	resolvedPath, _, err := resolveToolPath(relPath)
	if err != nil {
		return ""
	}
	info, err := os.Lstat(resolvedPath)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return fileIdentity(info)
}

// topmostMissingParent returns the highest parent directory of relPath that does not
// exist yet, or "" when its parent already exists.
func topmostMissingParent(relPath string) (string, error) {
	top := ""
	for dir := path.Dir(relPath); dir != "."; dir = path.Dir(dir) {
		resolvedDir, _, err := resolveToolPath(dir)
		if err != nil {
			return "", err
		}
		if _, err := os.Lstat(resolvedDir); err == nil {
			break
		} else if !isMissing(err) {
			return "", err
		}
		top = dir
	}
	return top, nil
}

// removeCreatedParents removes the now-empty parent directories an operation created.
func removeCreatedParents(record operationRecord) {
	for relPath, top := range record.CreatedParents {
		parent := path.Dir(relPath)
		if parent != top && !strings.HasPrefix(parent, top+"/") {
			continue
		}

		// Walk down from the topmost created directory and stop at the first entry that
		// is no longer a real directory, so nothing reached through a symlink put there
		// since is removed.
		var created []string
		dir := top
		for _, component := range append([]string{""}, strings.Split(strings.TrimPrefix(strings.TrimPrefix(parent, top), "/"), "/")...) {
			if component != "" {
				dir = path.Join(dir, component)
			}
			resolvedDir, _, err := resolveToolPath(dir)
			if err != nil {
				break
			}
			if info, err := os.Lstat(resolvedDir); err != nil || !info.IsDir() {
				break
			}
			created = append(created, resolvedDir)
		}

		// os.Remove only removes empty directories, so anything added since stays.
		for i := len(created) - 1; i >= 0; i-- {
			if err := os.Remove(created[i]); err != nil && !isMissing(err) {
				break
			}
		}
	}
}

func createOperationSnapshot(record operationRecord) error {
	snapshotPath, err := recordSnapshotPath(record)
	if err != nil {
		return err
	}
	resolvedSnapshotRoot, _, err := resolveStatePath(snapshotPath)
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
	snapshotPath, err := recordSnapshotPath(record)
	if err != nil {
		return
	}
	resolvedSnapshotRoot, _, err := resolveStatePath(snapshotPath)
	if err != nil {
		return
	}
	_ = removeAllMakingWritable(filepath.Dir(resolvedSnapshotRoot))
}

func snapshotWorkspacePath(filePath, resolvedSnapshotRoot string) error {
	resolvedPath, relPath, err := resolveToolPath(filePath)
	if err != nil {
		return err
	}

	info, err := os.Lstat(resolvedPath)
	if err != nil {
		if isMissing(err) {
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
	// WriteFile reuses an existing file, keeping its name and hard links; Chmod then
	// applies the exact mode, which the umask may have narrowed for a new file.
	if err := os.WriteFile(destinationResolvedPath, data, mode.Perm()); err != nil {
		return err
	}
	return os.Chmod(destinationResolvedPath, mode.Perm())
}

// copyResolvedDirectory creates directories writable by the owner while it fills them,
// then gives them the source's mode, so read-only directories can be copied. Directories
// that already exist at the destination keep their own mode.
func copyResolvedDirectory(sourceResolvedPath, destinationResolvedPath string) error {
	type createdDir struct {
		path string
		mode fs.FileMode
	}
	var createdDirs []createdDir

	err := filepath.Walk(sourceResolvedPath, func(currentPath string, info os.FileInfo, walkErr error) error {
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
			if existing, err := os.Lstat(destinationPath); err == nil {
				if !existing.IsDir() {
					return fmt.Errorf("%s exists and is not a directory", filepath.Clean(destinationPath))
				}
				return nil
			} else if !os.IsNotExist(err) {
				return err
			}
			createdDirs = append(createdDirs, createdDir{path: destinationPath, mode: dirMode(info.Mode())})
			return os.MkdirAll(destinationPath, info.Mode().Perm()|0700)
		}

		return copyResolvedRegularFile(currentPath, destinationPath, info.Mode())
	})
	if err != nil {
		return err
	}

	for i := len(createdDirs) - 1; i >= 0; i-- {
		if err := os.Chmod(createdDirs[i].path, createdDirs[i].mode); err != nil {
			return err
		}
	}
	return nil
}

func withRecordedMutation(kind string, paths []string, mutate func() error) error {
	record, err := newOperationRecord(kind, paths)
	if err != nil {
		return err
	}
	if err := createOperationSnapshot(record); err != nil {
		cleanupOperationSnapshot(record)
		return fmt.Errorf("snapshot before %s: %w", kind, err)
	}

	if err := mutate(); err != nil {
		// Most failures happen before anything changes; leave those paths untouched.
		if workspaceMatchesSnapshot(record) {
			cleanupOperationSnapshot(record)
			return err
		}
		// Otherwise put the snapshot back. If that fails too, keep the record so undo
		// can retry once the cause is fixed.
		if restoreErr := restoreOperationSnapshot(record); restoreErr != nil {
			if appendErr := appendOperationRecord(record); appendErr != nil {
				// No record points at the snapshot now, and it may hold the only copy of
				// what was there. Keep it from being collected and say where it is.
				location := keepOperationSnapshot(record)
				return fmt.Errorf("%w (restoring the previous state failed: %v; recording it for undo failed: %v; the previous state is saved in %s)", err, restoreErr, appendErr, location)
			}
			return fmt.Errorf("%w (restoring the previous state failed: %v; undo_last_change can retry)", err, restoreErr)
		}
		cleanupOperationSnapshot(record)
		return err
	}

	if err := appendOperationRecord(record); err != nil {
		// The change was made; it just cannot be undone.
		cleanupOperationSnapshot(record)
		fmt.Fprintf(os.Stderr, "[warn] %s succeeded but could not be recorded for undo: %v\n", kind, err)
	}

	return nil
}

// workspaceMatchesSnapshot reports whether the record's paths are exactly as they were
// when its snapshot was taken.
func workspaceMatchesSnapshot(record operationRecord) bool {
	current, err := captureWorkspaceState(record.Paths)
	if err != nil {
		return false
	}
	snapshot, err := captureSnapshotState(record)
	if err != nil {
		return false
	}

	// The snapshot also holds the parent directories of each path; compare only the paths.
	compared := 0
	for key, entry := range snapshot {
		if !stateKeyWithinPaths(key, record.Paths) {
			continue
		}
		compared++
		other, ok := current[key]
		if !ok || other.IsDir != entry.IsDir || !bytes.Equal(other.Data, entry.Data) {
			return false
		}
	}
	return compared == len(current)
}

func stateKeyWithinPaths(key string, paths []string) bool {
	for _, candidate := range paths {
		if key == candidate || strings.HasPrefix(key, candidate+"/") {
			return true
		}
	}
	return false
}

// loadOperationRecords reads the operation log. When the log cannot be decoded and
// setAsideCorrupt is set, it is renamed out of the way and an empty log is returned so
// recording and undo keep working; read-only callers get the error instead.
func loadOperationRecords(setAsideCorrupt bool) ([]operationRecord, error) {
	resolvedPath, _, err := resolveStatePath(defaultOperationsPath)
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
		if !setAsideCorrupt {
			return nil, fmt.Errorf("decode operations: %w", err)
		}
		corruptPath := fmt.Sprintf("%s.corrupt-%d", resolvedPath, time.Now().UnixNano())
		if renameErr := os.Rename(resolvedPath, corruptPath); renameErr != nil {
			return nil, fmt.Errorf("decode operations: %w", err)
		}
		fmt.Fprintf(os.Stderr, "[warn] %s was unreadable (%v); moved it to %s and started a new operation log\n", defaultOperationsPath, err, filepath.Base(corruptPath))
		return nil, nil
	}

	return records, nil
}

func saveOperationRecords(records []operationRecord) error {
	resolvedPath, _, err := resolveStatePath(defaultOperationsPath)
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

	// Write a temporary file and rename it so an interrupted save never truncates the log.
	tempFile, err := os.CreateTemp(filepath.Dir(resolvedPath), "operations-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tempFile.Name())
	if _, err := tempFile.Write(encoded); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	return os.Rename(tempFile.Name(), resolvedPath)
}

func appendOperationRecord(record operationRecord) error {
	records, err := loadOperationRecords(true)
	if err != nil {
		return err
	}

	records = append(records, record)
	var expired []operationRecord
	if excess := len(records) - maxRecordedOperations; excess > 0 {
		expired = records[:excess]
		records = append([]operationRecord(nil), records[excess:]...)
	}
	if err := saveOperationRecords(records); err != nil {
		return err
	}

	// Expired snapshots are deleted only once the saved log no longer refers to them.
	for _, record := range expired {
		cleanupOperationSnapshot(record)
	}
	removeUnreferencedSnapshots(records)
	return nil
}

const keepSnapshotMarker = "KEEP"

// keepOperationSnapshot marks a snapshot that no record refers to as one to keep, and
// returns its location for the user.
func keepOperationSnapshot(record operationRecord) string {
	snapshotPath, err := recordSnapshotPath(record)
	if err != nil {
		return "(unknown)"
	}
	resolvedSnapshotRoot, relPath, err := resolveStatePath(snapshotPath)
	if err != nil {
		return "(unknown)"
	}
	_ = os.WriteFile(filepath.Join(filepath.Dir(resolvedSnapshotRoot), keepSnapshotMarker), nil, 0644)
	return filepath.ToSlash(relPath)
}

// removeUnreferencedSnapshots deletes snapshot directories that no record points to,
// such as those left by a log that was set aside or by an interrupted operation. Recent
// directories are kept, since another goat process may still be in the middle of the
// mutation that created them.
func removeUnreferencedSnapshots(records []operationRecord) {
	resolvedRoot, _, err := resolveStatePath(defaultSnapshotsRoot)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(resolvedRoot)
	if err != nil {
		return
	}

	referenced := make(map[string]struct{}, len(records))
	for _, record := range records {
		referenced[record.ID] = struct{}{}
	}
	for _, entry := range entries {
		if _, ok := referenced[entry.Name()]; ok {
			continue
		}
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < unreferencedSnapshotGracePeriod {
			continue
		}
		if _, err := os.Lstat(filepath.Join(resolvedRoot, entry.Name(), keepSnapshotMarker)); err == nil {
			continue
		}
		_ = removeAllMakingWritable(filepath.Join(resolvedRoot, entry.Name()))
	}
}

func latestUndoableOperationRecord(setAsideCorrupt bool) (operationRecord, int, []operationRecord, error) {
	records, err := loadOperationRecords(setAsideCorrupt)
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
	record, index, _, err := latestUndoableOperationRecord(false)
	if err != nil {
		return "", err
	}
	if index < 0 {
		return "No recorded operations available to undo.", nil
	}

	if !operationSnapshotExists(record) {
		return fmt.Sprintf("The undo snapshot for %s on %s is missing, so it cannot be restored; undo_last_change will skip it.", record.Kind, strings.Join(record.Paths, ", ")), nil
	}

	currentState, err := captureWorkspaceState(record.Paths)
	if err != nil {
		return "", err
	}
	snapshotState, err := captureSnapshotState(record)
	if err != nil {
		return "", err
	}
	// The snapshot also holds each path's parent directories, which undo leaves alone.
	previousState := map[string]capturedPathState{}
	for key, entry := range snapshotState {
		if stateKeyWithinPaths(key, record.Paths) {
			previousState[key] = entry
		}
	}

	return fmt.Sprintf("Preview for undo_last_change on %s (%s):\n%s", record.Kind, strings.Join(record.Paths, ", "), renderStateDiff(currentState, previousState)), nil
}

func undoLastChange() (string, error) {
	record, index, records, err := latestUndoableOperationRecord(true)
	if err != nil {
		return "", err
	}
	if index < 0 {
		return "No recorded operations available to undo.", nil
	}

	if err := restoreOperationSnapshot(record); err != nil {
		if !errors.Is(err, errSnapshotMissing) {
			return "", fmt.Errorf("%w (the user can run /undo skip to give up on this operation and undo earlier ones)", err)
		}
		// Nothing can ever restore this operation; mark it so undo moves on to earlier ones.
		if _, skipErr := markOperationSkipped(records, index); skipErr != nil {
			return "", fmt.Errorf("%w (marking it skipped failed: %v)", err, skipErr)
		}
		return "", fmt.Errorf("%w; skipped it, so undo again to undo the operation before it", err)
	}

	records[index].UndoneAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveOperationRecords(records); err != nil {
		return "", err
	}

	return fmt.Sprintf("Undid %s for %s.", record.Kind, strings.Join(record.Paths, ", ")), nil
}

func markOperationSkipped(records []operationRecord, index int) (operationRecord, error) {
	records[index].UndoneAt = time.Now().UTC().Format(time.RFC3339Nano)
	records[index].Skipped = true
	return records[index], saveOperationRecords(records)
}

// skipLastChange gives up on the latest undoable operation without restoring anything,
// so undo can reach earlier operations.
func skipLastChange() (string, error) {
	_, index, records, err := latestUndoableOperationRecord(true)
	if err != nil {
		return "", err
	}
	if index < 0 {
		return "No recorded operations available to skip.", nil
	}

	record, err := markOperationSkipped(records, index)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Skipped %s for %s without restoring it; undo now reaches the operation before it.", record.Kind, strings.Join(record.Paths, ", ")), nil
}

func operationSnapshotExists(record operationRecord) bool {
	snapshotPath, err := recordSnapshotPath(record)
	if err != nil {
		return false
	}
	resolvedSnapshotRoot, _, err := resolveStatePath(snapshotPath)
	if err != nil {
		return false
	}
	info, err := os.Stat(resolvedSnapshotRoot)
	return err == nil && info.IsDir()
}

func restoreOperationSnapshot(record operationRecord) error {
	// Validate the recorded paths again: operations.json lives on disk and could be edited.
	paths, err := normalizeOperationPaths(record.Paths)
	if err != nil {
		return fmt.Errorf("invalid operation record: %w", err)
	}

	// Every record gets a snapshot directory, even when its paths did not exist yet.
	// Check it is there before deleting anything it is meant to replace.
	snapshotPath, err := recordSnapshotPath(record)
	if err != nil {
		return err
	}
	resolvedSnapshotRoot, _, err := resolveStatePath(snapshotPath)
	if err != nil {
		return err
	}
	if !operationSnapshotExists(record) {
		return fmt.Errorf("%w for %s on %s; nothing was restored", errSnapshotMissing, record.Kind, strings.Join(record.Paths, ", "))
	}

	sort.Slice(paths, func(i, j int) bool {
		return len(paths[i]) > len(paths[j])
	})
	for _, filePath := range paths {
		// A file that is still the same file is rewritten in place below, which keeps its
		// on-disk name (case included) and its hard links. Any other file now at the path
		// is replaced, so its other hard links are never written to.
		if restoreInPlace(record, resolvedSnapshotRoot, filePath) {
			continue
		}
		if err := removeWorkspacePathIfExists(filePath); err != nil {
			return err
		}
	}

	// Copy back only each recorded path's own subtree, never its parent directories.
	// Each destination is validated right before it is written, because restoring one
	// path can change where a symlink in another path's parents leads.
	for i := len(paths) - 1; i >= 0; i-- {
		sourcePath := filepath.Join(resolvedSnapshotRoot, filepath.FromSlash(paths[i]))
		info, err := os.Lstat(sourcePath)
		if os.IsNotExist(err) {
			continue // the path did not exist before the operation
		} else if err != nil {
			return err
		}

		destinationPath, _, err := resolveToolPath(paths[i])
		if err != nil {
			return fmt.Errorf("restore %s: %w", paths[i], err)
		}
		err = copyResolvedPath(sourcePath, destinationPath, info)
		if err != nil && errors.Is(err, fs.ErrPermission) && info.Mode().IsRegular() {
			// The file kept in place may have become read-only; replace it instead.
			if removeErr := removeWorkspacePathIfExists(paths[i]); removeErr == nil {
				err = copyResolvedPath(sourcePath, destinationPath, info)
			}
		}
		if err != nil {
			return err
		}
	}

	removeCreatedParents(record)
	return nil
}

func restoreInPlace(record operationRecord, resolvedSnapshotRoot, relPath string) bool {
	recorded := record.FileIdentities[relPath]
	if recorded == "" {
		return false
	}
	snapshotInfo, err := os.Lstat(filepath.Join(resolvedSnapshotRoot, filepath.FromSlash(relPath)))
	if err != nil || !snapshotInfo.Mode().IsRegular() {
		return false
	}
	return regularFileIdentity(relPath) == recorded
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
	snapshotPath, err := recordSnapshotPath(record)
	if err != nil {
		return nil, err
	}
	resolvedSnapshotRoot, _, err := resolveStatePath(snapshotPath)
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
		if isMissing(err) {
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
	// Compare file identity as well, so case variants and symlinked aliases of the
	// same directory are caught.
	if isInsideExistingDir(firstResolvedPath, secondResolvedPath) || isInsideExistingDir(secondResolvedPath, firstResolvedPath) {
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
