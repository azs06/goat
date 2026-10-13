package workspace

import (
	"os"
	"path/filepath"
	"runtime"
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
	if info, err := os.Stat(filepath.Dir(resolved)); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
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
	state, home := t.TempDir(), t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if got := StateHome(); got != filepath.Join(state, "goat") {
		t.Fatalf("got %s", got)
	}
	t.Setenv("XDG_STATE_HOME", "relative/state")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got := StateHome(); got != filepath.Join(home, ".local", "state", "goat") {
		t.Fatalf("a relative XDG_STATE_HOME should be ignored, got %s", got)
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

	result, err := MigrateLegacyState()
	if err != nil || result.Root != root || len(result.Clashed) != 0 || len(result.Failed) != 0 || len(result.Moved) < 4 {
		t.Fatalf("got %+v %v", result, err)
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
	if result, err := MigrateLegacyState(); err != nil || len(result.Moved) != 0 {
		t.Fatalf("got %+v %v", result, err)
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

	result, err := MigrateLegacyState()
	if err != nil || len(result.Moved) != 1 || result.Moved[0] != "prompts.jsonl" || len(result.Clashed) != 1 || result.Clashed[0] != "session.json" {
		t.Fatalf("got %+v %v", result, err)
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
	if _, err := MigrateLegacyState(); err == nil {
		t.Fatal("expected a symlinked .goat to be refused")
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "history.jsonl")); err != nil {
		t.Fatal("expected the link's target untouched")
	}
}

func TestCopyStateEntryCopiesATree(t *testing.T) {
	from, to := t.TempDir(), filepath.Join(t.TempDir(), "copy")
	os.MkdirAll(filepath.Join(from, "snapshots", "1", "before", "locked"), 0o755)
	os.WriteFile(filepath.Join(from, "snapshots", "1", "before", "a.txt"), []byte("a"), 0o600)
	os.Chmod(filepath.Join(from, "snapshots", "1", "before", "a.txt"), 0o664)
	os.Chmod(filepath.Join(from, "snapshots", "1", "before", "locked"), 0o555)
	t.Cleanup(func() { os.Chmod(filepath.Join(from, "snapshots", "1", "before", "locked"), 0o755) })
	if err := copyStateEntry(from, to); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeCopy(to) })
	// Undo restores the modes snapshots recorded, so a copy keeps them exactly.
	info, err := os.Stat(filepath.Join(to, "snapshots", "1", "before", "a.txt"))
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o664 {
		t.Fatalf("got %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(to, "snapshots", "1", "before", "locked")); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o555 {
		t.Fatalf("got %v %v", info, err)
	}
	os.Symlink("/etc", filepath.Join(from, "link"))
	if err := copyStateEntry(from, filepath.Join(t.TempDir(), "again")); err == nil {
		t.Fatal("expected a symlink to be refused")
	}
}

func TestStateDirectoryIsTheSameHoweverTheWorkspaceIsReached(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(real)
	viaReal, _ := StateRoot()
	t.Chdir(link)
	t.Setenv("PWD", link)
	viaLink, _ := StateRoot()
	if viaReal != viaLink {
		t.Fatalf("one workspace, two state directories:\n%s\n%s", viaReal, viaLink)
	}
}

func TestMoveStateEntryNeverRemovesWhatItDidNotMake(t *testing.T) {
	root, legacy := t.TempDir(), t.TempDir()
	// Another goat moved the entry first: this one finds the source gone and leaves
	// what is at the destination alone.
	to := filepath.Join(root, "snapshots")
	os.MkdirAll(filepath.Join(to, "1"), 0o700)
	if moved, err := moveStateEntry(filepath.Join(legacy, "snapshots"), filepath.Join(root, "other")); moved || err != nil {
		t.Fatalf("got %v %v", moved, err)
	}
	if _, err := moveStateEntry(filepath.Join(legacy, "missing"), to); err != errStateClash {
		t.Fatalf("expected a clash, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(to, "1")); err != nil {
		t.Fatal("the destination another goat made must survive")
	}
}

func TestProtectedDirectoriesStayOutOfTreesAndDirectoryChanges(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	t.Cleanup(ResetProtectedDirs)
	state := filepath.Join(workspace, ".local", "state", "goat")
	os.MkdirAll(filepath.Join(state, "projects", "-Users-someone-secret-1234"), 0o700)
	os.WriteFile(filepath.Join(state, "projects", "-Users-someone-secret-1234", "history.jsonl"), []byte("secret"), 0o600)
	ProtectDir(state, "goat's state directory, with the history and undo snapshots")

	entries, _, err := TreeDir(".", 200)
	if err != nil {
		t.Fatal(err)
	}
	if tree := strings.Join(entries, "\n"); strings.Contains(tree, "secret") || strings.Contains(tree, "projects") {
		t.Fatalf("the tree shows the state directory:\n%s", tree)
	}
	if _, err := ReadFileForModel(".local/state/goat/projects/-Users-someone-secret-1234/history.jsonl"); err == nil || !strings.Contains(err.Error(), "state directory") {
		t.Fatalf("got %v", err)
	}
	for name, run := range map[string]func() error{
		"copy_dir":   func() error { return CopyDir(".local", "leak", false) },
		"move_dir":   func() error { return MoveDir(".local", "moved", false) },
		"delete_dir": func() error { return DeleteDir(".local/state") },
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "state directory") {
			t.Errorf("%s: expected a refusal, got %v", name, err)
		}
	}
	if _, err := os.Stat("leak"); err == nil {
		t.Fatal("expected nothing copied")
	}
}
