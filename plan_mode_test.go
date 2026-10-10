package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
)

func TestPlanModeTellsTheModel(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	fake := newFakeAnthropic(t, newSSE("msg_1").text("plan").stop("end_turn", ""))
	if _, err := sendPromptStream(context.Background(), fake.provider(true), "refactor it", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortLow, planMode: true}); err != nil {
		t.Fatal(err)
	}
	text := messagesOf(t, fake.request(0))[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(text, "[Plan mode is on") || !strings.HasSuffix(text, "refactor it") {
		t.Fatalf("got %q", text)
	}
}

func TestPlanModeNoteSurvivesProjectInstructions(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	os.WriteFile("AGENTS.md", []byte("Use tabs."), 0o644)
	previous := projectInstructions
	projectInstructions = projectInstructionsText
	t.Cleanup(func() { projectInstructions = previous })

	fake := newFakeAnthropic(t, newSSE("msg_1").text("plan").stop("end_turn", ""))
	if _, err := sendPromptStream(context.Background(), fake.provider(true), "refactor it", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortLow, planMode: true}); err != nil {
		t.Fatal(err)
	}
	text := messagesOf(t, fake.request(0))[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "Use tabs.") || !strings.Contains(text, "[Plan mode is on") || !strings.HasSuffix(text, "refactor it") {
		t.Fatalf("expected the instructions and the plan note, got %q", text)
	}
}
