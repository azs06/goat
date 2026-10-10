//go:build unix

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResolveToolPathRejectsStateDirectory(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	for _, path := range []string{".goat", ".goat/history.jsonl", "./.goat/operations.json", "sub/../.goat"} {
		if _, _, err := resolveToolPath(path); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("%s: expected reserved-path error, got %v", path, err)
		}
	}
	if err := writeFile(".goat/operations.json", "[]"); err == nil {
		t.Fatal("expected write_file into .goat to fail")
	}
	if _, _, err := resolveStatePath(".goat/history.jsonl"); err != nil {
		t.Fatalf("expected internal state path to resolve, got %v", err)
	}
}

func TestResolveToolPathRejectsSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0644); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatalf("create dir symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(workspace, "secret-link")); err != nil {
		t.Fatalf("create file symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(workspace, ".goat"), filepath.Join(workspace, "state-link")); err != nil {
		t.Fatalf("create state symlink: %v", err)
	}

	if _, err := readFileForModel("escape/secret.txt"); err == nil {
		t.Fatal("expected read through a directory symlink to fail")
	}
	if _, err := readFileForModel("secret-link"); err == nil {
		t.Fatal("expected read through a file symlink to fail")
	}
	if err := writeFile("escape/new.txt", "x"); err == nil {
		t.Fatal("expected write through a directory symlink to fail")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected no file outside the workspace, got %v", err)
	}
	if err := writeFile("state-link/operations.json", "[]"); err == nil {
		t.Fatal("expected write through a symlink into .goat to fail")
	}

	if err := os.MkdirAll(filepath.Join(workspace, "real"), 0755); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	if err := os.Symlink(filepath.Join(workspace, "real"), filepath.Join(workspace, "inside")); err != nil {
		t.Fatalf("create inside symlink: %v", err)
	}
	if err := writeFile("inside/ok.txt", "ok"); err != nil {
		t.Fatalf("expected write through an in-workspace symlink to work, got %v", err)
	}
}

func TestGrepFilesSkipsSymlinksAndUnreadableDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission test needs a non-root user")
	}
	workspace := t.TempDir()
	outside := t.TempDir()
	chdirForTest(t, workspace)

	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("needle outside"), 0644)
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(workspace, "link.txt"))
	os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("needle inside"), 0644)
	locked := filepath.Join(workspace, "locked")
	os.Mkdir(locked, 0755)
	os.WriteFile(filepath.Join(locked, "b.txt"), []byte("needle locked"), 0644)
	os.Chmod(locked, 0)
	t.Cleanup(func() { os.Chmod(locked, 0755) })

	matches, _, err := grepFiles(".", "needle", false, 0)
	if err != nil {
		t.Fatalf("expected unreadable entries to be skipped, got %v", err)
	}
	if len(matches) != 1 || !strings.HasPrefix(matches[0], "a.txt:") {
		t.Fatalf("expected only a.txt to match, got %q", matches)
	}
}

func TestDeleteDirRefusesWorkspaceRoot(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)
	os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("hi"), 0644)

	for _, path := range []string{".", "./", "sub/.."} {
		if err := deleteDir(path); err == nil || !strings.Contains(err.Error(), "workspace root") {
			t.Fatalf("%s: expected workspace root refusal, got %v", path, err)
		}
		if _, err := previewDeleteDir(path); err == nil {
			t.Fatalf("%s: expected preview to refuse the workspace root", path)
		}
	}

	if _, err := os.Stat(filepath.Join(workspace, "a.txt")); err != nil {
		t.Fatalf("expected workspace to be untouched, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".goat", "snapshots")); !os.IsNotExist(err) {
		t.Fatalf("expected no snapshot to be created, got %v", err)
	}
}

