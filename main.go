package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

const defaultModel = "gpt-5.4"

const (
	maxStreamRateLimitRetries = 3
	maxStreamRetryDelay       = 8 * time.Second

	maxToolOutputCharsForModel    = 16000
	maxToolOutputLinesForModel    = 400
	maxRunBashOutputCharsForModel = 6000
	maxRunBashOutputLinesForModel = 120

	defaultReasoningEffort    = shared.ReasoningEffortMedium
	maxCompactionHistoryItems = 40
	maxCompactionSourceChars  = 12000
	maxCompactionEntryChars   = 1200

	autoCompactHistoryItems = 24
	autoCompactSourceChars  = 18000
)

var rateLimitRetryDelayPattern = regexp.MustCompile(`(?i)try again in\s+([0-9]*\.?[0-9]+)s`)

type cliCommand struct {
	name        string
	description string
	callback    func(c *config, args ...string) error
}

type config struct {
	model           string
	reasoningEffort shared.ReasoningEffort
	responseID      string
	client          *openai.Client
}

func loadConfig() *config {
	model := strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	if model == "" {
		model = defaultModel
	}

	reasoningEffort, err := parseReasoningEffort(os.Getenv("OPENAI_REASONING_EFFORT"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warn] invalid OPENAI_REASONING_EFFORT: %v; using %s\n", err, defaultReasoningEffort)
		reasoningEffort = defaultReasoningEffort
	}

	cfg := &config{model: model, reasoningEffort: reasoningEffort}
	if responseID, err := loadLatestConversationResponseID(); err == nil {
		cfg.responseID = responseID
	} else {
		fmt.Fprintf(os.Stderr, "[warn] failed to load saved conversation context: %v\n", err)
	}

	return cfg
}

func commandExit(c *config, args ...string) error {
	fmt.Print("Closing the Goat... Goodbye!")
	os.Exit(0)
	return nil
}

func commandHelp(c *config, args ...string) error {
	fmt.Println("Welcome to the Goat!")
	fmt.Println("Available commands:")
	fmt.Println("- compact: Summarize local session history into a fresh compacted context")
	fmt.Println("- exit: Exit the Goat")
	fmt.Println("- help: Display available commands")
	fmt.Println("- history: Show the conversation history path")
	fmt.Println("- model: Show the active model")
	fmt.Println("- reset: Clear the conversation context")
	fmt.Println("- resume: Reload the saved conversation context from history")
	fmt.Println("- tools: List available tools")
	fmt.Println("- undo: Undo the latest recorded file or directory change")
	return nil
}

func commandReset(c *config, args ...string) error {
	c.responseID = ""
	recordHistory(historyEntry{Type: "reset"})
	fmt.Println("Conversation context cleared.")
	return nil
}

func commandModel(c *config, args ...string) error {
	fmt.Printf("Active model: %s\n", c.model)
	fmt.Printf("Reasoning effort: %s\n", c.reasoningEffort)
	return nil
}

func commandResume(c *config, args ...string) error {
	responseID, err := loadLatestConversationResponseID()
	if err != nil {
		return err
	}
	c.responseID = responseID
	if strings.TrimSpace(responseID) == "" {
		fmt.Println("No saved conversation context was found.")
		return nil
	}

	fmt.Println("Conversation context restored from saved history.")
	return nil
}

func commandTools(c *config, args ...string) error {
	fmt.Println("Available tools:")
	for _, name := range availableToolNames() {
		fmt.Printf("- %s\n", name)
	}
	return nil
}

func commandUndo(c *config, args ...string) error {
	message, err := undoLastChange()
	if err != nil {
		return err
	}
	fmt.Println(message)
	return nil
}

func commandCompact(c *config, args ...string) error {
	if c.client == nil {
		return fmt.Errorf("OpenAI client is not initialized")
	}

	nextResponseID, summary, err := compactConversation(context.Background(), c.client, c.model)
	if err != nil {
		return err
	}

	applyCompactedConversation(c, nextResponseID, summary, "manual")
	fmt.Println("Conversation context compacted.")
	return nil
}

