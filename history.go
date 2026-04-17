package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const defaultHistoryPath = ".goat/history.jsonl"

type historyEntry struct {
	Timestamp  string `json:"timestamp"`
	Type       string `json:"type"`
	Content    string `json:"content,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	Arguments  string `json:"arguments,omitempty"`
	ResponseID string `json:"response_id,omitempty"`
	Status     string `json:"status,omitempty"`
}

// this is defined as a variable to allow overriding in tests
var historyEntryAppender = appendHistoryEntry

func conversationHistoryPath() string {
	return defaultHistoryPath
}

func appendHistoryEntry(entry historyEntry) error {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}

	resolvedPath, _, err := resolveToolPath(conversationHistoryPath())
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(resolvedPath), 0755); err != nil {
		return fmt.Errorf("create history directory: %w", err)
	}

	encoded, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal history entry: %w", err)
	}
	encoded = append(encoded, '\n')

	file, err := os.OpenFile(resolvedPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open history file: %w", err)
	}
	defer file.Close()

	if _, err := file.Write(encoded); err != nil {
		return fmt.Errorf("write history entry: %w", err)
	}

	return nil
}

func recordHistory(entry historyEntry) {
	if err := historyEntryAppender(entry); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] failed to save history: %v\n", err)
	}
}
