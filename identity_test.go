package main

import (
	"context"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
)

func TestSystemPromptSaysTheAgentIsGoat(t *testing.T) {
	for name, prompt := range map[string]string{"default": systemPrompt, "code mode": codeModeSystemPrompt} {
		if !strings.HasPrefix(prompt, "You are goat,") {
			t.Errorf("%s: expected the prompt to open with goat's name, got %q", name, prompt[:min(60, len(prompt))])
		}
		for _, want := range []string{"say you are goat", "Never call yourself ChatGPT, Claude, DeepSeek", "name that model", "rather than guessing"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s: prompt lacks %q", name, want)
			}
		}
	}
}

func TestEachPromptNamesTheModelInUse(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	fake := newFakeAnthropic(t,
		newSSE("msg_1").text("goat").stop("end_turn", ""),
		newSSE("msg_2").text("goat").stop("end_turn", ""),
	)
	p := fake.provider(true)
	conv, err := sendPromptStream(context.Background(), p, "who are you?", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortLow})
	if err != nil {
		t.Fatal(err)
	}
	// After a /model switch the next prompt names the new model.
	if _, err := sendPromptStream(context.Background(), p, "and now?", conv, turnOptions{model: "claude-sonnet-5-5", effort: provider.EffortLow, planMode: true}); err != nil {
		t.Fatal(err)
	}

	first := messagesOf(t, fake.request(0))[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if first != "[goat is using the model claude-opus-5-5 via anthropic.]\n\nwho are you?" {
		t.Errorf("first prompt %q", first)
	}
	second := messagesOf(t, fake.request(1))
	last := second[len(second)-1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(last, "[Plan mode is on") || !strings.HasSuffix(last, "[goat is using the model claude-sonnet-5-5 via anthropic.]\n\nand now?") {
		t.Errorf("second prompt %q", last)
	}

	// The history keeps what the user typed.
	entries, _ := loadConversationHistoryEntries()
	for _, entry := range entries {
		if entry.Type == "user" && strings.Contains(entry.Content, "goat is using") {
			t.Errorf("the note leaked into the history: %q", entry.Content)
		}
	}
	if modelNote("", "anthropic") != "" {
		t.Error("expected no note without a model")
	}
}

func TestModelNoteComesBeforeShellOutput(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	fake := newFakeAnthropic(t, newSSE("msg_1").text("ok").stop("end_turn", ""))
	if _, err := sendPromptStream(context.Background(), fake.provider(true), "what failed?", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortLow, contextNote: "[shell output]\n\n"}); err != nil {
		t.Fatal(err)
	}
	text := messagesOf(t, fake.request(0))[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if text != "[goat is using the model claude-opus-5-5 via anthropic.]\n\n[shell output]\n\nwhat failed?" {
		t.Errorf("prompt %q", text)
	}
}