func commandHistory(c *config, args ...string) error {
	resolvedPath, relPath, err := resolveToolPath(conversationHistoryPath())
	if err != nil {
		return err
	}

	if info, err := os.Stat(resolvedPath); err == nil {
		fmt.Printf("Conversation history: %s (%d bytes)\n", relPath, info.Size())
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	fmt.Printf("Conversation history: %s (not created yet)\n", relPath)
	return nil
}

var commands = map[string]cliCommand{
	"compact": {
		name:        "compact",
		description: "Summarize local session history into a fresh compacted context",
		callback:    commandCompact,
	},
	"exit": {
		name:        "exit",
		description: "Exit the Goat",
		callback:    commandExit,
	},
	"help": {
		name:        "help",
		description: "Display available commands",
		callback:    commandHelp,
	},
	"history": {
		name:        "history",
		description: "Show the conversation history path",
		callback:    commandHistory,
	},
	"model": {
		name:        "model",
		description: "Show the active model",
		callback:    commandModel,
	},
	"reset": {
		name:        "reset",
		description: "Clear the conversation context",
		callback:    commandReset,
	},
	"resume": {
		name:        "resume",
		description: "Reload the saved conversation context from history",
		callback:    commandResume,
	},
	"tools": {
		name:        "tools",
		description: "List available tools",
		callback:    commandTools,
	},
	"undo": {
		name:        "undo",
		description: "Undo the latest recorded file or directory change",
		callback:    commandUndo,
	},
}

func cleanInput(text string) []string {
	text = strings.TrimSpace(text)
	text = strings.ToLower(text)
	words := strings.Fields(text)
	return words
}

func createResponse(ctx context.Context, c *openai.Client, params responses.ResponseNewParams) (*responses.Response, error) {
	return c.Responses.New(ctx, params)
}

var responseCreator = createResponse

func parseReasoningEffort(raw string) (shared.ReasoningEffort, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return defaultReasoningEffort, nil
	}

	switch shared.ReasoningEffort(value) {
	case shared.ReasoningEffortLow, shared.ReasoningEffortMedium, shared.ReasoningEffortHigh:
		return shared.ReasoningEffort(value), nil
	default:
		return "", fmt.Errorf("unsupported reasoning effort %q", raw)
	}
}

func sendPrompt(ctx context.Context, c *openai.Client, model string, reasoningEffort shared.ReasoningEffort, prompt string) error {
	resp, err := responseCreator(ctx, c, newResponseParams(
		model,
		reasoningEffort,
		responses.ResponseNewParamsInputUnion{OfString: openai.String(prompt)},
		"",
	))
	if err != nil {
		return err
	}

	fmt.Println(resp.OutputText())
	return nil
}

func newPlainResponseParams(model string, reasoningEffort shared.ReasoningEffort, input responses.ResponseNewParamsInputUnion, previousResponseID, instructions string) responses.ResponseNewParams {
	params := responses.ResponseNewParams{
		Model: model,
		Input: input,
		Reasoning: shared.ReasoningParam{
			Effort: reasoningEffort,
		},
	}

	if strings.TrimSpace(instructions) != "" {
		params.Instructions = openai.String(instructions)
	}
	if strings.TrimSpace(previousResponseID) != "" {
		params.PreviousResponseID = openai.String(previousResponseID)
	}

	return params
}

func newResponseParams(model string, reasoningEffort shared.ReasoningEffort, input responses.ResponseNewParamsInputUnion, previousResponseID string) responses.ResponseNewParams {
	params := newPlainResponseParams(model, reasoningEffort, input, previousResponseID, SystemPrompt)
	params.Tools = getTools()
	return params
}

func createStreamedResponse(ctx context.Context, c *openai.Client, params responses.ResponseNewParams) (*responses.Response, bool, error) {
	stream := c.Responses.NewStreaming(ctx, params)
	defer stream.Close()

	var (
		finalResponse *responses.Response
		printedText   bool
	)

	for stream.Next() {
		switch event := stream.Current().AsAny().(type) {
		case responses.ResponseTextDeltaEvent:
			fmt.Print(event.Delta)
			printedText = true
		case responses.ResponseCompletedEvent:
			response := event.Response
			finalResponse = &response
		}
	}

	if err := stream.Err(); err != nil {
		return nil, printedText, err
	}
	if finalResponse == nil {
		return nil, printedText, fmt.Errorf("stream completed without a final response")
	}

	return finalResponse, printedText, nil
}

var streamedResponseCreator = createStreamedResponse
var streamRetrySleeper = sleepWithContext

type responseStreamErrorPayload struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryDelayFromHeaders(headers http.Header) (time.Duration, bool) {
	if retryAfterMS := strings.TrimSpace(headers.Get("Retry-After-Ms")); retryAfterMS != "" {
		ms, err := strconv.ParseFloat(retryAfterMS, 64)
		if err == nil && ms >= 0 {
			return time.Duration(ms * float64(time.Millisecond)), true
		}
	}

	if retryAfter := strings.TrimSpace(headers.Get("Retry-After")); retryAfter != "" {
		seconds, err := strconv.ParseFloat(retryAfter, 64)
		if err == nil && seconds >= 0 {
			return time.Duration(seconds * float64(time.Second)), true
		}

		retryAt, err := http.ParseTime(retryAfter)
		if err == nil {
			delay := time.Until(retryAt)
			if delay < 0 {
				return 0, true
			}
			return delay, true
		}
	}

	return 0, false
}

