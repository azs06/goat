package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
)

func TestProjectInstructionsGoWithTheFirstMessageOnly(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	os.MkdirAll(filepath.Join(config, "goat"), 0o755)
	os.WriteFile(filepath.Join(config, "goat", "AGENTS.md"), []byte("Answer tersely."), 0o644)
	os.WriteFile("AGENTS.md", []byte("Run go test before finishing."), 0o644)
	os.WriteFile("CLAUDE.md", []byte("Should not be used when AGENTS.md exists."), 0o644)

	if got := strings.Join(projectInstructionSources(), ","); got != "~/.config/goat/AGENTS.md,AGENTS.md" {
		t.Fatalf("sources %q", got)
	}

	previous := projectInstructions
	projectInstructions = projectInstructionsText
	t.Cleanup(func() { projectInstructions = previous })

	fake := newFakeAnthropic(t,
		newSSE("msg_1").text("ok").stop("end_turn", ""),
		newSSE("msg_2").text("ok").stop("end_turn", ""),
	)
	p := fake.provider(true)
	conv, err := sendPromptStream(context.Background(), p, "hello", conversationState{}, "claude-opus-5-5", provider.EffortLow)
	if err != nil {
		t.Fatal(err)
	}
	first := messagesOf(t, fake.request(0))[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	for _, want := range []string{"Answer tersely.", "Run go test before finishing.", "hello"} {
		if !strings.Contains(first, want) {
			t.Errorf("first message lacks %q:\n%s", want, first)
		}
	}
	if strings.Contains(first, "Should not be used") {
		t.Error("expected CLAUDE.md to be skipped when AGENTS.md exists")
	}
	if _, err := sendPromptStream(context.Background(), p, "again", conv, "claude-opus-5-5", provider.EffortLow); err != nil {
		t.Fatal(err)
	}
	second := messagesOf(t, fake.request(1))
	last := second[len(second)-1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if last != "again" {
		t.Errorf("expected later messages without the instructions, got %q", last)
	}

	entries, _ := loadConversationHistoryEntries()
	if entries[0].Content != "hello" {
		t.Errorf("expected the history to keep the prompt as typed, got %q", entries[0].Content)
	}
}
