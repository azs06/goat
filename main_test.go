package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func chdirForTest(t *testing.T, dir string) {
	t.Helper()

	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working dir: %v", err)
	}

	if err := os.Chdir(dir); err != nil {
		t.Fatalf("change working dir: %v", err)
	}

	t.Cleanup(func() {
		if err := os.Chdir(previousDir); err != nil {
			t.Fatalf("restore working dir: %v", err)
		}
	})
}

func TestRunBashCommandUsesRelativeWorkdir(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	subdir := filepath.Join(workspace, "nested")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatalf("create subdir: %v", err)
	}

	output, err := runBashCommand(context.Background(), "pwd", "nested")
	if err != nil {
		t.Fatalf("run bash command: %v", err)
	}

	if !strings.Contains(output, filepath.Join(workspace, "nested")) {
		t.Fatalf("expected output to contain resolved workdir, got %q", output)
	}
}

func TestRunBashCommandRejectsEscapingWorkspace(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	_, err := runBashCommand(context.Background(), "pwd", "../outside")
	if err == nil {
		t.Fatal("expected escaping workdir to fail")
	}

	if !strings.Contains(err.Error(), "path escapes the workspace") {
		t.Fatalf("expected workspace escape error, got %v", err)
	}
}

func TestRunBashCommandIncludesFailureOutput(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	_, err := runBashCommand(context.Background(), "echo failure >&2; exit 7", ".")
	if err == nil {
		t.Fatal("expected bash command to fail")
	}

	if !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("expected exit status in error, got %v", err)
	}

	if !strings.Contains(err.Error(), "failure") {
		t.Fatalf("expected stderr in error, got %v", err)
	}
}

func TestEditFileReplacesSingleMatch(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("hello goat"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	replaced, err := editFile("sample.txt", "goat", "agent", false)
	if err != nil {
		t.Fatalf("edit file: %v", err)
	}

	if replaced != 1 {
		t.Fatalf("expected 1 replacement, got %d", replaced)
	}

	content, err := os.ReadFile(filepath.Join(workspace, "sample.txt"))
	if err != nil {
		t.Fatalf("read edited file: %v", err)
	}

	if string(content) != "hello agent" {
		t.Fatalf("unexpected content %q", string(content))
	}
}

func TestEditFileRejectsAmbiguousMatchWithoutReplaceAll(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("goat goat"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	_, err := editFile("sample.txt", "goat", "agent", false)
	if err == nil {
		t.Fatal("expected ambiguous edit to fail")
	}

	if !strings.Contains(err.Error(), "matched 2 times") {
		t.Fatalf("expected ambiguous match error, got %v", err)
	}
}

func TestEditFileReplaceAllReplacesEveryMatch(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("goat goat goat"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	replaced, err := editFile("sample.txt", "goat", "agent", true)
	if err != nil {
		t.Fatalf("replace all edit failed: %v", err)
	}

	if replaced != 3 {
		t.Fatalf("expected 3 replacements, got %d", replaced)
	}

	content, err := os.ReadFile(filepath.Join(workspace, "sample.txt"))
	if err != nil {
		t.Fatalf("read edited file: %v", err)
	}

	if string(content) != "agent agent agent" {
		t.Fatalf("unexpected content %q", string(content))
	}
}
