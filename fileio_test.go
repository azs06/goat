package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "script.sh")
	os.WriteFile(path, []byte("old"), 0o755)
	os.Chmod(path, 0o755)

	if err := writeFileAtomic(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	data, _ := os.ReadFile(path)
	if string(data) != "new" || info.Mode().Perm() != 0o755 {
		t.Fatalf("got %q with mode %v", data, info.Mode().Perm())
	}

	link := filepath.Join(dir, "link.sh")
	os.Symlink("script.sh", link)
	if err := writeFileAtomic(link, []byte("through the link"), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Lstat(link); info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("expected the symlink to stay a symlink")
	}
	if data, _ := os.ReadFile(path); string(data) != "through the link" {
		t.Fatalf("expected the target to change, got %q", data)
	}

	hard := filepath.Join(dir, "hard.sh")
	os.Link(path, hard)
	if err := writeFileAtomic(path, []byte("shared"), 0o644); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(hard); string(data) != "shared" {
		t.Fatalf("expected the hard link to see the change, got %q", data)
	}

	fresh := filepath.Join(dir, "fresh.txt")
	if err := writeFileAtomic(fresh, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(fresh); info.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode %v", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".sh" && entry.Name() != "fresh.txt" {
			t.Errorf("leftover temporary file %s", entry.Name())
		}
	}
}