func TestFailedSnapshotIsCleanedUp(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	os.MkdirAll(filepath.Join(workspace, "dir"), 0755)
	os.WriteFile(filepath.Join(workspace, "dir", "a.txt"), []byte("a"), 0644)
	// A named pipe is not a regular file, so snapshotting the directory fails.
	if err := syscall.Mkfifo(filepath.Join(workspace, "dir", "pipe"), 0644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	if err := deleteDir("dir"); err == nil {
		t.Fatal("expected snapshot failure to abort the delete")
	}
	if _, err := os.Stat(filepath.Join(workspace, "dir", "a.txt")); err != nil {
		t.Fatalf("expected directory to be untouched, got %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(workspace, ".goat", "snapshots"))
	if len(entries) != 0 {
		t.Fatalf("expected failed snapshot to be removed, found %d entr(ies)", len(entries))
	}
}

func TestUndoRestoresEachMutationKind(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	read := func(path string) string {
		data, err := os.ReadFile(filepath.Join(workspace, path))
		if err != nil {
			return "<missing>"
		}
		return string(data)
	}

	os.MkdirAll(filepath.Join(workspace, "src", "nested"), 0755)
	os.WriteFile(filepath.Join(workspace, "src", "a.txt"), []byte("original"), 0644)
	os.WriteFile(filepath.Join(workspace, "src", "nested", "b.txt"), []byte("nested"), 0644)

	steps := []struct {
		name   string
		mutate func() error
		check  func() bool
	}{
		{"write new file", func() error { return writeFile("new.txt", "fresh") }, func() bool { return read("new.txt") == "<missing>" }},
		{"edit file", func() error { _, err := editFile("src/a.txt", "original", "edited", false); return err }, func() bool { return read("src/a.txt") == "original" }},
		{"move file", func() error { return moveFile("src/a.txt", "moved.txt", false) }, func() bool { return read("src/a.txt") == "original" && read("moved.txt") == "<missing>" }},
		{"delete file", func() error { return deleteFile("src/a.txt") }, func() bool { return read("src/a.txt") == "original" }},
		{"copy dir", func() error { return copyDir("src", "copy", false) }, func() bool { return read("copy/nested/b.txt") == "<missing>" }},
		{"move dir", func() error { return moveDir("src", "elsewhere/src", false) }, func() bool {
			return read("src/nested/b.txt") == "nested" && read("elsewhere/src/nested/b.txt") == "<missing>"
		}},
		{"delete dir", func() error { return deleteDir("src") }, func() bool { return read("src/a.txt") == "original" && read("src/nested/b.txt") == "nested" }},
	}

	for _, step := range steps {
		if err := step.mutate(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if _, err := undoLastChange(); err != nil {
			t.Fatalf("%s: undo failed: %v", step.name, err)
		}
		if !step.check() {
			t.Fatalf("%s: undo did not restore the previous state", step.name)
		}
	}

	message, err := undoLastChange()
	if err != nil {
		t.Fatalf("undo with nothing left: %v", err)
	}
	if !strings.Contains(message, "No recorded operations") {
		t.Fatalf("expected nothing left to undo, got %q", message)
	}
}

func TestOldSnapshotsArePruned(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	for i := 0; i < maxRecordedOperations+5; i++ {
		if err := writeFile("counter.txt", strconv.Itoa(i)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	records, err := loadOperationRecords(false)
	if err != nil {
		t.Fatalf("load records: %v", err)
	}
	if len(records) != maxRecordedOperations {
		t.Fatalf("expected %d records, got %d", maxRecordedOperations, len(records))
	}
	entries, err := os.ReadDir(filepath.Join(workspace, ".goat", "snapshots"))
	if err != nil {
		t.Fatalf("read snapshots: %v", err)
	}
	if len(entries) != maxRecordedOperations {
		t.Fatalf("expected %d snapshot dirs, got %d", maxRecordedOperations, len(entries))
	}
}

func TestReadFileForModelRefusesLargeFiles(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	os.WriteFile(filepath.Join(workspace, "big.txt"), make([]byte, maxReadFileSize+1), 0644)
	if _, err := readFileForModel("big.txt"); err == nil || !strings.Contains(err.Error(), "read_file limit") {
		t.Fatalf("expected size limit error, got %v", err)
	}
	if _, err := readFileForModel("."); err == nil {
		t.Fatal("expected reading a directory to fail")
	}
}

func TestHistorySurvivesHugeAndCorruptEntries(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	recordHistory(historyEntry{Type: "tool_output", ToolName: "run_bash", Content: strings.Repeat("x", 2<<20)})
	recordHistory(historyEntry{Type: "assistant", Content: "done", ResponseID: "resp-1"})

	historyPath := filepath.Join(workspace, ".goat", "history.jsonl")
	file, err := os.OpenFile(historyPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open history: %v", err)
	}
	file.WriteString(`{"type":"assistant","content":"cut sho` + "\n")
	file.Close()

	info, err := os.Stat(historyPath)
	if err != nil {
		t.Fatalf("stat history: %v", err)
	}
	if info.Size() > 1<<20 {
		t.Fatalf("expected huge content to be truncated before saving, history is %d bytes", info.Size())
	}

	entries, err := loadConversationHistoryEntries()
	if err != nil {
		t.Fatalf("expected history to load, got %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 readable entries, got %d", len(entries))
	}
	responseID, err := loadLatestConversationResponseID()
	if err != nil || responseID != "resp-1" {
		t.Fatalf("expected resp-1, got %q (%v)", responseID, err)
	}
}

func TestBuildTextDiffStaysBoundedForLargeFiles(t *testing.T) {
	var builder strings.Builder
	for i := 0; i < 20000; i++ {
		builder.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	oldContent := builder.String()
	newContent := strings.Replace(oldContent, "line 10000\n", "changed\n", 1)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	diff := buildTextDiff("a", "b", oldContent, newContent)
	runtime.ReadMemStats(&after)

	if !strings.Contains(diff, "-line 10000") || !strings.Contains(diff, "+changed") {
		t.Fatalf("expected the changed line in the diff, got %q", diff)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<20 {
		t.Fatalf("expected a bounded diff, allocated %d MB", allocated>>20)
	}

	reversed := strings.Join(reverseLines(strings.Split(strings.TrimSuffix(oldContent, "\n"), "\n")), "\n") + "\n"
	runtime.ReadMemStats(&before)
	buildTextDiff("a", "b", oldContent, reversed)
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<20 {
		t.Fatalf("expected a fully rewritten file to fall back to a bounded diff, allocated %d MB", allocated>>20)
	}
}

func reverseLines(lines []string) []string {
	reversed := make([]string, len(lines))
	for i, line := range lines {
		reversed[len(lines)-1-i] = line
	}
	return reversed
}

func TestRunBashReturnsWhileBackgroundProcessRuns(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	start := time.Now()
	output, err := runBashCommand(context.Background(), "sleep 20 & echo $! > bg.pid; echo started", ".")
	t.Cleanup(func() { killPIDFile(filepath.Join(workspace, "bg.pid")) })
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("expected run_bash to return promptly, took %s", elapsed)
	}
	if !strings.Contains(output, "started") || !strings.Contains(output, "background process") {
		t.Fatalf("expected output and a background note, got %q", output)
	}
}

func TestRunBashTimeoutKillsProcessGroup(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	previousTimeout := bashToolTimeout
	bashToolTimeout = time.Second
	t.Cleanup(func() { bashToolTimeout = previousTimeout })

	start := time.Now()
	_, err := runBashCommand(context.Background(), "sleep 30 & echo $! > bg.pid; sleep 30", ".")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("expected the timeout to be enforced, took %s", elapsed)
	}

	pid := readPIDFile(t, filepath.Join(workspace, "bg.pid"))
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("expected the background child to be killed with the shell")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunBashReportsInterruption(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := runBashCommand(ctx, "sleep 30", ".")
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("expected interruption error, got %v", err)
	}
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse pid: %v", err)
	}
	return pid
}

func killPIDFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

func caseInsensitiveFilesystem(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "CaseProbe")
	if err := os.WriteFile(probe, nil, 0644); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	defer os.Remove(probe)
	_, err := os.Stat(filepath.Join(dir, "caseprobe"))
	return err == nil
}

func TestStateDirectoryCaseVariantsAreReserved(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := writeFile("a.txt", "a"); err != nil {
		t.Fatalf("seed operation log: %v", err)
	}

	for _, path := range []string{".GOAT/operations.json", ".Goat", ".goat./x", ".goat /x"} {
		if _, _, err := resolveToolPath(path); err == nil {
			t.Errorf("%s: expected reserved-path error", path)
		}
	}
	if !shouldSkipDir(".GOAT") {
		t.Error("expected searches to skip case variants of .goat")
	}
	if err := os.Symlink(filepath.Join(workspace, ".goat"), filepath.Join(workspace, "alias")); err != nil {
		t.Fatalf("create alias: %v", err)
	}
	if _, err := readFileForModel("alias/operations.json"); err == nil {
		t.Error("expected a symlink alias of .goat to be reserved")
	}
}

func TestForgedSnapshotPathCannotDeleteWorkspace(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	os.WriteFile(filepath.Join(workspace, "keep.txt"), []byte("keep"), 0644)
	os.MkdirAll(filepath.Join(workspace, "src"), 0755)
	os.WriteFile(filepath.Join(workspace, "src", "k.go"), []byte("package k"), 0644)

	var forged []operationRecord
	for i := 0; i < maxRecordedOperations; i++ {
		forged = append(forged, operationRecord{ID: strconv.Itoa(i + 1), Kind: "write_file", Paths: []string{"x.txt"}})
	}
	forged[0].ID = "../../.."
	forged[1].ID = "../../src"
	if err := saveOperationRecords(forged); err != nil {
		t.Fatalf("save forged records: %v", err)
	}

	if err := writeFile("b.txt", "b"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := writeFile("c.txt", "c"); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, path := range []string{"keep.txt", "src/k.go", "b.txt"} {
		if _, err := os.Stat(filepath.Join(workspace, path)); err != nil {
			t.Fatalf("expected %s to survive pruning forged records, got %v", path, err)
		}
	}

	forgedUndo := []operationRecord{{ID: "1", Kind: "write_file", Paths: []string{"."}}}
	if err := saveOperationRecords(forgedUndo); err != nil {
		t.Fatalf("save forged undo record: %v", err)
	}
	if _, err := undoLastChange(); err == nil {
		t.Fatal("expected undo of a record naming the workspace root to fail")
	}
	if _, err := os.Stat(filepath.Join(workspace, "keep.txt")); err != nil {
		t.Fatalf("expected workspace to survive a forged undo, got %v", err)
	}
}

func TestTransfersRefuseTheSameFileUnderAnotherName(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	os.MkdirAll(filepath.Join(workspace, "Src"), 0755)
	os.WriteFile(filepath.Join(workspace, "Src", "f.txt"), []byte("data"), 0644)
	os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("data"), 0644)

	if caseInsensitiveFilesystem(t, workspace) {
		if err := moveDir("Src", "src", true); err == nil {
			t.Error("expected a case-only move_dir with overwrite to fail")
		}
		if err := copyDir("Src", "src", true); err == nil {
			t.Error("expected a case-only copy_dir with overwrite to fail")
		}
		if err := moveFile("a.txt", "A.txt", true); err == nil {
			t.Error("expected a case-only move_file with overwrite to fail")
		}
	}

	for _, path := range []string{"Src/f.txt", "a.txt"} {
		if data, err := os.ReadFile(filepath.Join(workspace, path)); err != nil || string(data) != "data" {
			t.Fatalf("expected %s to be intact, got %q (%v)", path, data, err)
		}
	}
}

func TestCopyDirRefusesDestinationInsideSourceViaAlias(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	os.MkdirAll(filepath.Join(workspace, "src", "nested"), 0755)
	os.Symlink(filepath.Join(workspace, "src"), filepath.Join(workspace, "alias"))

	destinations := []string{"alias/nested/inner"}
	if caseInsensitiveFilesystem(t, workspace) {
		destinations = append(destinations, "SRC/nested/inner")
	}
	for _, destination := range destinations {
		if err := copyDir("src", destination, false); err == nil || !strings.Contains(err.Error(), "overlap") {
			t.Errorf("%s: expected overlap error, got %v", destination, err)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "src", "nested", "inner")); !os.IsNotExist(err) {
		t.Fatalf("expected nothing to be copied into the source, got %v", err)
	}
}

func TestFailedMutationRestoresPreviousState(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("original"), 0644)
	err := withRecordedMutation("write_file", []string{"a.txt"}, func() error {
		os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("half written"), 0644)
		return os.ErrInvalid
	})
	if err == nil {
		t.Fatal("expected the mutation error")
	}
	if data, _ := os.ReadFile(filepath.Join(workspace, "a.txt")); string(data) != "original" {
		t.Fatalf("expected the file to be restored, got %q", data)
	}
	entries, _ := os.ReadDir(filepath.Join(workspace, ".goat", "snapshots"))
	if len(entries) != 0 {
		t.Fatalf("expected the snapshot to be cleaned up after restoring, found %d", len(entries))
	}
}

func TestRunBashCommandFinishingBeforeDeadlineIsNotReportedAsTimeout(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	// Let the deadline pass after the shell exits but while run_bash still waits
	// bashPipeWaitDelay for the background process to release the output.
	start := time.Now()
	if _, err := runBashCommand(context.Background(), "true", "."); err != nil {
		t.Fatalf("measure shell startup: %v", err)
	}
	shellStartup := time.Since(start)

	previousTimeout := bashToolTimeout
	bashToolTimeout = shellStartup + bashPipeWaitDelay/2
	t.Cleanup(func() { bashToolTimeout = previousTimeout })

	output, err := runBashCommand(context.Background(), "echo done; sleep 20 & echo $! > bg.pid", ".")
	t.Cleanup(func() { killPIDFile(filepath.Join(workspace, "bg.pid")) })
	if err != nil {
		t.Fatalf("expected a clean exit to succeed, got %v", err)
	}
	if !strings.Contains(output, "done") {
		t.Fatalf("expected output, got %q", output)
	}
}

func TestRunBashCommandHasNoControllingTerminal(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	start := time.Now()
	_, err := runBashCommand(context.Background(), "read x < /dev/tty", ".")
	if err == nil {
		t.Fatal("expected reading /dev/tty to fail")
	}
	if strings.Contains(err.Error(), "timed out") || time.Since(start) > 10*time.Second {
		t.Fatalf("expected a prompt for /dev/tty to fail fast, got %v after %s", err, time.Since(start))
	}
}

func TestMoveFileAllowsDistinctHardLinks(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("data"), 0644)
	if err := os.Link(filepath.Join(workspace, "a.txt"), filepath.Join(workspace, "b.txt")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}

	if err := moveFile("a.txt", "b.txt", true); err != nil {
		t.Fatalf("expected moving between distinct hard links to work, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected a.txt to be gone, got %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(workspace, "b.txt")); err != nil || string(data) != "data" {
		t.Fatalf("expected b.txt to keep the data, got %q (%v)", data, err)
	}
}

func TestFailedMutationThatChangedNothingLeavesNoRecordAndKeepsFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission test needs a non-root user")
	}
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := writeFile("foo.go", "v2"); err != nil {
		t.Fatalf("seed recorded change: %v", err)
	}

	locked := filepath.Join(workspace, "vendor")
	os.Mkdir(locked, 0755)
	os.WriteFile(filepath.Join(locked, "x.go"), []byte("orig"), 0444)
	os.Link(filepath.Join(locked, "x.go"), filepath.Join(workspace, "x-link.go"))
	os.Chmod(locked, 0555)
	t.Cleanup(func() { os.Chmod(locked, 0755) })
	before, _ := os.Stat(filepath.Join(locked, "x.go"))

	if err := writeFile("vendor/x.go", "new"); err == nil {
		t.Fatal("expected the write to fail")
	}
	if err := deleteFile("vendor/x.go"); err == nil {
		t.Fatal("expected the delete to fail")
	}

	after, err := os.Stat(filepath.Join(locked, "x.go"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("expected the untouched file to keep its identity, got %v", err)
	}
	records, _ := loadOperationRecords(false)
	if len(records) != 1 {
		t.Fatalf("expected only the successful write to be recorded, got %d record(s)", len(records))
	}
	if _, err := undoLastChange(); err != nil {
		t.Fatalf("expected the earlier change to stay undoable, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "foo.go")); !os.IsNotExist(err) {
		t.Fatalf("expected undo to remove foo.go, got %v", err)
	}
}

func TestWriteFileRefusesDirectory(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	os.MkdirAll(filepath.Join(workspace, "src"), 0755)
	os.WriteFile(filepath.Join(workspace, "src", "a.go"), []byte("package a"), 0644)
	before, _ := os.Stat(filepath.Join(workspace, "src", "a.go"))

	if err := writeFile("src", "oops"); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("expected a directory error, got %v", err)
	}
	after, err := os.Stat(filepath.Join(workspace, "src", "a.go"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("expected the directory to be untouched, got %v", err)
	}
}

func TestCorruptOperationLogIsSetAside(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	os.MkdirAll(filepath.Join(workspace, ".goat"), 0755)
	os.WriteFile(filepath.Join(workspace, ".goat", "operations.json"), []byte(`[{"id":"1","ki`), 0644)

	if err := writeFile("a.txt", "hello"); err != nil {
		t.Fatalf("expected the write to succeed, got %v", err)
	}
	records, err := loadOperationRecords(false)
	if err != nil || len(records) != 1 {
		t.Fatalf("expected a fresh log with one record, got %d (%v)", len(records), err)
	}
	matches, _ := filepath.Glob(filepath.Join(workspace, ".goat", "operations.json.corrupt-*"))
	if len(matches) != 1 {
		t.Fatalf("expected the corrupt log to be kept aside, found %d", len(matches))
	}
	if _, err := undoLastChange(); err != nil {
		t.Fatalf("expected undo to work, got %v", err)
	}
}

func TestSnapshotsHandleReadOnlyDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission test needs a non-root user")
	}
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	readOnly := filepath.Join(workspace, "src", "ro")
	os.MkdirAll(readOnly, 0755)
	os.WriteFile(filepath.Join(readOnly, "gen.go"), []byte("package ro"), 0644)
	os.Chmod(readOnly, 0555)
	t.Cleanup(func() {
		os.Chmod(readOnly, 0755)
		filepath.Walk(filepath.Join(workspace, ".goat"), func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				os.Chmod(path, 0755)
			}
			return nil
		})
	})

	if err := copyDir("src", "copy", false); err != nil {
		t.Fatalf("copy_dir of a tree with a read-only directory: %v", err)
	}
	info, err := os.Stat(filepath.Join(workspace, "copy", "ro"))
	if err != nil || info.Mode().Perm() != 0555 {
		t.Fatalf("expected the copied directory to keep mode 0555, got %v (%v)", info.Mode().Perm(), err)
	}
	if _, err := undoLastChange(); err != nil {
		t.Fatalf("undo copy_dir: %v", err)
	}
}

func TestTreeDirMarksUnreadableDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission test needs a non-root user")
	}
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	os.WriteFile(filepath.Join(workspace, "a.txt"), nil, 0644)
	locked := filepath.Join(workspace, "locked")
	os.Mkdir(locked, 0)
	t.Cleanup(func() { os.Chmod(locked, 0755) })
	os.WriteFile(filepath.Join(workspace, "z.txt"), nil, 0644)

	lines, _, err := treeDir(".", 0)
	if err != nil {
		t.Fatalf("expected the tree to render, got %v", err)
	}
	tree := strings.Join(lines, "\n")
	if !strings.Contains(tree, "locked/ [unreadable]") || !strings.Contains(tree, "z.txt") {
		t.Fatalf("expected the unreadable directory to be marked and the rest listed, got\n%s", tree)
	}
}

func TestUndoKeepsAncestorDirectoryModes(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	modes := map[string]os.FileMode{"priv": 0700, "shared": 0775 | os.ModeSetgid, "a/b": 0750}
	for dir, mode := range modes {
		os.MkdirAll(filepath.Join(workspace, dir), 0755)
		os.WriteFile(filepath.Join(workspace, dir, "f.txt"), []byte("original"), 0644)
		if err := os.Chmod(filepath.Join(workspace, dir), mode); err != nil {
			t.Fatalf("chmod %s: %v", dir, err)
		}
	}

	for dir := range modes {
		if err := writeFile(dir+"/f.txt", "changed"); err != nil {
			t.Fatalf("write %s: %v", dir, err)
		}
		if _, err := undoLastChange(); err != nil {
			t.Fatalf("undo %s: %v", dir, err)
		}
	}

	for dir, mode := range modes {
		info, err := os.Stat(filepath.Join(workspace, dir))
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if got := dirMode(info.Mode()); got != mode {
			t.Errorf("%s: mode changed from %v to %v", dir, mode, got)
		}
	}
}

