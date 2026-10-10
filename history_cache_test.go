package main

import (
	"os"
	"testing"
)

func TestHistoryCacheReadsOnlyNewLines(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	recordHistory(historyEntry{Type: "user", Content: "one"})
	first, err := loadConversationHistoryEntries()
	if err != nil || len(first) != 1 {
		t.Fatalf("got %d entries (%v)", len(first), err)
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
	if err := os.Remove(".goat/history.jsonl"); err != nil {
		t.Fatal(err)
	}
	recordHistory(historyEntry{Type: "user", Content: "fresh"})
	fresh, _ := loadConversationHistoryEntries()
	if len(fresh) != 1 || fresh[0].Content != "fresh" {
		t.Fatalf("expected the new file's entries, got %+v", fresh)
	}
}
