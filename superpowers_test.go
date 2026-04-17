package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileCreatesParentDirectories(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := writeFile("nested/deep/file.txt", "super powers"); err != nil {
		t.Fatalf("write file: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(workspace, "nested", "deep", "file.txt"))
	if err != nil {
		t.Fatalf("read created file: %v", err)
	}

	if string(content) != "super powers" {
		t.Fatalf("unexpected content %q", string(content))
	}
}

func TestPreviewWriteFileDoesNotModifyFile(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("old text\n"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	preview, err := previewWriteFile("sample.txt", "new text\n")
	if err != nil {
		t.Fatalf("preview write file: %v", err)
	}
	if !strings.Contains(preview, "--- sample.txt") || !strings.Contains(preview, "+new text") {
		t.Fatalf("expected diff preview, got %q", preview)
	}

	content, err := os.ReadFile(filepath.Join(workspace, "sample.txt"))
	if err != nil {
		t.Fatalf("read sample file: %v", err)
	}
	if string(content) != "old text\n" {
		t.Fatalf("expected preview not to modify file, got %q", string(content))
	}
}

func TestPreviewEditFileDoesNotModifyFile(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("hello goat\n"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	preview, err := previewEditFile("sample.txt", "goat", "agent", false)
	if err != nil {
		t.Fatalf("preview edit file: %v", err)
	}
	if !strings.Contains(preview, "Replacements: 1") || !strings.Contains(preview, "+hello agent") {
		t.Fatalf("expected edit preview, got %q", preview)
	}

	content, err := os.ReadFile(filepath.Join(workspace, "sample.txt"))
	if err != nil {
		t.Fatalf("read sample file: %v", err)
	}
	if string(content) != "hello goat\n" {
		t.Fatalf("expected preview not to modify file, got %q", string(content))
	}
}

func TestCopyFileCopiesContents(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.WriteFile(filepath.Join(workspace, "source.txt"), []byte("copy me"), 0644); err != nil {
		t.Fatalf("seed source file: %v", err)
	}

	if err := copyFile("source.txt", "nested/dest.txt", false); err != nil {
		t.Fatalf("copy file: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(workspace, "nested", "dest.txt"))
	if err != nil {
		t.Fatalf("read destination file: %v", err)
	}
	if string(content) != "copy me" {
		t.Fatalf("unexpected destination content %q", string(content))
	}
}

func TestCopyFileRejectsExistingDestinationWithoutOverwrite(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.WriteFile(filepath.Join(workspace, "source.txt"), []byte("copy me"), 0644); err != nil {
		t.Fatalf("seed source file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "dest.txt"), []byte("already here"), 0644); err != nil {
		t.Fatalf("seed destination file: %v", err)
	}

	err := copyFile("source.txt", "dest.txt", false)
	if err == nil {
		t.Fatal("expected copy without overwrite to fail")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already exists error, got %v", err)
	}
}

func TestPreviewCopyFileDoesNotCreateDestination(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.WriteFile(filepath.Join(workspace, "source.txt"), []byte("copy me\n"), 0644); err != nil {
		t.Fatalf("seed source file: %v", err)
	}

	preview, err := previewCopyFile("source.txt", "nested/dest.txt", false)
	if err != nil {
		t.Fatalf("preview copy file: %v", err)
	}
	if !strings.Contains(preview, "Preview for copy_file") || !strings.Contains(preview, "+copy me") {
		t.Fatalf("expected copy preview, got %q", preview)
	}

	if _, err := os.Stat(filepath.Join(workspace, "nested", "dest.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected preview not to create destination, got err=%v", err)
	}
}

func TestMoveFileMovesContents(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.WriteFile(filepath.Join(workspace, "source.txt"), []byte("move me"), 0644); err != nil {
		t.Fatalf("seed source file: %v", err)
	}

	if err := moveFile("source.txt", "nested/dest.txt", false); err != nil {
		t.Fatalf("move file: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workspace, "source.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected source file to be removed, got err=%v", err)
	}

	content, err := os.ReadFile(filepath.Join(workspace, "nested", "dest.txt"))
	if err != nil {
		t.Fatalf("read destination file: %v", err)
	}
	if string(content) != "move me" {
		t.Fatalf("unexpected destination content %q", string(content))
	}
}

func TestPreviewMoveFileDoesNotMoveSource(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.WriteFile(filepath.Join(workspace, "source.txt"), []byte("move me\n"), 0644); err != nil {
		t.Fatalf("seed source file: %v", err)
	}

	preview, err := previewMoveFile("source.txt", "nested/dest.txt", false)
	if err != nil {
		t.Fatalf("preview move file: %v", err)
	}
	if !strings.Contains(preview, "Source file source.txt would be removed after the move.") {
		t.Fatalf("expected move preview message, got %q", preview)
	}

	if _, err := os.Stat(filepath.Join(workspace, "source.txt")); err != nil {
		t.Fatalf("expected source to remain after preview, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "nested", "dest.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected preview not to create destination, got err=%v", err)
	}
}

func TestDeleteFileRemovesFile(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.WriteFile(filepath.Join(workspace, "trash.txt"), []byte("delete me"), 0644); err != nil {
		t.Fatalf("seed trash file: %v", err)
	}

	if err := deleteFile("trash.txt"); err != nil {
		t.Fatalf("delete file: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workspace, "trash.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected file to be deleted, got err=%v", err)
	}
}

func TestPreviewDeleteFileDoesNotDelete(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.WriteFile(filepath.Join(workspace, "trash.txt"), []byte("delete me\n"), 0644); err != nil {
		t.Fatalf("seed trash file: %v", err)
	}

	preview, err := previewDeleteFile("trash.txt")
	if err != nil {
		t.Fatalf("preview delete file: %v", err)
	}
	if !strings.Contains(preview, "+++ /dev/null") || !strings.Contains(preview, "-delete me") {
		t.Fatalf("expected delete preview, got %q", preview)
	}

	if _, err := os.Stat(filepath.Join(workspace, "trash.txt")); err != nil {
		t.Fatalf("expected file to remain after preview, got %v", err)
	}
}

func TestTreeDirReturnsRecursiveTree(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.MkdirAll(filepath.Join(workspace, "pkg", "nested"), 0755); err != nil {
		t.Fatalf("create directories: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main"), 0644); err != nil {
		t.Fatalf("seed main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "pkg", "nested", "worker.go"), []byte("package nested"), 0644); err != nil {
		t.Fatalf("seed worker.go: %v", err)
	}

	lines, truncated, err := treeDir(".", 20)
	if err != nil {
		t.Fatalf("tree dir: %v", err)
	}
	if truncated {
		t.Fatal("did not expect tree output to be truncated")
	}

	tree := strings.Join(lines, "\n")
	if !strings.Contains(tree, "./") || !strings.Contains(tree, "main.go") || !strings.Contains(tree, "pkg/") || !strings.Contains(tree, "worker.go") {
		t.Fatalf("unexpected tree output %q", tree)
	}
}

func TestTreeDirSkipsInternalDirectories(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0755); err != nil {
		t.Fatalf("create .git: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".goat"), 0755); err != nil {
		t.Fatalf("create .goat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git", "hidden.txt"), []byte("hidden"), 0644); err != nil {
		t.Fatalf("seed .git file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".goat", "history.jsonl"), []byte("hidden"), 0644); err != nil {
		t.Fatalf("seed .goat file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "visible.txt"), []byte("visible"), 0644); err != nil {
		t.Fatalf("seed visible file: %v", err)
	}

	lines, _, err := treeDir(".", 20)
	if err != nil {
		t.Fatalf("tree dir: %v", err)
	}

	tree := strings.Join(lines, "\n")
	if strings.Contains(tree, ".git") || strings.Contains(tree, ".goat") {
		t.Fatalf("expected internal directories to be skipped, got %q", tree)
	}
	if !strings.Contains(tree, "visible.txt") {
		t.Fatalf("expected visible file to appear, got %q", tree)
	}
}

func TestFindFilesReturnsRelativeMatches(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.MkdirAll(filepath.Join(workspace, "pkg", "nested"), 0755); err != nil {
		t.Fatalf("create directories: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main"), 0644); err != nil {
		t.Fatalf("seed main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "pkg", "nested", "worker.go"), []byte("package nested"), 0644); err != nil {
		t.Fatalf("seed worker.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("hello"), 0644); err != nil {
		t.Fatalf("seed readme: %v", err)
	}

	matches, truncated, err := findFiles(".", "*.go", 10)
	if err != nil {
		t.Fatalf("find files: %v", err)
	}
	if truncated {
		t.Fatal("did not expect results to be truncated")
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d (%v)", len(matches), matches)
	}
	if matches[0] != "main.go" {
		t.Fatalf("expected first match to be main.go, got %q", matches[0])
	}
	if matches[1] != "pkg/nested/worker.go" {
		t.Fatalf("expected nested match, got %q", matches[1])
	}
}

func TestFindFilesAndGrepFilesSkipInternalDirectories(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0755); err != nil {
		t.Fatalf("create .git: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".goat"), 0755); err != nil {
		t.Fatalf("create .goat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git", "hidden.txt"), []byte("secret needle\n"), 0644); err != nil {
		t.Fatalf("seed .git file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".goat", "hidden.txt"), []byte("secret needle\n"), 0644); err != nil {
		t.Fatalf("seed .goat file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "visible.txt"), []byte("public needle\n"), 0644); err != nil {
		t.Fatalf("seed visible file: %v", err)
	}

	files, _, err := findFiles(".", "*.txt", 10)
	if err != nil {
		t.Fatalf("find files: %v", err)
	}
	if len(files) != 1 || files[0] != "visible.txt" {
		t.Fatalf("expected only visible file, got %v", files)
	}

	matches, _, err := grepFiles(".", "needle", false, 10)
	if err != nil {
		t.Fatalf("grep files: %v", err)
	}
	if len(matches) != 1 || !strings.Contains(matches[0], "visible.txt:1:public needle") {
		t.Fatalf("expected only visible grep match, got %v", matches)
	}
}

func TestGrepFilesFindsMatchesRecursively(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.MkdirAll(filepath.Join(workspace, "pkg"), 0755); err != nil {
		t.Fatalf("create directories: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n// magic needle\n"), 0644); err != nil {
		t.Fatalf("seed main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "pkg", "worker.go"), []byte("package pkg\nconst note = \"Needle inside\"\n"), 0644); err != nil {
		t.Fatalf("seed worker.go: %v", err)
	}

	matches, truncated, err := grepFiles(".", "needle", false, 10)
	if err != nil {
		t.Fatalf("grep files: %v", err)
	}
	if truncated {
		t.Fatal("did not expect grep results to be truncated")
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d (%v)", len(matches), matches)
	}
	if !strings.Contains(matches[0], "main.go:2:// magic needle") {
		t.Fatalf("expected main.go match, got %q", matches[0])
	}
	if !strings.Contains(matches[1], "pkg/worker.go:2:const note = \"Needle inside\"") {
		t.Fatalf("expected worker.go match, got %q", matches[1])
	}
}
