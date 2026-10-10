package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func gitTestRepo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("init", "-q", "-b", "trunk")
	os.WriteFile("a.go", []byte("package a\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "first")
	os.WriteFile("a.go", []byte("package a\n\nvar X = 1\n"), 0o644)
	os.WriteFile("new.txt", []byte("hi\n"), 0o644)
}

func TestGitStateAndContext(t *testing.T) {
	gitTestRepo(t)
	state, ok := readGitState()
	if !ok || state.branch != "trunk" || len(state.changed) != 2 {
		t.Fatalf("got %+v %v", state, ok)
	}
	if got := state.footerText(); !strings.Contains(got, "trunk") || !strings.Contains(got, "2 changed") {
		t.Fatalf("footer %q", got)
	}
	context := gitContextText()
	if !strings.Contains(context, "branch trunk") || !strings.Contains(context, " M a.go") || !strings.Contains(context, "?? new.txt") {
		t.Fatalf("context %q", context)
	}
}

func TestDiffCommand(t *testing.T) {
	gitTestRepo(t)
	out := captureStdout(t, func() {
		if err := commandDiff(context.Background(), &config{}, nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "+var X = 1") || !strings.Contains(out, "2 changed files") {
		t.Fatalf("unexpected diff output:\n%s", out)
	}
}

func TestDiffOutsideGit(t *testing.T) {
	chdirForTest(t, t.TempDir())
	if _, ok := readGitState(); ok {
		t.Skip("the temporary directory is inside a git repository")
	}
	if err := commandDiff(context.Background(), &config{}, nil); err == nil {
		t.Fatal("expected an error outside git")
	}
	if gitContextText() != "" {
		t.Fatal("expected no git context outside git")
	}
}
