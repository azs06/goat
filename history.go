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
	"sync"
	"time"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/workspace"
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
	// PromptTokens is the response's whole input, of which CacheReadTokens were read from
	// the provider's prompt cache and CacheWriteTokens written to it.
	PromptTokens     int64 `json:"prompt_tokens,omitempty"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	// Model is the model that produced the response, for pricing.
	Model string `json:"model,omitempty"`
	// Provider is the API the entry belongs to; empty for entries from before goat
	// supported more than one.
	Provider string `json:"provider,omitempty"`
	// ConversationID ties the entry to one conversation, so a conversation brought back
	// with /resume finds its own history again. Empty for older entries.
	ConversationID string `json:"conversation_id,omitempty"`
}

// withUsage records a response's token usage on entry.
func withUsage(entry historyEntry, usage provider.Usage, model string) historyEntry {
	entry.Model = model
	entry.ContextTokens = usage.ContextTokens()
	entry.PromptTokens = usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
	entry.CacheReadTokens = usage.CacheReadTokens
	entry.CacheWriteTokens = usage.CacheWriteTokens
	return entry
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

// historyCache keeps the parsed history log between reads. The log only grows, so a
// read parses just the lines appended since the last one; a different or shorter file
// is read again from the start.
var historyCache struct {
	sync.Mutex
	path    string
	info    os.FileInfo
	offset  int64
	entries []historyEntry
}

// loadConversationHistoryEntries skips lines it cannot decode, such as a line cut short
// by a crash, so one bad line does not make the whole history unreadable. The returned
// slice must not be modified.
func loadConversationHistoryEntries() ([]historyEntry, error) {
	resolvedPath, _, err := workspace.ResolveStatePath(conversationHistoryPath())
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
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("read history file: %w", err)
	}

	historyCache.Lock()
	defer historyCache.Unlock()
	if historyCache.path != resolvedPath || historyCache.info == nil || !os.SameFile(historyCache.info, info) || info.Size() < historyCache.offset {
		historyCache.path, historyCache.offset, historyCache.entries = resolvedPath, 0, nil
	}
	historyCache.info = info
	if info.Size() == historyCache.offset {
		return historyCache.entries[:len(historyCache.entries):len(historyCache.entries)], nil
	}
	if _, err := file.Seek(historyCache.offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("read history file: %w", err)
	}

	reader := bufio.NewReader(file)
	skipped := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("read history file: %w", readErr)
		}
		// A last line without its newline may still be being written; it is read
		// next time, unless it is the end of a file that stopped growing.
		if readErr != nil && len(line) > 0 && !json.Valid(bytes.TrimSpace(line)) {
			break
		}
		historyCache.offset += int64(len(line))
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var entry historyEntry
			if err := json.Unmarshal(trimmed, &entry); err != nil {
				skipped++
			} else {
				historyCache.entries = append(historyCache.entries, entry)
			}
		}
		if readErr != nil {
			break
		}
	}
	if skipped > 0 {
		warnf("skipped %d unreadable line(s) in %s", skipped, displayPath(resolvedPath))
	}

	return historyCache.entries[:len(historyCache.entries):len(historyCache.entries)], nil
}

func appendHistoryEntry(entry historyEntry) error {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}

	resolvedPath, _, err := workspace.ResolveStatePath(conversationHistoryPath())
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
	entry.Content = truncateToolOutputCharsForModel(provider.HideEndpointPaths(entry.Content), maxHistoryFieldChars)
	entry.Arguments = truncateToolOutputCharsForModel(entry.Arguments, maxHistoryFieldChars)
	if err := historyEntryAppender(entry); err != nil {
		warnf("failed to save history: %v", err)
	}
}

// cacheUsage sums the prompt cache usage of the active conversation's responses.
type cacheUsage struct {
	responses                   int
	promptTokens, read, written int64
}

func conversationCacheUsage(entries []historyEntry) cacheUsage {
	var usage cacheUsage
	seen := map[string]bool{}
	for _, entry := range historyEntriesSinceLastReset(entries) {
		// Every tool call of a response carries the same usage; count it once.
		if entry.PromptTokens == 0 || entry.ResponseID == "" || seen[entry.ResponseID] {
			continue
		}
		seen[entry.ResponseID] = true
		usage.responses++
		usage.promptTokens += entry.PromptTokens
		usage.read += entry.CacheReadTokens
		usage.written += entry.CacheWriteTokens
	}
	return usage
}
