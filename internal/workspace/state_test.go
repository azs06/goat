package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateLivesOutsideTheWorkspace(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	resolved, _, err := ResolveStatePath(".goat/history.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(resolved, workspace) || !strings.HasPrefix(resolved, StateHome()) || filepath.Base(resolved) != "history.jsonl" {
		t.Fatalf("got %s", resolved)
	}
	if info, err := os.Stat(filepath.Dir(resolved)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("expected a private state directory, got %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, StateDirName)); err == nil {
		t.Fatal("expected nothing written in the workspace")
	}
	for _, bad := range []string{"history.jsonl", ".goat", ".goat/../x", "/etc/passwd"} {
		if _, _, err := ResolveStatePath(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
	// Different workspaces never share a state directory, even when their names
	// flatten to the same text.
	if stateDirKey("/a/b-c") == stateDirKey("/a/b/c") || len(stateDirKey("/"+strings.Repeat("x", 400))) > 200 {
		t.Fatal("state directory names collide or are too long")
	}
}

func TestStateHomeFollowsXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/srv/state")
	if home := StateHome(); home != "/srv/state/goat" {
		t.Fatalf("got %s", home)
	}
	t.Setenv("XDG_STATE_HOME", "relative/state")
	t.Setenv("HOME", "/home/someone")
	if home := StateHome(); home != "/home/someone/.local/state/goat" {
		t.Fatalf("a relative XDG_STATE_HOME should be ignored, got %s", home)
	}
}

func TestMigrateLegacyStateMovesDotGoatOut(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)

	// A change made by goat, then the state put where an older goat kept it.
	os.WriteFile("a.txt", []byte("original"), 0o644)
	if err := WriteFile("a.txt", "changed"); err != nil {
		t.Fatal(err)
	}
	root, _ := StateRoot()
	os.WriteFile(filepath.Join(root, "history.jsonl"), []byte("{}\n"), 0o600)
	os.WriteFile(filepath.Join(root, "operations.json.corrupt-1"), []byte("x"), 0o600)
	if err := os.Rename(root, filepath.Join(workspace, StateDirName)); err != nil {
		t.Fatal(err)
	}

	gotRoot, moved, left, err := MigrateLegacyState()
	if err != nil || gotRoot != root || len(left) != 0 || len(moved) < 4 {
		t.Fatalf("got %s %v %v %v", gotRoot, moved, left, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, StateDirName)); !os.IsNotExist(err) {
		t.Fatal("expected .goat to be removed")
	}
	// The operation log's snapshot paths still lead to the moved snapshot.
	if _, err := UndoLastChange(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile("a.txt"); string(data) != "original" {
		t.Fatalf("expected the change undone, got %q", data)
	}

	// Nothing to do the next time.
	if _, moved, _, err := MigrateLegacyState(); err != nil || len(moved) != 0 {
		t.Fatalf("got %v %v", moved, err)
	}
}

func TestMigrateLegacyStateKeepsWhatWouldClash(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	root, _ := EnsureStateRoot()
	os.WriteFile(filepath.Join(root, "session.json"), []byte("new"), 0o600)
	os.MkdirAll(StateDirName, 0o755)
	os.WriteFile(filepath.Join(StateDirName, "session.json"), []byte("old"), 0o600)
	os.WriteFile(filepath.Join(StateDirName, "prompts.jsonl"), []byte("\"hi\"\n"), 0o600)

	_, moved, left, err := MigrateLegacyState()
	if err != nil || len(moved) != 1 || moved[0] != "prompts.jsonl" || len(left) != 1 || left[0] != "session.json" {
		t.Fatalf("got %v %v %v", moved, left, err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "session.json")); string(data) != "new" {
		t.Fatal("expected the state directory's file to win")
	}
	if data, _ := os.ReadFile(filepath.Join(StateDirName, "session.json")); string(data) != "old" {
		t.Fatal("expected the clashing file left in .goat")
	}
}

func TestMigrateLegacyStateLeavesASymlinkAlone(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	elsewhere := t.TempDir()
	os.WriteFile(filepath.Join(elsewhere, "history.jsonl"), []byte("x"), 0o600)
	os.Symlink(elsewhere, StateDirName)
	if _, _, _, err := MigrateLegacyState(); err == nil {
		t.Fatal("expected a symlinked .goat to be refused")
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "history.jsonl")); err != nil {
		t.Fatal("expected the link's target untouched")
	}
}

func TestCopyStateEntryCopiesATree(t *testing.T) {
	from, to := t.TempDir(), filepath.Join(t.TempDir(), "copy")
	os.MkdirAll(filepath.Join(from, "snapshots", "1", "before"), 0o755)
	os.WriteFile(filepath.Join(from, "snapshots", "1", "before", "a.txt"), []byte("a"), 0o640)
	if err := copyStateEntry(from, to); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(to, "snapshots", "1", "before", "a.txt"))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("got %v %v", info, err)
	}
	os.Symlink("/etc", filepath.Join(from, "link"))
	if err := copyStateEntry(from, filepath.Join(t.TempDir(), "again")); err == nil {
		t.Fatal("expected a symlink to be refused")
	}
}
