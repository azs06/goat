package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/skills"
)

func callSkill(t *testing.T, opts Options, args map[string]string) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return Execute(context.Background(), provider.ToolCall{Name: Skill, Arguments: string(raw)}, opts)
}

// A skill the agent writes with write_file can be loaded in the same conversation, and
// undo_last_change takes it away again.
func TestSkillWrittenByTheAgentLoadsAtOnce(t *testing.T) {
	t.Chdir(t.TempDir())
	skills.UserConfigDir = nil

	if _, err := callSkill(t, Options{}, map[string]string{"name": "release-notes"}); err == nil || !strings.Contains(err.Error(), "there are no skills") {
		t.Fatalf("expected no skills yet, got %v", err)
	}
	write := func(path, content string) {
		raw, _ := json.Marshal(map[string]string{"path": path, "content": content})
		if _, err := Execute(context.Background(), provider.ToolCall{Name: "write_file", Arguments: string(raw)}, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	write(".agents/skills/release-notes/references/style.md", "Short lines.")
	write(".agents/skills/release-notes/SKILL.md", "---\nname: release-notes\ndescription: Write release notes. Use when tagging a release.\n---\n\nRead references/style.md first.")

	// Plan mode only reads, and loading a skill is reading.
	out, err := callSkill(t, Options{PlanMode: true}, map[string]string{"name": "release-notes"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Skill release-notes, from .agents/skills.", "Its directory is .agents/skills/release-notes,", "\n\nRead references/style.md first."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "outside the workspace") {
		t.Errorf("expected a workspace skill to be read with the file tools:\n%s", out)
	}
	if out, err := callSkill(t, Options{}, map[string]string{"name": "release-notes", "file": "references/style.md"}); err != nil || out != "File references/style.md of skill release-notes:\nShort lines." {
		t.Fatalf("got %q, %v", out, err)
	}
	if _, err := callSkill(t, Options{}, map[string]string{"name": "release-notes", "file": "../../../go.mod"}); err == nil {
		t.Fatal("expected a path out of the skill to be refused")
	}

	if _, err := Execute(context.Background(), provider.ToolCall{Name: "undo_last_change"}, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := callSkill(t, Options{}, map[string]string{"name": "release-notes"}); err == nil {
		t.Fatal("expected the undone skill to be gone")
	}
}

func TestSkillOutsideTheWorkspaceSaysHowToReachIt(t *testing.T) {
	t.Chdir(t.TempDir())
	config := t.TempDir()
	skills.UserConfigDir = func() string { return config }
	t.Cleanup(func() { skills.UserConfigDir = nil })
	writeFile(t, filepath.Join(config, "skills", "mine", "SKILL.md"), "---\nname: mine\ndescription: The user's.\n---\nRun scripts/go.sh.")

	out, err := callSkill(t, Options{}, map[string]string{"name": "mine"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "from ~/.config/goat/skills") || !strings.Contains(out, filepath.Join(config, "skills", "mine")) || !strings.Contains(out, "run its scripts with run_bash by their absolute path") {
		t.Fatalf("output:\n%s", out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
