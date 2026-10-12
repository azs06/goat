package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/azs06/goat/internal/provider"
)

const (
	maxCompactionHistoryItems = 200
	maxCompactionSourceChars  = 60000
	maxCompactionEntryChars   = 2000

	// defaultAutoCompactTokens is the conversation size, in tokens as reported by the
	// API, at which the next turn first compacts the session. GOAT_AUTO_COMPACT_TOKENS
	// overrides it; lower it when the account's tokens-per-minute limit is small.
	defaultAutoCompactTokens = 40000
	// defaultAnthropicAutoCompactTokens applies to models behind the Anthropic API, whose
	// cached transcripts make long conversations cheaper.
	defaultAnthropicAutoCompactTokens = 60000

	// charsPerTokenEstimate sizes history recorded without token usage.
	charsPerTokenEstimate = 4
)

// historyEntriesSinceLastReset returns the active conversation's entries.
func historyEntriesSinceLastReset(entries []historyEntry) []historyEntry {
	if activeConversationID != "" {
		var own []historyEntry
		for _, entry := range entries {
			if entry.ConversationID == activeConversationID {
				own = append(own, entry)
			}
		}
		return own
	}

	// A conversation without an ID owns the unstamped entries since the last unstamped
	// reset; entries of conversations that have IDs are never mixed in.
	var unstamped []historyEntry
	for _, entry := range entries {
		if entry.ConversationID == "" {
			unstamped = append(unstamped, entry)
		}
	}
	entries = unstamped

	lastReset := -1
	for i, entry := range entries {
		if entry.Type == "reset" {
			lastReset = i
		}
	}
	if lastReset == -1 {
		return entries
	}
	return entries[lastReset+1:]
}

func historyEntryCompactionText(entry historyEntry) string {
	switch entry.Type {
	case "user":
		return fmt.Sprintf("User: %s", strings.TrimSpace(entry.Content))
	case "assistant":
		return fmt.Sprintf("Assistant: %s", strings.TrimSpace(entry.Content))
	case "tool_call":
		if entry.Status == "user" {
			return fmt.Sprintf("User ran a shell command (%s): %s", entry.ToolName, strings.TrimSpace(entry.Arguments))
		}
		return fmt.Sprintf("Tool call %s: %s", entry.ToolName, strings.TrimSpace(entry.Arguments))
	case "tool_output":
		status := entry.Status
		if status == "" {
			status = "ok"
		}
		return fmt.Sprintf("Tool output %s (%s): %s", entry.ToolName, status, prepareToolOutputForModel(entry.ToolName, strings.TrimSpace(entry.Content)))
	case "error":
		return fmt.Sprintf("Error: %s", strings.TrimSpace(entry.Content))
	case "compact":
		return fmt.Sprintf("Compact summary: %s", strings.TrimSpace(entry.Content))
	default:
		return ""
	}
}

func buildCompactionSource(entries []historyEntry) string {
	entries = historyEntriesSinceLastReset(entries)
	if len(entries) > maxCompactionHistoryItems {
		entries = entries[len(entries)-maxCompactionHistoryItems:]
	}

	blocks := make([]string, 0, len(entries))
	for _, entry := range entries {
		block := historyEntryCompactionText(entry)
		if strings.TrimSpace(block) == "" {
			continue
		}
		block = truncateToolOutputCharsForModel(block, maxCompactionEntryChars)
		blocks = append(blocks, block)
	}

	return truncateToolOutputCharsForModel(strings.Join(blocks, "\n\n"), maxCompactionSourceChars)
}

func applyCompactedConversation(cfg *config, next provider.Conversation, summary, mode string) {
	recordCompactionSummary(cfg, summary, mode)
	replaceConversation(cfg, conversationState{conversation: next})
	recordCompactionSeed(cfg, next, summary)
}

// recordCompactionSummary logs the summary under the conversation it summarizes, before
// the switch, so the new conversation does not count it twice.
func recordCompactionSummary(cfg *config, summary, mode string) {
	recordHistory(historyEntry{Type: "compact", Content: summary, Status: mode, Provider: cfg.model.Provider})
}

// recordCompactionSeed logs the start of the new conversation: a reset, then the seed
// exchange it begins with.
func recordCompactionSeed(cfg *config, next provider.Conversation, summary string) {
	providerName := cfg.model.Provider
	recordHistory(historyEntry{Type: "reset", Provider: providerName})
	recordHistory(historyEntry{Type: "user", Content: provider.SeedPrompt(summary), Provider: providerName})
	recordHistory(historyEntry{Type: "assistant", Content: "Context loaded from compacted session summary.", ResponseID: next.ResponseID, Provider: providerName})
}

func currentConversationCharCount(entries []historyEntry) int {
	charCount := 0
	for _, entry := range entries {
		charCount += len([]rune(historyEntryCompactionText(entry)))
	}
	return charCount
}

