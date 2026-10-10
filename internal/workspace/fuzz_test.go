package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzResolveToolPath checks that no path the model can send resolves outside the
// workspace, into .goat, or to a credentials file.
func FuzzResolveToolPath(f *testing.F) {
	for _, seed := range []string{"a.txt", "../x", ".goat/history.jsonl", "./.GOAT", ".env", "sub/../../etc", "a/./b/", ".env.example", "..\\x", "/abs"} {
		f.Add(seed)
	}
	f.Chdir(f.TempDir())
	root, err := os.Getwd()
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, path string) {
		resolved, rel, err := ResolvePath(path)
		if err != nil {
			return
		}
		if escapesRoot(rel) || isStatePath(rel) || isSecretFileName(filepath.Base(rel)) {
			t.Fatalf("%q resolved to %q", path, rel)
		}
		if !strings.HasPrefix(resolved, root) {
			t.Fatalf("%q resolved outside the workspace: %q", path, resolved)
		}
	})
}