func TestFailedRemovalRestoresDirectoryModes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission test needs a non-root user")
	}
	if _, err := exec.LookPath("chflags"); err != nil {
		t.Skip("needs chflags to make a file undeletable")
	}
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	noSearch := filepath.Join(workspace, "d", "a")
	readOnly := filepath.Join(noSearch, "b")
	os.MkdirAll(readOnly, 0755)
	pinned := filepath.Join(readOnly, "f.txt")
	os.WriteFile(pinned, []byte("pinned"), 0644)
	if out, err := exec.Command("chflags", "uchg", pinned).CombinedOutput(); err != nil {
		t.Skipf("chflags uchg: %v %s", err, out)
	}
	os.Chmod(readOnly, 0500)
	os.Chmod(noSearch, 0600)
	t.Cleanup(func() {
		os.Chmod(noSearch, 0755)
		os.Chmod(readOnly, 0755)
		exec.Command("chflags", "nouchg", pinned).Run()
	})

	for attempt := 0; attempt < 5; attempt++ {
		if err := removeWorkspacePathIfExists("d"); err == nil {
			t.Fatal("expected deleting a tree with an immutable file to fail")
		}
	}
	os.Chmod(noSearch, 0700)
	defer os.Chmod(noSearch, 0600)
	for dir, want := range map[string]os.FileMode{"d": 0755, "d/a/b": 0500} {
		info, err := os.Stat(filepath.Join(workspace, dir))
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s: expected mode %v to be restored, got %v", dir, want, got)
		}
	}
}

