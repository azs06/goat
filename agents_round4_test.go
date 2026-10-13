package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/subagent"
	"github.com/azs06/goat/internal/tools"
)

// Text a failed request carried (steered lines, agents' reports already marked
// delivered) goes on through every compaction and carried-over model switch; only
// /clear drops it.
func TestPendingTextSurvivesCompactionAndModelSwitch(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{})
	cfg, _ := agentTestConfig(t, fake, subagent.Config{}, true)
	for _, step := range []struct {
		name string
		run  func()
	}{
		{"auto compaction", func() {
			applyCompactedConversation(cfg, provider.Conversation{Provider: provider.Anthropic}, "summary", "auto")
		}},
		{"/compact", func() {
			applyCompactedConversation(cfg, provider.Conversation{Provider: provider.Anthropic}, "summary", "manual")
		}},
		{"/model carry-over", func() {
			carryOverToModel(cfg, cfg.model, provider.Conversation{Provider: provider.Anthropic}, "summary")
		}},
	} {
		cfg.conversation.pendingText = "owed report"
		captureTerminal(t, step.run)
		if got := cfg.conversation.pendingText; got != "owed report" {
			t.Fatalf("%s dropped the carried text: %q", step.name, got)
		}
	}
	captureTerminal(t, func() {
		if err := commandClear(context.Background(), cfg, nil); err != nil {
			t.Fatal(err)
		}
	})
	if got := cfg.conversation.pendingText; got != "" {
		t.Fatalf("/clear should drop the carried text: %q", got)
	}
}

// A report that waits for a prompt whose request failed is kept in the history once,
// however often the prompt is tried.
func TestAgentReportIsRecordedOnceAcrossRetries(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[early]]": {newSSE("e1").text("early report").stop("end_turn", "")},
		"[[stuck]]": {blockStream},
		"[[next]]":  {newSSE("n1").text("thanks").stop("end_turn", "")},
	})
	cfg, h := agentTestConfig(t, fake, subagent.Config{}, true)
	if _, err := h.Spawn(context.Background(), tools.SpawnRequest{Description: "early", Prompt: "[[early]] go", Mode: tools.AgentModeExplore, Background: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a1 to finish", func() bool { running, _, _ := h.m.Counts(); return running == 0 })
	ctx, cancel := context.WithCancel(context.Background())
	captureTerminal(t, func() {
		go func() {
			waitFor(t, "the stuck request", func() bool { return fake.started.Load() > 0 })
			cancel()
		}()
		if err := runPrompt(ctx, cfg, "[[stuck]] what did it find?"); err == nil {
			t.Fatal("the stuck request should fail")
		}
	})
	if len(cfg.agentNotices) != 1 {
		t.Fatalf("the report should wait for the next prompt: %v", cfg.agentNotices)
	}
	captureTerminal(t, func() {
		if err := runPrompt(context.Background(), cfg, "[[next]] what did it find?"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(fmt.Sprint(messagesOf(t, fake.requests("[[next]]")[0])), "early report") {
		t.Fatal("the report should go with the retried prompt")
	}
	entries, err := loadConversationHistoryEntries()
	if err != nil {
		t.Fatal(err)
	}
	recorded := 0
	for _, entry := range entries {
		if entry.Type == "user" && strings.Contains(entry.Content, "early report") {
			recorded++
		}
	}
	if recorded != 1 {
		t.Fatalf("the report is in the history %d times", recorded)
	}
}
