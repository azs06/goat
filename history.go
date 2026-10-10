package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/azs06/goat/internal/provider"
)

const defaultHistoryPath = ".goat/history.jsonl"

// maxHistoryFieldChars caps the text saved per history field. The model only ever sees
// truncated tool output, so the full text of a large file or command output is not kept.
const maxHistoryFieldChars = 32000

type historyEntry struct {
	Timestamp  string `json:"timestamp"`
	Type       string `json:"type"`
	Content    string `json:"content,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	Arguments  string `json:"arguments,omitempty"`
	ResponseID string `json:"response_id,omitempty"`
	Status     string `json:"status,omitempty"`
	// ContextTokens is the total token usage of the response this entry came from.
	ContextTokens int64 `json:"context_tokens,omitempty"`
	// Provider is the API the entry belongs to; empty for entries from before goat
	// supported more than one.
	Provider string `json:"provider,omitempty"`
	// ConversationID ties the entry to one conversation, so a conversation brought back
	// with /resume finds its own history again. Empty for older entries.
	ConversationID string `json:"conversation_id,omitempty"`
}

// activeConversationID is the conversation new history entries belong to. When it is
// empty (a conversation started before entries carried IDs) the conversation is
// everything since the last reset entry.
var activeConversationID string

func newConversationID() string {
	return fmt.Sprintf("c%d", time.Now().UnixNano())
}

// this is defined as a variable to allow overriding in tests
var historyEntryAppender = appendHistoryEntry

func conversationHistoryPath() string {
	return defaultHistoryPath
}

// loadConversationHistoryEntries skips lines it cannot decode, such as a line cut short
// by a crash, so one bad line does not make the whole history unreadable.
func loadConversationHistoryEntries() ([]historyEntry, error) {
	resolvedPath, _, err := resolveStatePath(conversationHistoryPath())
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

	reader := bufio.NewReader(file)
	var entries []historyEntry
	skipped := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("read history file: %w", readErr)
		}

		if line = bytes.TrimSpace(line); len(line) > 0 {
			var entry historyEntry
			if err := json.Unmarshal(line, &entry); err != nil {
				skipped++
			} else {
				entries = append(entries, entry)
			}
		}

		if readErr != nil {
			break
		}
	}
	if skipped > 0 {
		warnf("skipped %d unreadable line(s) in %s", skipped, conversationHistoryPath())
	}

	return entries, nil
}

func appendHistoryEntry(entry historyEntry) error {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}

	resolvedPath, _, err := resolveStatePath(conversationHistoryPath())
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
	responseID, _, err := latestResumableConversation()
	return responseID, err
}

// latestResumableConversation finds the OpenAI response the history log last ended on,
// and the conversation that response belongs to, for resuming without a session file.
// Only OpenAI conversations can be resumed from a response ID alone.
func latestResumableConversation() (string, string, error) {
	entries, err := loadConversationHistoryEntries()
	if err != nil {
		return "", "", err
	}

	latestResponseID, conversationID := "", ""
	for _, entry := range entries {
		switch entry.Type {
		case "reset":
			latestResponseID, conversationID = "", ""
		case "assistant":
			if entry.Provider != "" && entry.Provider != provider.OpenAI {
				latestResponseID, conversationID = "", ""
			} else if strings.TrimSpace(entry.ResponseID) != "" {
				latestResponseID, conversationID = strings.TrimSpace(entry.ResponseID), entry.ConversationID
			}
		}
	}

	return latestResponseID, conversationID, nil
}

func recordHistory(entry historyEntry) {
	if entry.ConversationID == "" {
		entry.ConversationID = activeConversationID
	}
	entry.Content = truncateToolOutputCharsForModel(entry.Content, maxHistoryFieldChars)
	entry.Arguments = truncateToolOutputCharsForModel(entry.Arguments, maxHistoryFieldChars)
	if err := historyEntryAppender(entry); err != nil {
		warnf("failed to save history: %v", err)
	}
}