func TestSnapshotsWithReadOnlyDirectoriesAreCleanedUp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission test needs a non-root user")
	}
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	sub := filepath.Join(workspace, "ro", "sub")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "f.txt"), []byte("x"), 0644)
	os.Chmod(sub, 0555)
	t.Cleanup(func() { os.Chmod(sub, 0755) })

	err := withRecordedMutation("delete_dir", []string{"ro"}, func() error { return os.ErrInvalid })
	if err == nil {
		t.Fatal("expected the mutation error")
	}
	entries, _ := os.ReadDir(filepath.Join(workspace, ".goat", "snapshots"))
	if len(entries) != 0 {
		t.Fatalf("expected the snapshot to be removed, found %d", len(entries))
	}
}

func TestUndoPreviewLeavesCorruptLogAndOrphansAreCollected(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := writeFile("a.txt", "1"); err != nil {
		t.Fatalf("write: %v", err)
	}
	logPath := filepath.Join(workspace, ".goat", "operations.json")
	os.WriteFile(logPath, []byte("{not json"), 0644)

	if _, err := previewUndoLastChange(); err == nil {
		t.Fatal("expected the preview to report the unreadable log")
	}
	if data, _ := os.ReadFile(logPath); string(data) != "{not json" {
		t.Fatalf("expected the preview to leave the log alone, got %q", data)
	}

	snapshots := filepath.Join(workspace, ".goat", "snapshots")
	orphans, _ := os.ReadDir(snapshots)
	old := time.Now().Add(-2 * unreferencedSnapshotGracePeriod)
	for _, orphan := range orphans {
		os.Chtimes(filepath.Join(snapshots, orphan.Name()), old, old)
	}
	inProgress := strconv.FormatInt(time.Now().UnixNano(), 10) // another process's mutation
	os.MkdirAll(filepath.Join(snapshots, inProgress, "before"), 0755)

	if err := writeFile("b.txt", "2"); err != nil {
		t.Fatalf("write: %v", err)
	}
	records, err := loadOperationRecords(false)
	if err != nil || len(records) != 1 {
		t.Fatalf("expected a fresh log with one record, got %d (%v)", len(records), err)
	}
	entries, _ := os.ReadDir(snapshots)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 2 || !strings.Contains(strings.Join(names, " "), records[0].ID) || !strings.Contains(strings.Join(names, " "), inProgress) {
		t.Fatalf("expected the referenced and the recent unrecorded snapshot to remain, got %v", names)
	}
}

