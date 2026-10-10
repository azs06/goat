package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func writeSearchFixture(t *testing.T) {
	t.Helper()
	files := map[string]string{
		".gitignore":      "build/\n*.log\n/vendor\n",
		"main.go":         "package main\nfunc Handler() {}\n",
		"util/strings.go": "package util\nfunc handlerName() {}\n",
		"build/out.go":    "func Handler() {}\n",
		"debug.log":       "Handler crashed\n",
		"vendor/lib/x.go": "func Handler() {}\n",
		"docs/notes.md":   "Handler docs\n",
	}
	for name, content := range files {
		if err := os.MkdirAll(dirOf(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func dirOf(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[:i]
	}
	return "."
}

func checkIgnoredSearch(t *testing.T) {
	t.Helper()
	matches, _, err := grepFilesWith(".", grepOptions{query: "handler"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(matches, "\n")
	for _, ignored := range []string{"build/", "debug.log", "vendor/"} {
		if strings.Contains(got, ignored) {
			t.Errorf("expected %s to be skipped:\n%s", ignored, got)
		}
	}
	if !strings.Contains(got, "main.go:2:") || !strings.Contains(got, "util/strings.go:2:") {
		t.Errorf("expected the tracked files:\n%s", got)
	}

	all, _, _ := grepFilesWith(".", grepOptions{query: "handler", includeIgnored: true})
	if !strings.Contains(strings.Join(all, "\n"), "build/out.go") {
		t.Error("expected include_ignored to search ignored files")
	}

	files, _, _ := findFilesWith(".", "*.go", 0, false)
	if strings.Contains(strings.Join(files, " "), "build") || len(files) != 2 {
		t.Errorf("find_files: %v", files)
	}
	tree, _, _ := treeDir(".", 0)
	if strings.Contains(strings.Join(tree, "\n"), "build/") {
		t.Errorf("tree_dir lists ignored dirs:\n%s", strings.Join(tree, "\n"))
	}
}

func TestSearchSkipsIgnoredFilesWithoutGit(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)
	t.Setenv("GIT_DIR", workspace+"/no-such-git-dir")
	writeSearchFixture(t)
	checkIgnoredSearch(t)
}

func TestSearchSkipsIgnoredFilesInGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	workspace := t.TempDir()
	chdirForTest(t, workspace)
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init failed: %s", out)
	}
	writeSearchFixture(t)
	checkIgnoredSearch(t)
}

func TestGrepRegexAndGlob(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)
	writeSearchFixture(t)

	matches, _, err := grepFilesWith(".", grepOptions{query: `func [A-Z]\w+\(`, regex: true, caseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || !strings.HasPrefix(matches[0], "main.go:2:") {
		t.Fatalf("regex matches: %v", matches)
	}
	if matches, _, _ := grepFilesWith(".", grepOptions{query: "handler", glob: "*.md"}); len(matches) != 1 || !strings.HasPrefix(matches[0], "docs/notes.md") {
		t.Fatalf("glob matches: %v", matches)
	}
	if _, _, err := grepFilesWith(".", grepOptions{query: "(", regex: true}); err == nil {
		t.Fatal("expected an invalid regex to fail")
	}
}

func TestReadFileLines(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)
	os.WriteFile("f.txt", []byte("1\n2\n3\n4\n5\n"), 0o644)

	text, first, last, total, err := readFileLines("f.txt", 2, 3)
	if err != nil || text != "2\n3\n4" || first != 2 || last != 4 || total != 5 {
		t.Fatalf("got %q %d-%d of %d (%v)", text, first, last, total, err)
	}
	if text, _, _, _, _ := readFileLines("f.txt", 4, 0); text != "4\n5" {
		t.Fatalf("to the end: %q", text)
	}
	if _, _, _, _, err := readFileLines("f.txt", 9, 1); err == nil {
		t.Fatal("expected an offset past the end to fail")
	}
	if _, _, _, _, err := readFileLines(".env", 1, 1); err == nil {
		t.Fatal("expected secret files to stay closed")
	}
}
