package skills

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseFrontmatter(t *testing.T) {
	body, fields, err := parse("\ufeff---\r\n" +
		"name: pdf-tools # a comment\r\n" +
		"description: >\r\n  Extract text from PDFs.\r\n  Use when reading PDFs.\r\n" +
		"license: \"Apache-2.0\"\r\n" +
		"quoted: 'it''s'\r\n" +
		"plain: first\r\n  continued\r\n" +
		"literal: |\r\n  one\r\n    two\r\n" +
		"metadata:\r\n  author: someone\r\n" +
		"disable-model-invocation: true\r\n" +
		"---\r\n\r\n# PDF tools\r\nDo things.\r\n")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"name":                     "pdf-tools",
		"description":              "Extract text from PDFs. Use when reading PDFs.",
		"license":                  "Apache-2.0",
		"quoted":                   "it's",
		"plain":                    "first continued",
		"literal":                  "one\n  two",
		"metadata":                 "",
		"disable-model-invocation": "true",
	}
	for key, value := range want {
		if fields[key] != value {
			t.Errorf("%s = %q, want %q", key, fields[key], value)
		}
	}
	if body != "# PDF tools\nDo things." {
		t.Errorf("body %q", body)
	}
	for _, bad := range []string{"no frontmatter", "---\nname: x\n"} {
		if _, _, err := parse(bad); err == nil {
			t.Errorf("expected %q to be refused", bad)
		}
	}
}

func TestDiscover(t *testing.T) {
	user, project := t.TempDir(), t.TempDir()
	writeSkill(t, filepath.Join(user, "shared"), "---\nname: shared\ndescription: The user's own.\n---\nUser body.")
	writeSkill(t, filepath.Join(user, "group", "deep"), "---\nname: deep\ndescription: Found below a group directory.\n---\n")
	writeSkill(t, filepath.Join(project, "shared"), "---\nname: shared\ndescription: The project's.\n---\n")
	writeSkill(t, filepath.Join(project, "manual"), "---\nname: manual\ndescription: Only by hand.\ndisable-model-invocation: true\n---\n")
	writeSkill(t, filepath.Join(project, "Bad_Name"), "---\nname: Bad_Name\ndescription: x\n---\n")
	writeSkill(t, filepath.Join(project, "nodesc"), "---\nname: nodesc\n---\n")
	writeSkill(t, filepath.Join(project, "node_modules", "hidden"), "---\nname: hidden\ndescription: skipped\n---\n")

	found, problems := Discover([]Root{{Dir: user, Label: "user"}, {Dir: project, Label: "project"}})
	var names []string
	for _, skill := range found {
		names = append(names, skill.Name+"@"+skill.Source)
	}
	if got := strings.Join(names, " "); got != "deep@user manual@project shared@user" {
		t.Fatalf("found %s", got)
	}
	if found[1].ModelInvocable || !found[2].ModelInvocable {
		t.Errorf("expected only manual to be hidden from the model: %+v", found)
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"already in user", "Bad_Name", "no description"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems lack %q:\n%s", want, joined)
		}
	}
	if body, err := found[2].Instructions(); err != nil || body != "User body." {
		t.Errorf("instructions %q, %v", body, err)
	}
}

func TestReadFileStaysInTheSkill(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "tool")
	writeSkill(t, dir, "---\nname: tool\ndescription: x\n---\n")
	os.MkdirAll(filepath.Join(dir, "references"), 0o755)
	os.WriteFile(filepath.Join(dir, "references", "guide.md"), []byte("guide"), 0o644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0o644)
	os.WriteFile(filepath.Join(root, "outside.txt"), []byte("outside"), 0o644)
	skill := Skill{Name: "tool", Dir: dir}
	secret := func(name string) bool { return name == ".env" }

	if got, err := skill.ReadFile("references/guide.md", secret); err != nil || got != "guide" {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, err := skill.ReadFile(".", secret); err != nil || !strings.Contains(got, "references/") || !strings.Contains(got, FileName) {
		t.Fatalf("listing %q, %v", got, err)
	}
	for _, bad := range []string{"../outside.txt", filepath.Join(root, "outside.txt"), ".env", "missing.md", ""} {
		if _, err := skill.ReadFile(bad, secret); err == nil {
			t.Errorf("expected %q to be refused", bad)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(root, "outside.txt"), filepath.Join(dir, "link.txt")); err != nil {
			t.Fatal(err)
		}
		if _, err := skill.ReadFile("link.txt", secret); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Errorf("expected a symlink out of the skill to be refused, got %v", err)
		}
	}
}