func TestUndoThroughSymlinkedParentDirectory(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	os.MkdirAll(filepath.Join(workspace, "lib"), 0755)
	os.WriteFile(filepath.Join(workspace, "lib", "f.txt"), []byte("original"), 0644)
	os.Symlink(filepath.Join(workspace, "lib"), filepath.Join(workspace, "src"))

	if err := writeFile("src/f.txt", "changed"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := undoLastChange(); err != nil {
		t.Fatalf("undo through a symlinked directory: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(workspace, "lib", "f.txt")); err != nil || string(data) != "original" {
		t.Fatalf("expected the original content back, got %q (%v)", data, err)
	}
	if info, err := os.Lstat(filepath.Join(workspace, "src")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected src to still be a symlink, got %v", err)
	}
}

func TestUndoSkipsOperationWhoseSnapshotIsMissing(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := writeFile("a.txt", "1"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := writeFile("b.txt", "content"); err != nil {
		t.Fatalf("write: %v", err)
	}
	records, _ := loadOperationRecords(false)
	os.RemoveAll(filepath.Join(workspace, ".goat", "snapshots", records[1].ID))

	if _, err := undoLastChange(); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected a missing snapshot error, got %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(workspace, "b.txt")); err != nil || string(data) != "content" {
		t.Fatalf("expected b.txt to be left alone, got %q (%v)", data, err)
	}

	if _, err := undoLastChange(); err != nil {
		t.Fatalf("expected undo to move on to the earlier operation, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected the earlier write to be undone, got %v", err)
	}
}

func TestUndoCannotEscapeThroughSymlinkRetargetedByRestore(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "ws")
	os.MkdirAll(filepath.Join(workspace, "q"), 0755)
	os.WriteFile(filepath.Join(workspace, "q", "f.txt"), []byte("q"), 0644)
	os.MkdirAll(filepath.Join(workspace, "z", "l", "x"), 0755)
	os.WriteFile(filepath.Join(workspace, "z", "l", "x", "old.txt"), []byte("old"), 0644)
	chdirForTest(t, workspace)

	if err := moveDir("q", "z/l/x", true); err != nil {
		t.Fatalf("move_dir: %v", err)
	}

	// Rearrange so z/l/x resolves inside the workspace only while q is a symlink.
	os.MkdirAll(filepath.Join(workspace, "d1", "d2"), 0755)
	os.MkdirAll(filepath.Join(workspace, "OUT"), 0755)
	os.MkdirAll(filepath.Join(root, "OUT"), 0755)
	os.RemoveAll(filepath.Join(workspace, "z", "l"))
	os.Symlink("../q/../../OUT", filepath.Join(workspace, "z", "l"))
	os.RemoveAll(filepath.Join(workspace, "q"))
	os.Symlink("d1/d2", filepath.Join(workspace, "q"))

	undoLastChange()

	if _, err := os.Stat(filepath.Join(root, "OUT", "x")); !os.IsNotExist(err) {
		t.Fatalf("expected nothing to be written outside the workspace, got %v", err)
	}
}

func TestUndoRemovesParentDirectoriesItCreated(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	os.MkdirAll(filepath.Join(workspace, "real"), 0755)
	os.Symlink(filepath.Join(workspace, "real"), filepath.Join(workspace, "link"))
	os.MkdirAll(filepath.Join(workspace, "src", "sub"), 0755)
	os.WriteFile(filepath.Join(workspace, "src", "sub", "f.txt"), []byte("f"), 0644)

	steps := []struct {
		name    string
		mutate  func() error
		removed []string
		kept    []string
	}{
		{"write_file", func() error { return writeFile("link/n1/n2/x.txt", "X") }, []string{"real/n1"}, []string{"real"}},
		{"copy_file", func() error { return copyFile("src/sub/f.txt", "c1/c2/f.txt", false) }, []string{"c1"}, nil},
		{"move_dir", func() error { return moveDir("src/sub", "m/n/sub", false) }, []string{"m"}, []string{"src/sub/f.txt"}},
	}
	for _, step := range steps {
		if err := step.mutate(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if _, err := undoLastChange(); err != nil {
			t.Fatalf("%s: undo: %v", step.name, err)
		}
		for _, path := range step.removed {
			if _, err := os.Lstat(filepath.Join(workspace, path)); !os.IsNotExist(err) {
				t.Errorf("%s: expected %s to be removed, got %v", step.name, path, err)
			}
		}
		for _, path := range step.kept {
			if _, err := os.Lstat(filepath.Join(workspace, path)); err != nil {
				t.Errorf("%s: expected %s to remain, got %v", step.name, path, err)
			}
		}
	}

	// A parent that gained other content since the operation is left in place.
	if err := writeFile("p/q/a.txt", "a"); err != nil {
		t.Fatalf("write: %v", err)
	}
	os.WriteFile(filepath.Join(workspace, "p", "other.txt"), []byte("mine"), 0644)
	if _, err := undoLastChange(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "p", "other.txt")); err != nil {
		t.Fatalf("expected unrelated content to survive, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "p", "q")); !os.IsNotExist(err) {
		t.Fatalf("expected the empty created directory to be removed, got %v", err)
	}
}

func TestUndoRewritesFilesInPlace(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	os.MkdirAll(filepath.Join(workspace, "docs"), 0755)
	os.WriteFile(filepath.Join(workspace, "docs", "Readme.md"), []byte("hello"), 0644)
	os.Link(filepath.Join(workspace, "docs", "Readme.md"), filepath.Join(workspace, "readme-link.md"))

	path := "docs/Readme.md"
	if caseInsensitiveFilesystem(t, workspace) {
		path = "DOCS/README.md"
	}
	if _, err := editFile(path, "hello", "changed", false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if _, err := undoLastChange(); err != nil {
		t.Fatalf("undo: %v", err)
	}

	entries, _ := os.ReadDir(filepath.Join(workspace, "docs"))
	if len(entries) != 1 || entries[0].Name() != "Readme.md" {
		t.Fatalf("expected the file to keep its name, got %v", entries)
	}
	if data, _ := os.ReadFile(filepath.Join(workspace, "readme-link.md")); string(data) != "hello" {
		t.Fatalf("expected the hard link to see the restored content, got %q", data)
	}

	// A file that became read-only since the edit is still restored.
	if _, err := editFile("docs/Readme.md", "hello", "again", false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	os.Chmod(filepath.Join(workspace, "docs", "Readme.md"), 0444)
	if _, err := undoLastChange(); err != nil {
		t.Fatalf("undo of a read-only file: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(workspace, "docs", "Readme.md")); string(data) != "hello" {
		t.Fatalf("expected the content back, got %q", data)
	}
}

func TestUndoTreatsPathUnderAFileAsAbsent(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := writeFile("e.txt", "E"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := writeFile("d/f.txt", "F"); err != nil {
		t.Fatalf("write: %v", err)
	}
	os.RemoveAll(filepath.Join(workspace, "d"))
	os.WriteFile(filepath.Join(workspace, "d"), []byte("now a file"), 0644)

	if _, err := undoLastChange(); err != nil {
		t.Fatalf("expected undo of the vanished path to succeed, got %v", err)
	}
	if _, err := undoLastChange(); err != nil {
		t.Fatalf("expected the earlier operation to be undoable, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "e.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected e.txt to be undone, got %v", err)
	}
}

func TestUndoSkipGivesUpOnAnOperation(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := writeFile("a.txt", "1"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := writeFile("b.txt", "2"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := commandUndo(context.Background(), &config{}, []string{"skip"}); err != nil {
		t.Fatalf("undo skip: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "b.txt")); err != nil {
		t.Fatalf("expected skip to leave b.txt alone, got %v", err)
	}
	if _, err := undoLastChange(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected a.txt to be undone after the skip, got %v", err)
	}
	if err := commandUndo(context.Background(), &config{}, []string{"bogus"}); err == nil {
		t.Fatal("expected an unknown undo option to fail")
	}
}

func TestUndoPreviewIsAccurate(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	os.MkdirAll(filepath.Join(workspace, "src", "deep"), 0755)
	os.WriteFile(filepath.Join(workspace, "src", "deep", "a.txt"), []byte("bye\n"), 0644)
	if _, err := editFile("src/deep/a.txt", "bye", "hi", false); err != nil {
		t.Fatalf("edit: %v", err)
	}

	preview, err := previewUndoLastChange()
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	lines := strings.Split(preview, "\n")
	if strings.Contains("\n"+preview+"\n", "\n+ src/\n") || strings.Contains("\n"+preview+"\n", "\n+ src/deep/\n") || !strings.Contains(preview, "+bye") || len(lines) != 5 {
		t.Fatalf("expected only the file change in the preview, got\n%s", preview)
	}

	os.RemoveAll(filepath.Join(workspace, ".goat", "snapshots"))
	preview, err = previewUndoLastChange()
	if err != nil || !strings.Contains(preview, "missing") || strings.Contains(preview, "/dev/null") {
		t.Fatalf("expected the preview to report the missing snapshot, got %q (%v)", preview, err)
	}
}

func TestKeptSnapshotsAreNotCollected(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	snapshot := filepath.Join(workspace, ".goat", "snapshots", "123")
	os.MkdirAll(filepath.Join(snapshot, "before", "dst"), 0755)
	os.WriteFile(filepath.Join(snapshot, "before", "dst", "only-copy.txt"), []byte("precious"), 0644)
	location := keepOperationSnapshot(operationRecord{ID: "123"})
	old := time.Now().Add(-2 * unreferencedSnapshotGracePeriod)
	os.Chtimes(snapshot, old, old)

	if err := writeFile("a.txt", "1"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapshot, "before", "dst", "only-copy.txt")); err != nil {
		t.Fatalf("expected the kept snapshot to survive collection, got %v", err)
	}
	if !strings.Contains(location, ".goat/snapshots/123") {
		t.Fatalf("expected the snapshot location to be reported, got %q", location)
	}
}

func TestUndoNeverWritesIntoAnotherHardLinkedFile(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "ws")
	os.MkdirAll(workspace, 0755)
	store := filepath.Join(root, "store.js")
	os.WriteFile(store, []byte("STORE CONTENT"), 0644)
	if err := os.Link(store, filepath.Join(workspace, "a.js")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	os.WriteFile(filepath.Join(workspace, "b.js"), []byte("OLD B"), 0644)
	chdirForTest(t, workspace)

	if err := moveFile("a.js", "b.js", true); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, err := undoLastChange(); err != nil {
		t.Fatalf("undo: %v", err)
	}

	for path, want := range map[string]string{store: "STORE CONTENT", filepath.Join(workspace, "a.js"): "STORE CONTENT", filepath.Join(workspace, "b.js"): "OLD B"} {
		if data, err := os.ReadFile(path); err != nil || string(data) != want {
			t.Errorf("%s: expected %q, got %q (%v)", path, want, data, err)
		}
	}
}

func TestUndoLeavesFileAtCreatedParentPath(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := writeFile("d/f.txt", "F"); err != nil {
		t.Fatalf("write: %v", err)
	}
	os.RemoveAll(filepath.Join(workspace, "d"))
	os.WriteFile(filepath.Join(workspace, "d"), []byte("user file"), 0644)

	if _, err := previewUndoLastChange(); err != nil {
		t.Fatalf("expected the preview to treat the path as absent, got %v", err)
	}
	if _, err := undoLastChange(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(workspace, "d")); err != nil || string(data) != "user file" {
		t.Fatalf("expected the user's file to survive undo, got %q (%v)", data, err)
	}
}

func TestUndoWorksForRecordsFromOlderVersions(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("original"), 0644)
	if _, err := editFile("a.txt", "original", "edited", false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if err := writeFile("new/dir/b.txt", "b"); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Older versions wrote neither field (and wrote snapshot_path, which is ignored now).
	logPath := filepath.Join(workspace, ".goat", "operations.json")
	var raw []map[string]any
	data, _ := os.ReadFile(logPath)
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	for _, record := range raw {
		delete(record, "file_identities")
		delete(record, "created_parents")
		record["snapshot_path"] = "before"
	}
	data, _ = json.Marshal(raw)
	os.WriteFile(logPath, data, 0644)

	if _, err := undoLastChange(); err != nil {
		t.Fatalf("undo write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "new", "dir", "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected b.txt to be removed, got %v", err)
	}
	if _, err := undoLastChange(); err != nil {
		t.Fatalf("undo edit: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(workspace, "a.txt")); string(data) != "original" {
		t.Fatalf("expected the edit to be undone, got %q", data)
	}
}

func TestUndoDoesNotRemoveDirectoriesThroughANewSymlink(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := writeFile("a/b/c/f.txt", "f"); err != nil {
		t.Fatalf("write: %v", err)
	}
	os.MkdirAll(filepath.Join(workspace, "x", "c"), 0755)
	os.RemoveAll(filepath.Join(workspace, "a", "b"))
	os.Symlink("../x", filepath.Join(workspace, "a", "b"))

	if _, err := undoLastChange(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "x", "c")); err != nil {
		t.Fatalf("expected the user's directory behind the symlink to survive, got %v", err)
	}
}

func TestUndoReplacesAFileThatGainedHardLinks(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "ws")
	os.MkdirAll(workspace, 0755)
	os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("orig"), 0644)
	chdirForTest(t, workspace)

	if _, err := editFile("a.txt", "orig", "edited", false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	// Same file, but it now has a hard link outside the workspace.
	if err := os.Link(filepath.Join(workspace, "a.txt"), filepath.Join(root, "outside.txt")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}

	if _, err := undoLastChange(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(workspace, "a.txt")); string(data) != "orig" {
		t.Fatalf("expected a.txt to be restored, got %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "outside.txt")); string(data) != "edited" {
		t.Fatalf("expected the outside link to be left alone, got %q", data)
	}
}

func statFile(workspace, path string) (os.FileInfo, error) {
	return os.Stat(filepath.Join(workspace, path))
}

func mkdirAll(t *testing.T, workspace, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(workspace, path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}