func retryDelayFromMessage(message string) (time.Duration, bool) {
	matches := rateLimitRetryDelayPattern.FindStringSubmatch(message)
	if len(matches) != 2 {
		return 0, false
	}

	seconds, err := strconv.ParseFloat(matches[1], 64)
	if err != nil || seconds < 0 {
		return 0, false
	}

	return time.Duration(seconds * float64(time.Second)), true
}

func fallbackRetryDelay(attempt int) time.Duration {
	delay := time.Second << attempt
	if delay > maxStreamRetryDelay {
		return maxStreamRetryDelay
	}
	return delay
}

func streamRetryDelay(err error, attempt int) (time.Duration, bool) {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests {
		if apiErr.Response != nil {
			if delay, ok := retryDelayFromHeaders(apiErr.Response.Header); ok {
				return delay, true
			}
		}
		if delay, ok := retryDelayFromMessage(apiErr.Message); ok {
			return delay, true
		}
		return fallbackRetryDelay(attempt), true
	}

	var streamErr *ssestream.StreamError
	if errors.As(err, &streamErr) {
		var payload responseStreamErrorPayload
		if json.Unmarshal(streamErr.Event.Data, &payload) == nil && payload.Error.Code == "rate_limit_exceeded" {
			if delay, ok := retryDelayFromMessage(payload.Error.Message); ok {
				return delay, true
			}
			return fallbackRetryDelay(attempt), true
		}
	}

	return 0, false
}

func executeStreamedResponse(ctx context.Context, c *openai.Client, params responses.ResponseNewParams) (*responses.Response, bool, error) {
	var lastErr error
	for attempt := 0; attempt <= maxStreamRateLimitRetries; attempt++ {
		resp, printedText, err := streamedResponseCreator(ctx, c, params)
		if err == nil {
			return resp, printedText, nil
		}

		lastErr = err
		if printedText {
			return nil, true, fmt.Errorf("assistant output was interrupted after partial text was already printed: %w", err)
		}

		delay, retryable := streamRetryDelay(err, attempt)
		if !retryable {
			return nil, false, err
		}
		if attempt == maxStreamRateLimitRetries {
			break
		}

		fmt.Fprintf(os.Stderr, "[warn] rate limit encountered; retrying in %s (%d/%d)\n", delay.Round(time.Millisecond), attempt+1, maxStreamRateLimitRetries)
		if err := streamRetrySleeper(ctx, delay); err != nil {
			return nil, false, err
		}
	}

	return nil, false, fmt.Errorf("request failed after %d rate-limit retry attempt(s): %w", maxStreamRateLimitRetries, lastErr)
}

func availableToolNames() []string {
	return []string{
		"read_dir",
		"tree_dir",
		"read_file",
		"write_file",
		"edit_file",
		"edit_json_file",
		"copy_file",
		"move_file",
		"delete_file",
		"copy_dir",
		"move_dir",
		"delete_dir",
		"undo_last_change",
		"run_bash",
		"find_files",
		"grep_files",
	}
}

