package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func loadConversationHistoryEntries() ([]historyEntry, error) {
	resolvedPath, _, err := resolveToolPath(conversationHistoryPath())
	if err != nil {
		return nil, err
	}

	file, err := os.Open(resolvedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open history file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 1024*1024)

	var entries []historyEntry
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var entry historyEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("decode history entry: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan history file: %w", err)
	}

	return entries, nil
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

func loadLatestConversationResponseID() (string, error) {
	entries, err := loadConversationHistoryEntries()
	if err != nil {
		return "", err
	}

	latestResponseID := ""
	for _, entry := range entries {
		switch entry.Type {
		case "reset":
			latestResponseID = ""
		case "assistant":
			if strings.TrimSpace(entry.ResponseID) != "" {
				latestResponseID = strings.TrimSpace(entry.ResponseID)
			}
		}
	}

	return latestResponseID, nil
}

func recordHistory(entry historyEntry) {
	if err := historyEntryAppender(entry); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] failed to save history: %v\n", err)
	}
}