// estimateConversationTokens starts from the latest token usage the API reported and
// adds a character-based estimate for everything recorded after it, such as tool
// output from a turn that failed or was interrupted before another response arrived.
func estimateConversationTokens(entries []historyEntry) int64 {
	entries = historyEntriesSinceLastReset(entries)

	latestUsage := -1
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].ContextTokens > 0 {
			latestUsage = i
			break
		}
	}

	if latestUsage == -1 {
		return int64(currentConversationCharCount(entries)) / charsPerTokenEstimate
	}

	// A response's entries are recorded together. Its usage already covers its own
	// output (entries that carry the usage), but not the tool outputs recorded with it.
	usage := entries[latestUsage]
	first := latestUsage
	for usage.ResponseID != "" && first > 0 && entries[first-1].ResponseID == usage.ResponseID {
		first--
	}

	var unreported []historyEntry
	for _, entry := range entries[first:] {
		if entry.ContextTokens > 0 && entry.ResponseID == usage.ResponseID {
			continue
		}
		unreported = append(unreported, entry)
	}
	return usage.ContextTokens + int64(currentConversationCharCount(unreported))/charsPerTokenEstimate
}

func shouldAutoCompactConversation(entries []historyEntry, tokenThreshold int64) (bool, string) {
	if tokenThreshold <= 0 {
		tokenThreshold = defaultAutoCompactTokens
	}

	if estimate := estimateConversationTokens(entries); estimate >= tokenThreshold {
		return true, fmt.Sprintf("context reached about %d tokens", estimate)
	}
	return false, ""
}

// autoCompactThreshold is the conversation size that triggers compaction. Models behind
// the Anthropic API get a larger default: their transcripts are cached, so a long
// conversation costs less there.
func autoCompactThreshold(cfg *config) int64 {
	if cfg.autoCompactTokens > 0 {
		return cfg.autoCompactTokens
	}
	if provider.SpeaksAnthropic(cfg.model.Provider) {
		return defaultAnthropicAutoCompactTokens
	}
	return defaultAutoCompactTokens
}

// maybeAutoCompactConversation compacts the conversation once it has grown past the
// threshold or was built with another prompt. focus, when set, tells the summary what
// to keep.
func maybeAutoCompactConversation(ctx context.Context, cfg *config, focus string) (bool, error) {
	if cfg == nil || cfg.conversation.conversation.IsEmpty() {
		return false, nil
	}

	reason := ""
	if cfg.staleConversation {
		reason = "goat's prompt or tools changed since this conversation was saved"
	} else {
		entries, err := loadConversationHistoryEntries()
		if err != nil {
			return false, err
		}
		shouldCompact, why := shouldAutoCompactConversation(entries, autoCompactThreshold(cfg))
		if !shouldCompact {
			return false, nil
		}
		reason = why
	}

	provider, err := cfg.currentProvider()
	if err != nil {
		return false, err
	}
	next, summary, err := compactConversation(ctx, provider, cfg.model.ID, focus)
	if err != nil {
		return false, fmt.Errorf("auto compact conversation: %w", err)
	}

	applyCompactedConversation(cfg, next, summary, "auto")
	fmt.Println(formatNotice(fmt.Sprintf("Conversation auto-compacted (%s).", reason)))
	return true, nil
}

// compactConversation summarizes the local history with p and starts a new
// conversation from the summary. focus, when set, tells the summary what to keep.
func compactConversation(ctx context.Context, p provider.Provider, model, focus string) (provider.Conversation, string, error) {
	entries, err := loadConversationHistoryEntries()
	if err != nil {
		return provider.Conversation{}, "", err
	}

	source := buildCompactionSource(entries)
	if strings.TrimSpace(source) == "" {
		return provider.Conversation{}, "", fmt.Errorf("no saved conversation history is available to compact")
	}

	summaryPrompt := "Summarize this coding session into a compact handoff for a fresh conversation. Include the current goal, files changed, concrete implementation state, unresolved issues, and the next recommended step. Keep it concise and factual. No markdown headings."
	if focus = strings.TrimSpace(focus); focus != "" {
		summaryPrompt += " Pay particular attention to: " + focus
	}
	startSpinner("Compacting conversation")
	defer stopSpinner()
	summary, err := completeWithRetry(ctx, p, model, "You produce concise session handoff summaries for coding work.", summaryPrompt+"\n\nSession history:\n"+source)
	if err != nil {
		return provider.Conversation{}, "", err
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return provider.Conversation{}, "", fmt.Errorf("compaction summary was empty")
	}

	// The new conversation starts from the summary, so it carries the project
	// instructions a new conversation would get.
	seed := summary
	if projectInstructions != nil {
		if instructions := projectInstructions(); instructions != "" {
			seed = instructions + "\n" + summary
		}
	}
	next, err := p.Seed(ctx, model, seed)
	if err != nil {
		return provider.Conversation{}, "", err
	}
	return next, summary, nil
}

// completeWithRetry retries transient failures the way turns do.
func completeWithRetry(ctx context.Context, p provider.Provider, model, system, prompt string) (string, error) {
	for attempt := 0; ; attempt++ {
		text, err := p.Complete(ctx, model, system, prompt)
		if err == nil {
			return text, nil
		}
		delay, retryable := p.RetryDelay(err, attempt)
		if !retryable || attempt == maxStreamRateLimitRetries || ctx.Err() != nil {
			return "", err
		}
		stopSpinner()
		warnf("%v; retrying in %s (%d/%d)", shortError(err), delay.Round(time.Millisecond), attempt+1, maxStreamRateLimitRetries)
		if err := streamRetrySleeper(ctx, delay); err != nil {
			return "", err
		}
	}
}
