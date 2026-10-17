package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestSkill(t *testing.T, dir, name, frontmatter, body string) {
	t.Helper()
	dir = filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\n" + frontmatter + "---\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFirstMessageListsTheSkills(t *testing.T) {
	cfg := seamConfig(t, newFakeAnthropic(t).provider(true))
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	writeTestSkill(t, filepath.Join(config, "goat", "skills"), "deploy", "description: The user's deploy steps.\n", "")
	writeTestSkill(t, filepath.Join(".agents", "skills"), "deploy", "description: The project's, shadowed.\n", "")
	writeTestSkill(t, filepath.Join(".claude", "skills"), "review", "description: >\n  Review a change.\n  Use before a PR.\n", "")
	writeTestSkill(t, filepath.Join(".agents", "skills"), "manual", "description: Only by hand.\ndisable-model-invocation: true\n", "")

	text := projectInstructionsText()
	want := "<skills>\nSkills you can load with the skill tool when a task matches one:\n- deploy: The user's deploy steps.\n- review: Review a change. Use before a PR.\n</skills>"
	if !strings.Contains(text, want) {
		t.Fatalf("expected the skill list, got:\n%s", text)
	}

	out := captureStdout(t, func() {
		if err := handleInput(cfg, "/skills"); err != nil {
			t.Fatal(err)
		}
		if err := handleInput(cfg, "/status"); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"Skills (3)", "deploy", "~/.config/goat/skills", "only with /skill", "Not loaded:", "already in ~/.config/goat/skills", "Skills:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if items := commandArgumentCompletions(cfg)("skill", "re"); len(items) != 1 || items[0].text != "/skill review" {
		t.Errorf("completions %+v", items)
	}
}

func TestSkillCommandSendsTheSkill(t *testing.T) {
	fake := newFakeAnthropic(t, newSSE("msg_1").text("Done.").stop("end_turn", ""))
	cfg := seamConfig(t, fake.provider(true))
	sessionSaver = func(string, savedSession) error { return nil }
	t.Cleanup(func() { sessionSaver = writeSessionFile })
	writeTestSkill(t, filepath.Join(".agents", "skills"), "manual", "description: Only by hand.\ndisable-model-invocation: true\n", "Step one: say SKILL-BODY.")

	captureStdout(t, func() {
		if err := handleInput(cfg, "/skill manual tag v1.2\nwith notes"); err != nil {
			t.Fatal(err)
		}
	})
	sent := fake.request(0).raw
	for _, want := range []string{"Use the skill manual for this request.", "Its directory is .agents/skills/manual", "Step one: say SKILL-BODY.", `tag v1.2\nwith notes`} {
		if !strings.Contains(sent, want) {
			t.Errorf("request lacks %q:\n%s", want, sent)
		}
	}
	if err := handleInput(cfg, "/skill nosuch"); err == nil || !strings.Contains(err.Error(), "no skill named") {
		t.Errorf("expected an unknown skill to fail, got %v", err)
	}
	if err := handleInput(cfg, "/skill"); err == nil {
		t.Error("expected /skill without a name to fail")
	}
}

func TestSkillRequest(t *testing.T) {
	for text, want := range map[string]string{
		"/skill skill":                     "",
		"/skill skill the skill page":      "the skill page",
		"  /skill  x  line one\nline two ": "line one\nline two",
	} {
		if got := skillRequest(text); got != want {
			t.Errorf("skillRequest(%q) = %q, want %q", text, got, want)
		}
	}
}
