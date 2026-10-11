package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileToolsStayOutOfAProtectedDirectory(t *testing.T) {
	home := t.TempDir()
	t.Chdir(home)
	config := filepath.Join(home, ".config", "goat")
	os.MkdirAll(config, 0o700)
	os.WriteFile(filepath.Join(config, "auth.json"), []byte(`{"openai": {"key": "sk-SECRET"}}`), 0o600)
	os.WriteFile(filepath.Join(home, "notes.txt"), []byte("sk-not-a-secret\n"), 0o600)
	os.Symlink(config, filepath.Join(home, "alias"))
	ProtectDir(config)
	t.Cleanup(ResetProtectedDirs)

	for _, path := range []string{".config/goat/auth.json", ".config/goat", "alias/auth.json", ".config/goat/new.txt", "./.config//goat/auth.json"} {
		if _, err := ReadFileForModel(path); err == nil {
			t.Errorf("%s: expected a refusal", path)
		}
		if _, _, err := ResolvePath(path); err == nil || !strings.Contains(err.Error(), "goat's config directory") && !strings.Contains(err.Error(), "symlink") {
			t.Errorf("%s: got %v", path, err)
		}
	}
	matches, _, err := GrepFiles(".", GrepOptions{Query: "sk-", IncludeIgnored: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(matches, "\n"), "SECRET") || len(matches) != 1 {
		t.Fatalf("search should skip the config directory, got %q", matches)
	}
	found, _, err := FindFiles(".", "*.json", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("got %q", found)
	}
}