func getTools() []responses.ToolUnionParam {
	return []responses.ToolUnionParam{
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "read_dir",
				Description: openai.String("List files and directories for a relative path in the workspace"),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "tree_dir",
				Description: openai.String("Render a recursive directory tree for a relative workspace path."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"limit": map[string]any{
							"type":    "integer",
							"minimum": 1,
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "read_file",
				Description: openai.String("Read a file from a relative path in the workspace"),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "write_file",
				Description: openai.String("Write or overwrite a file at a relative path in the workspace with provided content. Set preview=true to get a diff without changing the file."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"content": map[string]any{
							"type": "string",
						},
						"preview": map[string]any{
							"type": "boolean",
						},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "edit_file",
				Description: openai.String("Edit an existing workspace file by replacing exact text. Set preview=true to get a diff without changing the file."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"old_text": map[string]any{
							"type": "string",
						},
						"new_text": map[string]any{
							"type": "string",
						},
						"replace_all": map[string]any{
							"type": "boolean",
						},
						"preview": map[string]any{
							"type": "boolean",
						},
					},
					"required": []string{"path", "old_text", "new_text"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "edit_json_file",
				Description: openai.String("Edit a JSON file by setting or deleting a value at a JSON path. Use value_json for set operations and preview=true to inspect the diff first."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"json_path": map[string]any{
							"type": "string",
						},
						"operation": map[string]any{
							"type": "string",
							"enum": []string{"set", "delete"},
						},
						"value_json": map[string]any{
							"type": "string",
						},
						"preview": map[string]any{
							"type": "boolean",
						},
					},
					"required": []string{"path", "json_path", "operation"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "copy_file",
				Description: openai.String("Copy a file to another relative workspace path. Set preview=true to inspect the destination diff first."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"source_path": map[string]any{
							"type": "string",
						},
						"destination_path": map[string]any{
							"type": "string",
						},
						"overwrite": map[string]any{
							"type": "boolean",
						},
						"preview": map[string]any{
							"type": "boolean",
						},
					},
					"required": []string{"source_path", "destination_path"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "move_file",
				Description: openai.String("Move a file to another relative workspace path. Set preview=true to inspect the destination diff first."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"source_path": map[string]any{
							"type": "string",
						},
						"destination_path": map[string]any{
							"type": "string",
						},
						"overwrite": map[string]any{
							"type": "boolean",
						},
						"preview": map[string]any{
							"type": "boolean",
						},
					},
					"required": []string{"source_path", "destination_path"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "delete_file",
				Description: openai.String("Delete a file from the workspace. Set preview=true to inspect a deletion diff first."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"preview": map[string]any{
							"type": "boolean",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "copy_dir",
				Description: openai.String("Copy a directory to another relative workspace path. Set preview=true to inspect the destination changes first."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"source_path": map[string]any{
							"type": "string",
						},
						"destination_path": map[string]any{
							"type": "string",
						},
						"overwrite": map[string]any{
							"type": "boolean",
						},
						"preview": map[string]any{
							"type": "boolean",
						},
					},
					"required": []string{"source_path", "destination_path"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "move_dir",
				Description: openai.String("Move a directory to another relative workspace path. Set preview=true to inspect the changes first."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"source_path": map[string]any{
							"type": "string",
						},
						"destination_path": map[string]any{
							"type": "string",
						},
						"overwrite": map[string]any{
							"type": "boolean",
						},
						"preview": map[string]any{
							"type": "boolean",
						},
					},
					"required": []string{"source_path", "destination_path"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "delete_dir",
				Description: openai.String("Delete a directory from the workspace. Set preview=true to inspect the removal first."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"preview": map[string]any{
							"type": "boolean",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "undo_last_change",
				Description: openai.String("Undo the most recent mutating file or directory operation using saved snapshots. Set preview=true to inspect what would be restored first."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"preview": map[string]any{
							"type": "boolean",
						},
					},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "run_bash",
				Description: openai.String("Run a bash command inside the workspace and return combined stdout and stderr. Prefer concise commands because large outputs may be truncated for token safety."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{
							"type": "string",
						},
						"workdir": map[string]any{
							"type": "string",
						},
					},
					"required": []string{"command"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "find_files",
				Description: openai.String("Recursively find files within the workspace. Pattern can be a glob like *.go or a plain substring."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"pattern": map[string]any{
							"type": "string",
						},
						"limit": map[string]any{
							"type":    "integer",
							"minimum": 1,
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			OfFunction: &responses.FunctionToolParam{
				Name:        "grep_files",
				Description: openai.String("Recursively search for text inside files in the workspace and return matching lines."),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"query": map[string]any{
							"type": "string",
						},
						"case_sensitive": map[string]any{
							"type": "boolean",
						},
						"limit": map[string]any{
							"type":    "integer",
							"minimum": 1,
						},
					},
					"required": []string{"path", "query"},
				},
			},
		},
	}
}

func decodeToolArguments(raw string, target any) error {
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("decode tool arguments: %w", err)
	}
	return nil
}

func prettyToolArguments(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return ""
	}

	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return raw
	}

	formatted, err := json.MarshalIndent(decoded, "", "  ")
	if err != nil {
		return raw
	}

	return string(formatted)
}

func indentBlock(text, prefix string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func truncateForLog(text string, max int) string {
	if len(text) <= max {
		return text
	}
	if max < 4 {
		return text[:max]
	}
	return text[:max-3] + "..."
}

func toolOutputBudgetForModel(toolName string) (int, int) {
	switch toolName {
	case "run_bash":
		return maxRunBashOutputCharsForModel, maxRunBashOutputLinesForModel
	default:
		return maxToolOutputCharsForModel, maxToolOutputLinesForModel
	}
}

func truncateToolOutputLinesForModel(text string, maxLines int) string {
	if text == "" || maxLines <= 0 {
		return text
	}

	lines := strings.Split(text, "\n")
	if len(lines) <= maxLines {
		return text
	}

	headCount := maxLines / 2
	tailCount := maxLines - headCount
	if headCount == 0 || tailCount == 0 {
		return text
	}

	omitted := len(lines) - headCount - tailCount
	truncated := append([]string{}, lines[:headCount]...)
	truncated = append(truncated, fmt.Sprintf("[... %d line(s) omitted for token safety ...]", omitted))
	truncated = append(truncated, lines[len(lines)-tailCount:]...)
	return strings.Join(truncated, "\n")
}

func truncateToolOutputCharsForModel(text string, maxChars int) string {
	if text == "" || maxChars <= 0 {
		return text
	}

	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	if maxChars < 80 {
		return string(runes[:maxChars])
	}

	marker := fmt.Sprintf("\n[... %d character(s) omitted for token safety ...]\n", len(runes)-maxChars)
	markerRunes := []rune(marker)
	available := maxChars - len(markerRunes)
	if available < 2 {
		return string(runes[:maxChars])
	}

	headCount := available / 2
	tailCount := available - headCount
	omitted := len(runes) - headCount - tailCount
	marker = fmt.Sprintf("\n[... %d character(s) omitted for token safety ...]\n", omitted)
	markerRunes = []rune(marker)
	available = maxChars - len(markerRunes)
	if available < 2 {
		return string(runes[:maxChars])
	}

	headCount = available / 2
	tailCount = available - headCount
	return string(runes[:headCount]) + marker + string(runes[len(runes)-tailCount:])
}

func prepareToolOutputForModel(toolName, output string) string {
	maxChars, maxLines := toolOutputBudgetForModel(toolName)
	output = truncateToolOutputLinesForModel(output, maxLines)
	output = truncateToolOutputCharsForModel(output, maxChars)
	return output
}

func historyEntriesSinceLastReset(entries []historyEntry) []historyEntry {
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

func buildCompactionSeedPrompt(summary string) string {
	return fmt.Sprintf("Session summary from the previous conversation:\n\n%s\n\nUse this as the working context going forward. Reply with exactly: Context loaded.", strings.TrimSpace(summary))
}

func applyCompactedConversation(cfg *config, nextResponseID, summary, mode string) {
	seedPrompt := buildCompactionSeedPrompt(summary)
	recordHistory(historyEntry{Type: "compact", Content: summary, Status: mode})
	recordHistory(historyEntry{Type: "reset"})
	recordHistory(historyEntry{Type: "user", Content: seedPrompt})
	recordHistory(historyEntry{Type: "assistant", Content: "Context loaded from compacted session summary.", ResponseID: nextResponseID})
	cfg.responseID = nextResponseID
}

func currentConversationCompactionStats(entries []historyEntry) (int, int) {
	entries = historyEntriesSinceLastReset(entries)

	entryCount := 0
	charCount := 0
	for _, entry := range entries {
		block := historyEntryCompactionText(entry)
		if strings.TrimSpace(block) == "" {
			continue
		}
		entryCount++
		charCount += len([]rune(block))
	}

	return entryCount, charCount
}

func shouldAutoCompactConversation(entries []historyEntry) (bool, string) {
	entryCount, charCount := currentConversationCompactionStats(entries)
	if entryCount >= autoCompactHistoryItems {
		return true, fmt.Sprintf("history reached %d entries", entryCount)
	}
	if charCount >= autoCompactSourceChars {
		return true, fmt.Sprintf("history reached %d characters", charCount)
	}
	return false, ""
}

func maybeAutoCompactConversation(ctx context.Context, cfg *config) (bool, error) {
	if cfg == nil || cfg.client == nil || strings.TrimSpace(cfg.responseID) == "" {
		return false, nil
	}

	entries, err := loadConversationHistoryEntries()
	if err != nil {
		return false, err
	}

	shouldCompact, reason := shouldAutoCompactConversation(entries)
	if !shouldCompact {
		return false, nil
	}

	nextResponseID, summary, err := compactConversation(ctx, cfg.client, cfg.model)
	if err != nil {
		return false, fmt.Errorf("auto compact conversation: %w", err)
	}

	applyCompactedConversation(cfg, nextResponseID, summary, "auto")
	fmt.Printf("Conversation auto-compacted (%s).\n", reason)
	return true, nil
}

func compactConversation(ctx context.Context, c *openai.Client, model string) (string, string, error) {
	entries, err := loadConversationHistoryEntries()
	if err != nil {
		return "", "", err
	}

	source := buildCompactionSource(entries)
	if strings.TrimSpace(source) == "" {
		return "", "", fmt.Errorf("no saved conversation history is available to compact")
	}

	summaryPrompt := "Summarize this coding session into a compact handoff for a fresh conversation. Include the current goal, files changed, concrete implementation state, unresolved issues, and the next recommended step. Keep it concise and factual. No markdown headings."
	summaryResp, err := responseCreator(ctx, c, newPlainResponseParams(
		model,
		shared.ReasoningEffortLow,
		responses.ResponseNewParamsInputUnion{
			OfString: openai.String(summaryPrompt + "\n\nSession history:\n" + source),
		},
		"",
		"You produce concise session handoff summaries for coding work.",
	))
	if err != nil {
		return "", "", err
	}

	summary := strings.TrimSpace(summaryResp.OutputText())
	if summary == "" {
		return "", "", fmt.Errorf("compaction summary was empty")
	}

	seedResp, err := responseCreator(ctx, c, newPlainResponseParams(
		model,
		shared.ReasoningEffortLow,
		responses.ResponseNewParamsInputUnion{
			OfString: openai.String(buildCompactionSeedPrompt(summary)),
		},
		"",
		"You are loading a compacted session summary into a fresh conversation. Reply with exactly: Context loaded.",
	))
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(seedResp.ID) == "" {
		return "", "", fmt.Errorf("compaction seed response did not return an id")
	}

	return seedResp.ID, summary, nil
}

func runWithInputGuard(work func() error) error {
	guard, err := beginTerminalInputGuard()
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := guard.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "[warn] failed to restore terminal input state: %v\n", closeErr)
		}
	}()

	return work()
}

func logToolCallStart(toolCall responses.ResponseFunctionToolCall) {
	fmt.Printf("\n==> tool: %s\n", toolCall.Name)
	if args := prettyToolArguments(toolCall.Arguments); args != "" {
		fmt.Println(indentBlock(args, "    "))
	}
}

func logToolCallResult(name, output string, err error) {
	status := "ok"
	body := output
	if err != nil {
		status = "error"
		body = err.Error()
	}

	fmt.Printf("<== tool: %s [%s]\n", name, status)
	if strings.TrimSpace(body) != "" {
		fmt.Println(indentBlock(truncateForLog(body, 700), "    "))
	}
}

func executeToolCall(ctx context.Context, toolCall responses.ResponseFunctionToolCall) (string, error) {
	switch toolCall.Name {
	case "read_dir":
		var args struct {
			Path string `json:"path"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		entries, err := readDir(args.Path)
		if err != nil {
			return "", err
		}
		if len(entries) == 0 {
			return fmt.Sprintf("Directory listing for %s:\n(empty)", filepath.Clean(args.Path)), nil
		}
		return fmt.Sprintf("Directory listing for %s:\n%s", filepath.Clean(args.Path), strings.Join(entries, "\n")), nil
	case "tree_dir":
		var args struct {
			Path  string `json:"path"`
			Limit int    `json:"limit"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		lines, truncated, err := treeDir(args.Path, args.Limit)
		if err != nil {
			return "", err
		}
		message := fmt.Sprintf("Directory tree for %s:\n%s", filepath.Clean(args.Path), strings.Join(lines, "\n"))
		if truncated {
			message += fmt.Sprintf("\n(truncated after %d line(s))", len(lines))
		}
		return message, nil
	case "read_file":
		var args struct {
			Path string `json:"path"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		content, err := readFile(args.Path)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("File contents for %s:\n%s", filepath.Clean(args.Path), content), nil
	case "write_file":
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			Preview bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if strings.TrimSpace(args.Path) == "" {
			return "", fmt.Errorf("missing path")
		}
		if args.Preview {
			return previewWriteFile(args.Path, args.Content)
		}
		if err := writeFile(args.Path, args.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s written successfully.", filepath.Clean(args.Path)), nil
	case "edit_file":
		var args struct {
			Path       string `json:"path"`
			OldText    string `json:"old_text"`
			NewText    string `json:"new_text"`
			ReplaceAll bool   `json:"replace_all"`
			Preview    bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return previewEditFile(args.Path, args.OldText, args.NewText, args.ReplaceAll)
		}
		replaced, err := editFile(args.Path, args.OldText, args.NewText, args.ReplaceAll)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s edited successfully. Replaced %d occurrence(s).", filepath.Clean(args.Path), replaced), nil
	case "edit_json_file":
		var args struct {
			Path      string `json:"path"`
			JSONPath  string `json:"json_path"`
			Operation string `json:"operation"`
			ValueJSON string `json:"value_json"`
			Preview   bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return previewEditJSONFile(args.Path, args.JSONPath, args.Operation, args.ValueJSON)
		}
		if err := editJSONFile(args.Path, args.JSONPath, args.Operation, args.ValueJSON); err != nil {
			return "", err
		}
		return fmt.Sprintf("JSON file %s updated successfully.", filepath.Clean(args.Path)), nil
	case "copy_file":
		var args struct {
			SourcePath      string `json:"source_path"`
			DestinationPath string `json:"destination_path"`
			Overwrite       bool   `json:"overwrite"`
			Preview         bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return previewCopyFile(args.SourcePath, args.DestinationPath, args.Overwrite)
		}
		if err := copyFile(args.SourcePath, args.DestinationPath, args.Overwrite); err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s copied to %s successfully.", filepath.Clean(args.SourcePath), filepath.Clean(args.DestinationPath)), nil
	case "move_file":
		var args struct {
			SourcePath      string `json:"source_path"`
			DestinationPath string `json:"destination_path"`
			Overwrite       bool   `json:"overwrite"`
			Preview         bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return previewMoveFile(args.SourcePath, args.DestinationPath, args.Overwrite)
		}
		if err := moveFile(args.SourcePath, args.DestinationPath, args.Overwrite); err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s moved to %s successfully.", filepath.Clean(args.SourcePath), filepath.Clean(args.DestinationPath)), nil
	case "delete_file":
		var args struct {
			Path    string `json:"path"`
			Preview bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return previewDeleteFile(args.Path)
		}
		if err := deleteFile(args.Path); err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s deleted successfully.", filepath.Clean(args.Path)), nil
	case "copy_dir":
		var args struct {
			SourcePath      string `json:"source_path"`
			DestinationPath string `json:"destination_path"`
			Overwrite       bool   `json:"overwrite"`
			Preview         bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return previewCopyDir(args.SourcePath, args.DestinationPath, args.Overwrite)
		}
		if err := copyDir(args.SourcePath, args.DestinationPath, args.Overwrite); err != nil {
			return "", err
		}
		return fmt.Sprintf("Directory %s copied to %s successfully.", filepath.Clean(args.SourcePath), filepath.Clean(args.DestinationPath)), nil
	case "move_dir":
		var args struct {
			SourcePath      string `json:"source_path"`
			DestinationPath string `json:"destination_path"`
			Overwrite       bool   `json:"overwrite"`
			Preview         bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return previewMoveDir(args.SourcePath, args.DestinationPath, args.Overwrite)
		}
		if err := moveDir(args.SourcePath, args.DestinationPath, args.Overwrite); err != nil {
			return "", err
		}
		return fmt.Sprintf("Directory %s moved to %s successfully.", filepath.Clean(args.SourcePath), filepath.Clean(args.DestinationPath)), nil
	case "delete_dir":
		var args struct {
			Path    string `json:"path"`
			Preview bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return previewDeleteDir(args.Path)
		}
		if err := deleteDir(args.Path); err != nil {
			return "", err
		}
		return fmt.Sprintf("Directory %s deleted successfully.", filepath.Clean(args.Path)), nil
	case "undo_last_change":
		var args struct {
			Preview bool `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return previewUndoLastChange()
		}
		return undoLastChange()
	case "run_bash":
		var args struct {
			Command string `json:"command"`
			Workdir string `json:"workdir"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		return runBashCommand(ctx, args.Command, args.Workdir)
	case "find_files":
		var args struct {
			Path    string `json:"path"`
			Pattern string `json:"pattern"`
			Limit   int    `json:"limit"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		matches, truncated, err := findFiles(args.Path, args.Pattern, args.Limit)
		if err != nil {
			return "", err
		}
		if len(matches) == 0 {
			if strings.TrimSpace(args.Pattern) == "" {
				return fmt.Sprintf("No files found under %s.", filepath.Clean(args.Path)), nil
			}
			return fmt.Sprintf("No files found for pattern %q under %s.", args.Pattern, filepath.Clean(args.Path)), nil
		}

		message := fmt.Sprintf("Found %d file(s) under %s:\n%s", len(matches), filepath.Clean(args.Path), strings.Join(matches, "\n"))
		if truncated {
			message += fmt.Sprintf("\n(truncated after %d result(s))", len(matches))
		}
		return message, nil
	case "grep_files":
		var args struct {
			Path          string `json:"path"`
			Query         string `json:"query"`
			CaseSensitive bool   `json:"case_sensitive"`
			Limit         int    `json:"limit"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		matches, truncated, err := grepFiles(args.Path, args.Query, args.CaseSensitive, args.Limit)
		if err != nil {
			return "", err
		}
		if len(matches) == 0 {
			return fmt.Sprintf("No matches found for %q under %s.", args.Query, filepath.Clean(args.Path)), nil
		}

		message := fmt.Sprintf("Found %d match(es) for %q under %s:\n%s", len(matches), args.Query, filepath.Clean(args.Path), strings.Join(matches, "\n"))
		if truncated {
			message += fmt.Sprintf("\n(truncated after %d match(es))", len(matches))
		}
		return message, nil
	default:
		return "", fmt.Errorf("unsupported tool: %s", toolCall.Name)
	}
}

func sendPromptStream(ctx context.Context, c *openai.Client, prompt, responseID, model string, reasoningEffort shared.ReasoningEffort) (string, error) {
	recordHistory(historyEntry{Type: "user", Content: prompt})

	resp, printedText, err := executeStreamedResponse(ctx, c, newResponseParams(
		model,
		reasoningEffort,
		responses.ResponseNewParamsInputUnion{OfString: openai.String(prompt)},
		responseID,
	))
	if err != nil {
		recordHistory(historyEntry{Type: "error", Content: err.Error(), ResponseID: responseID, Status: "error"})
		return responseID, err
	}

	for {
		var toolOutputs []responses.ResponseInputItemUnionParam

		for _, item := range resp.Output {
			if item.Type != "function_call" {
				continue
			}

			toolCall := item.AsFunctionCall()
			logToolCallStart(toolCall)
			recordHistory(historyEntry{Type: "tool_call", ToolName: toolCall.Name, Arguments: toolCall.Arguments, ResponseID: resp.ID})

			toolOutput, err := executeToolCall(ctx, toolCall)
			logToolCallResult(toolCall.Name, toolOutput, err)
			toolOutputForModel := toolOutput
			if err != nil {
				recordHistory(historyEntry{Type: "tool_output", ToolName: toolCall.Name, Content: err.Error(), ResponseID: resp.ID, Status: "error"})
				toolOutputForModel = fmt.Sprintf("Tool %s failed: %v", toolCall.Name, err)
			} else {
				recordHistory(historyEntry{Type: "tool_output", ToolName: toolCall.Name, Content: toolOutput, ResponseID: resp.ID, Status: "ok"})
			}
			toolOutputForModel = prepareToolOutputForModel(toolCall.Name, toolOutputForModel)

			toolOutputs = append(toolOutputs, responses.ResponseInputItemUnionParam{
				OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: toolCall.CallID,
					Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
						OfString: openai.String(toolOutputForModel),
					},
				},
			})
		}

		if printedText {
			fmt.Println()
		}

		if len(toolOutputs) == 0 {
			recordHistory(historyEntry{Type: "assistant", Content: resp.OutputText(), ResponseID: resp.ID})
			return resp.ID, nil
		}

		previousResponseID := resp.ID
		resp, printedText, err = executeStreamedResponse(ctx, c, newResponseParams(
			model,
			reasoningEffort,
			responses.ResponseNewParamsInputUnion{
				OfInputItemList: toolOutputs,
			},
			previousResponseID,
		))
		if err != nil {
			if printedText {
				fmt.Println()
			}
			recordHistory(historyEntry{Type: "error", Content: err.Error(), ResponseID: previousResponseID, Status: "error"})
			return previousResponseID, err
		}
	}
}

func main() {
	ctx := context.Background()
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	cfg := loadConfig()
	apiKey := os.Getenv("OPENAI_API_KEY")
	client := openai.NewClient(
		option.WithAPIKey(apiKey),
	)
	cfg.client = &client
	fmt.Println("Welcome to G.O.A.T agent")
	if strings.TrimSpace(cfg.responseID) != "" {
		fmt.Println("Resumed saved conversation context from history.")
	}
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("G.O.A.T > ")
		scanner.Scan()
		text := scanner.Text()
		words := cleanInput(text)
		if len(words) == 0 {
			continue
		}

		command := words[0]

		if cmd, ok := commands[command]; ok {
			err := runWithInputGuard(func() error {
				return cmd.callback(cfg, words[1:]...)
			})
			if err != nil {
				fmt.Println("Error:", err)
			}
		} else {
			err := runWithInputGuard(func() error {
				if _, err := maybeAutoCompactConversation(ctx, cfg); err != nil {
					return err
				}
				nextResponseID, err := sendPromptStream(ctx, &client, text, cfg.responseID, cfg.model, cfg.reasoningEffort)
				if err != nil {
					return err
				}
				cfg.responseID = nextResponseID
				return nil
			})
			if err != nil {
				fmt.Println("Error:", err)
			}
		}
	}
}
