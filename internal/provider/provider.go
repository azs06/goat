// Package provider is goat's internal LLM API. This file holds the provider-independent
// types the agent loop, compaction and commands use; models.go names the providers and
// their models and credentials; openai.go, anthropic.go and deepseek.go adapt the types
// to each API.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ToolSpec describes a tool the model may call. Parameters is a JSON schema object.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ToolCall is a call the model asked for. Arguments is the raw JSON text it produced.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ToolResult answers a ToolCall.
type ToolResult struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name"`
	Output  string `json:"output"`
	IsError bool   `json:"is_error,omitempty"`
}

// Usage is the token usage of one response. InputTokens excludes cached tokens.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// ContextTokens is the size of the conversation after this response.
func (u Usage) ContextTokens() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.OutputTokens
}

// Conversation is where the next request continues from. Its contents belong to the
// provider named in it: OpenAI keeps the conversation on its servers and only needs the
// latest response ID, while Anthropic needs the whole transcript, kept append-only.
type Conversation struct {
	Provider string `json:"provider,omitempty"`
	// Format is FormatChat for a Chat Completions transcript, and empty for the
	// formats the provider name settles. An entry's api can change between runs.
	Format     string            `json:"format,omitempty"`
	ResponseID string            `json:"response_id,omitempty"`
	Messages   []json.RawMessage `json:"messages,omitempty"`
}

// FormatChat marks a transcript in the Chat Completions format.
const FormatChat = "chat"

// Fits reports whether provider can continue the conversation: it is empty, or it was
// made by that provider in the format the provider speaks now.
func (c Conversation) Fits(provider string) bool {
	if c.IsEmpty() {
		return true
	}
	format := ""
	if entry, ok := entries[provider]; ok && entry.API == APIOpenAI {
		format = FormatChat
	}
	return c.Provider == provider && c.Format == format
}

func (c Conversation) IsEmpty() bool {
	return strings.TrimSpace(c.ResponseID) == "" && len(c.Messages) == 0
}

// Request is one model call within a conversation.
type Request struct {
	Model        string
	System       string
	Effort       Effort
	Tools        []ToolSpec
	Conversation Conversation
	// ToolResults answer the tool calls of the conversation's latest response; they are
	// sent before UserText.
	ToolResults []ToolResult
	UserText    string
}

type StopReason string

const (
	StopEndTurn         StopReason = "end_turn"
	StopToolUse         StopReason = "tool_use"
	StopMaxTokens       StopReason = "max_tokens"
	StopRefusal         StopReason = "refusal"
	StopContextExceeded StopReason = "context_exceeded"
	StopOther           StopReason = "other"
)

// Response is the result of one model call.
type Response struct {
	ID string
	// Conversation includes this request and response. On a refusal it is the state
	// from before the request, since the declined output is discarded.
	Conversation  Conversation
	Text          string
	ToolCalls     []ToolCall
	Usage         Usage
	Stop          StopReason
	RefusalDetail string
}

// Provider is one LLM API.
type Provider interface {
	Name() string
	// Send performs one streamed model call, passing visible text to onText as it
	// arrives. printed reports whether any text was shown, also when err is non-nil.
	Send(ctx context.Context, req Request, onText func(string)) (resp *Response, printed bool, err error)
	// Complete is a one-off call without tools, outside any conversation.
	Complete(ctx context.Context, model, system, prompt string) (string, error)
	// Seed starts a new conversation whose context is a session summary.
	Seed(ctx context.Context, model, summary string) (Conversation, error)
	// RetryDelay reports whether err is a transient failure worth retrying, and how long
	// to wait first.
	RetryDelay(err error, attempt int) (time.Duration, bool)
	// ListModels returns the chat models the account can use.
	ListModels(ctx context.Context) ([]string, error)
}

// Effort is how much the model should reason. Providers map it to what each model
// supports.
type Effort string

const (
	EffortNone    Effort = "none"
	EffortMinimal Effort = "minimal"
	EffortLow     Effort = "low"
	EffortMedium  Effort = "medium"
	EffortHigh    Effort = "high"
	EffortXhigh   Effort = "xhigh"
	EffortMax     Effort = "max"
)

var EffortLevels = []Effort{EffortNone, EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXhigh, EffortMax}

const DefaultEffort = EffortMedium

func ParseEffort(raw string) (Effort, error) {
	value := Effort(strings.ToLower(strings.TrimSpace(raw)))
	if value == "" {
		return DefaultEffort, nil
	}
	for _, level := range EffortLevels {
		if value == level {
			return value, nil
		}
	}
	return "", fmt.Errorf("unsupported reasoning effort %q; use one of %s", raw, JoinEfforts(EffortLevels))
}

func JoinEfforts(levels []Effort) string {
	names := make([]string, len(levels))
	for i, level := range levels {
		names[i] = string(level)
	}
	return strings.Join(names, ", ")
}

// SeedPrompt is the user message that starts a conversation from a session summary.
func SeedPrompt(summary string) string {
	return fmt.Sprintf("Session summary from the previous conversation:\n\n%s\n\nUse this as the working context going forward. Reply with exactly: Context loaded.", strings.TrimSpace(summary))
}

// DescribeEffort says what the provider will actually receive for effort. p is the
// provider serving model, or nil when it has not been created yet.
func DescribeEffort(p Provider, model Model, effort Effort) string {
	kind := model.Provider
	if entry, ok := entries[model.Provider]; ok {
		kind = entry.API
	}
	switch kind {
	case OpenAI:
		if _, isEntry := entries[model.Provider]; isEntry {
			return "nothing; goat sends no effort setting to OpenAI-compatible servers"
		}
		if !openAISupportsReasoning(model.ID) {
			return "nothing; this model takes no reasoning setting"
		}
		return string(openAIReasoningEffort(effort))
	case DeepSeek:
		if level, ok := deepSeekEffort(effort); ok {
			return string(level)
		}
		return "thinking turned off"
	case Anthropic:
		caps := guessAnthropicCapabilities(model.ID)
		if _, isEntry := entries[model.Provider]; isEntry && !strings.HasPrefix(strings.ToLower(model.ID), "claude-") {
			caps = entryModelCapabilities
		}
		if provider, ok := p.(*anthropicProvider); ok {
			provider.mu.Lock()
			if known, found := provider.capabilities[model.ID]; found {
				caps = known
			}
			provider.mu.Unlock()
		}
		if level, ok := anthropicEffort(effort, caps); ok {
			return string(level)
		}
		return "nothing; this model takes no effort setting"
	default:
		return string(effort)
	}
}

func truncate(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	if max < 4 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}
