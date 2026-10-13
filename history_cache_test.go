package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHistoryCacheReadsOnlyNewLines(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	recordHistory(historyEntry{Type: "user", Content: "one"})
	first, err := loadConversationHistoryEntries()
	if err != nil || len(first) != 1 {
		t.Fatalf("got %d entries (%v)", len(first), err)
	}
	if info, err := os.Stat(statePath(t, "history.jsonl")); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("expected a private history file, got %v", info.Mode().Perm())
	}
	recordHistory(historyEntry{Type: "assistant", Content: "two"})
	second, _ := loadConversationHistoryEntries()
	if len(second) != 2 || second[1].Content != "two" {
		t.Fatalf("expected the appended entry, got %+v", second)
	}
	// Appending to the returned slice must not disturb the cache.
	_ = append(second, historyEntry{Content: "caller's own"})
	if again, _ := loadConversationHistoryEntries(); len(again) != 2 {
		t.Fatalf("cache was modified: %+v", again)
	}

	// A replaced file is read again from the start.
	if err := os.Remove(statePath(t, "history.jsonl")); err != nil {
		t.Fatal(err)
	}
	recordHistory(historyEntry{Type: "user", Content: "fresh"})
	fresh, _ := loadConversationHistoryEntries()
	if len(fresh) != 1 || fresh[0].Content != "fresh" {
		t.Fatalf("expected the new file's entries, got %+v", fresh)
	}
}

// loadLatestConversationResponseID returns just the response ID half of
// latestResumableConversation.
func loadLatestConversationResponseID() (string, error) {
	responseID, _, err := latestResumableConversation()
	return responseID, err
}

func TestHistorySurvivesHugeAndCorruptEntries(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	recordHistory(historyEntry{Type: "tool_output", ToolName: "run_bash", Content: strings.Repeat("x", 2<<20)})
	recordHistory(historyEntry{Type: "assistant", Content: "done", ResponseID: "resp-1"})

	historyPath := filepath.Join(statePath(t, ""), "history.jsonl")
	file, err := os.OpenFile(historyPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open history: %v", err)
	}
	file.WriteString(`{"type":"assistant","content":"cut sho` + "\n")
	file.Close()

	info, err := os.Stat(historyPath)
	if err != nil {
		t.Fatalf("stat history: %v", err)
	}
	if info.Size() > 1<<20 {
		t.Fatalf("expected huge content to be truncated before saving, history is %d bytes", info.Size())
	}

	entries, err := loadConversationHistoryEntries()
	if err != nil {
		t.Fatalf("expected history to load, got %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 readable entries, got %d", len(entries))
	}
	responseID, err := loadLatestConversationResponseID()
	if err != nil || responseID != "resp-1" {
		t.Fatalf("expected resp-1, got %q (%v)", responseID, err)
	}
}
