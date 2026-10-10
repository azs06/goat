package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/azs06/goat/internal/provider"

	"github.com/azs06/goat/internal/style"
)

const (
	maxStreamRateLimitRetries = 3

	maxToolRoundsPerTurn = 40

	maxToolOutputCharsForModel    = 16000
	maxToolOutputLinesForModel    = 400
	maxRunBashOutputCharsForModel = 6000
	maxRunBashOutputLinesForModel = 120
)

// conversationState is where the next turn continues from. pendingToolResults answer
// tool calls of the conversation's latest response that have not been sent yet, which
// happens when a turn is interrupted or fails partway through its tool loop. Both APIs
// reject a follow-up that leaves those calls unanswered, so they go with the next prompt.
type conversationState struct {
	conversation       provider.Conversation
	pendingToolResults []provider.ToolResult
}

var streamRetrySleeper = sleepWithContext

// projectInstructions, when set, returns the project instructions sent at the start of
// a new conversation.
var projectInstructions func() string

// takeSteering, when set, returns what the user typed while the turn was working, to
// be sent to the model with the next tool results.
var takeSteering func() string

// turnCheckpoint, when set, receives the state after every tool round so it can be
// saved before the turn finishes.
var turnCheckpoint func(conversationState)

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

// printModelText shows streamed assistant text. On a color terminal it goes through
// the markdown renderer, which prints each line once it is complete; plain output is
// printed as it arrives. Either way control characters are made visible first (carriage
// returns are dropped), so model text cannot move the cursor, clear the screen or set
// the window title. The spinner is cleared just before anything is drawn.
func printModelText(md *markdownStream, text string) {
	text = escapeControlChars(strings.ReplaceAll(text, "\r", ""))
	if !style.Enabled() || md == nil {
		fmt.Print(text)
		return
	}
	if out := md.Write(text); out != "" {
		stopSpinner()
		fmt.Print(out)
	}
}

// streamAttempt makes one provider call with a fresh renderer and a "Thinking" spinner.
// Whatever ends the call, the spinner is cleared and a partial last line is printed.
func streamAttempt(ctx context.Context, p provider.Provider, req provider.Request) (*provider.Response, bool, error) {
	md := newMarkdownStream(terminalWidth())
	startSpinner("Thinking")
	defer func() {
		stopSpinner()
		if style.Enabled() {
			fmt.Print(md.Flush())
		}
	}()
	return p.Send(ctx, req, func(text string) { printModelText(md, text) })
}

// executeStreamedResponse sends one request, retrying transient failures as long as no
// text has been shown yet.
func executeStreamedResponse(ctx context.Context, provider provider.Provider, req provider.Request) (*provider.Response, bool, error) {
	var lastErr error
	for attempt := 0; attempt <= maxStreamRateLimitRetries; attempt++ {
		resp, printedText, err := streamAttempt(ctx, provider, req)
		if err == nil {
			return resp, printedText, nil
		}

		lastErr = err
		if printedText {
			return nil, true, fmt.Errorf("assistant output was interrupted after partial text was already printed: %w", err)
		}

		delay, retryable := provider.RetryDelay(err, attempt)
		if !retryable || ctx.Err() != nil {
			return nil, false, err
		}
		if attempt == maxStreamRateLimitRetries {
			break
		}

		warnf("%v; retrying in %s (%d/%d)", shortError(err), delay.Round(time.Millisecond), attempt+1, maxStreamRateLimitRetries)
		if err := streamRetrySleeper(ctx, delay); err != nil {
			return nil, false, err
		}
	}

	return nil, false, fmt.Errorf("request failed after %d rate-limit retry attempt(s): %w", maxStreamRateLimitRetries, lastErr)
}

func shortError(err error) string {
	message := err.Error()
	if line, _, found := strings.Cut(message, "\n"); found {
		message = line
	}
	return truncateForLog(message, 200)
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
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	if max < 4 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
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

func charOmissionMarker(omitted int) string {
	return fmt.Sprintf("\n[... %d character(s) omitted for token safety ...]\n", omitted)
}

func truncateToolOutputCharsForModel(text string, maxChars int) string {
	runes := []rune(text)
	if maxChars <= 0 || len(runes) <= maxChars {
		return text
	}

	// Size the marker for the worst case; the real omitted count has no more digits.
	available := maxChars - len([]rune(charOmissionMarker(len(runes))))
	if maxChars < 80 || available < 2 {
		return string(runes[:maxChars])
	}

	headCount := available / 2
	tailCount := available - headCount
	return string(runes[:headCount]) + charOmissionMarker(len(runes)-headCount-tailCount) + string(runes[len(runes)-tailCount:])
}

func prepareToolOutputForModel(toolName, output string) string {
	maxChars, maxLines := toolOutputBudgetForModel(toolName)
	output = truncateToolOutputLinesForModel(output, maxLines)
	output = truncateToolOutputCharsForModel(output, maxChars)
	return output
}

// toolRenderWidth is the width tool blocks are fitted to. Plain output (pipes, logs,
// NO_COLOR) is never clipped, so logs keep the full command and error text.
func toolRenderWidth() int {
	if !style.Enabled() {
		return math.MaxInt32
	}
	return terminalWidth()
}

// logToolCallStart prints the "● Name(args)" header for a tool call. The leading blank
// line separates it from the assistant text or tool block before it.
// activityOut is where tool calls are shown. A -p run sends them to stderr, so stdout
// holds only the reply.
var activityOut io.Writer = os.Stdout

func logToolCallStart(toolCall provider.ToolCall) {
	fmt.Fprint(activityOut, "\n"+renderToolHeader(toolCall, toolRenderWidth())+"\n")
}

// logToolCallResult prints a finished tool call's result under its header.
func logToolCallResult(toolCall provider.ToolCall, output string, err error, elapsed time.Duration) {
	fmt.Fprintln(activityOut, renderToolResult(toolCall, output, err, elapsed, toolRenderWidth()))
}

// runToolCalls executes every tool call in resp and returns one result per call. Once
// ctx is cancelled the remaining calls are answered without being run, so the results
// always cover every call.
func runToolCalls(ctx context.Context, providerName, model string, resp *provider.Response) []provider.ToolResult {
	var results []provider.ToolResult

	for _, toolCall := range resp.ToolCalls {
		if ctx.Err() != nil {
			skipped := fmt.Sprintf("Tool %s was not run because the user interrupted the turn.", toolCall.Name)
			recordHistory(historyEntry{Type: "tool_output", ToolName: toolCall.Name, Content: skipped, ResponseID: resp.ID, Status: "skipped", Provider: providerName})
			results = append(results, provider.ToolResult{CallID: toolCall.ID, Name: toolCall.Name, Output: skipped, IsError: true})
			continue
		}

		logToolCallStart(toolCall)
		recordHistory(withUsage(historyEntry{Type: "tool_call", ToolName: toolCall.Name, Arguments: toolCall.Arguments, ResponseID: resp.ID, Provider: providerName}, resp.Usage, model))

		// Time spent waiting on the user (a run_bash approval) is not the tool's run time.
		started, waitedBefore := time.Now(), inputWaitTotal
		startSpinner("Running " + toolDisplayName(toolCall.Name))
		toolOutput, err := executeToolCall(ctx, toolCall)
		stopSpinner()
		logToolCallResult(toolCall, toolOutput, err, time.Since(started)-(inputWaitTotal-waitedBefore))
		result := provider.ToolResult{CallID: toolCall.ID, Name: toolCall.Name}
		if err != nil {
			recordHistory(historyEntry{Type: "tool_output", ToolName: toolCall.Name, Content: err.Error(), ResponseID: resp.ID, Status: "error", Provider: providerName})
			result.Output = fmt.Sprintf("Tool %s failed: %v", toolCall.Name, err)
			result.IsError = true
		} else {
			recordHistory(historyEntry{Type: "tool_output", ToolName: toolCall.Name, Content: toolOutput, ResponseID: resp.ID, Status: "ok", Provider: providerName})
			result.Output = toolOutput
		}
		result.Output = prepareToolOutputForModel(toolCall.Name, result.Output)
		results = append(results, result)
	}

	return results
}

// notRunResults answers tool calls that must not run, so the conversation stays valid.
func notRunResults(calls []provider.ToolCall, reason string) []provider.ToolResult {
	results := make([]provider.ToolResult, 0, len(calls))
	for _, call := range calls {
		results = append(results, provider.ToolResult{CallID: call.ID, Name: call.Name, Output: fmt.Sprintf("Tool %s was not run: %s.", call.Name, reason), IsError: true})
	}
	return results
}

var errContextExceeded = errors.New("the conversation no longer fits in the model's context window; run /compact")

// isContextOverflowError recognises both APIs' "prompt is too long" errors.
func isContextOverflowError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "context_length_exceeded") || strings.Contains(message, "prompt is too long") || strings.Contains(message, "context window")
}

// sendPromptStream runs one user turn: it streams the reply and keeps executing tool
// calls until the model answers without any. It always returns the state the next turn
// should continue from, including when it also returns an error.
func sendPromptStream(ctx context.Context, p provider.Provider, prompt string, conv conversationState, model string, effort provider.Effort) (conversationState, error) {
	providerName := p.Name()
	recordHistory(historyEntry{Type: "user", Content: prompt, Provider: providerName})

	req := provider.Request{
		Model:        model,
		System:       SystemPrompt,
		Effort:       effort,
		Tools:        agentTools,
		Conversation: conv.conversation,
		ToolResults:  conv.pendingToolResults,
		UserText:     prompt,
	}
	if planMode {
		req.UserText = planModeNote + req.UserText
	}
	if conv.conversation.IsEmpty() && len(conv.pendingToolResults) == 0 && projectInstructions != nil {
		if instructions := projectInstructions(); instructions != "" {
			req.UserText = instructions + "\n" + prompt
		}
	}

	for round := 1; ; round++ {
		resp, printedText, err := executeStreamedResponse(ctx, p, req)
		// The markdown renderer ends every line itself; plain streamed text may not.
		if printedText && !style.Enabled() {
			fmt.Println()
		}
		if err != nil {
			recordHistory(historyEntry{Type: "error", Content: err.Error(), ResponseID: req.Conversation.ResponseID, Status: "error", Provider: providerName})
			if isContextOverflowError(err) {
				err = fmt.Errorf("%w (%v)", errContextExceeded, shortError(err))
			}
			return conversationState{conversation: req.Conversation, pendingToolResults: req.ToolResults}, err
		}

		if resp.Stop == provider.StopRefusal {
			// The declined output is discarded; anything this request carried is still owed.
			err := fmt.Errorf("the model declined to answer%s; rephrase the request or switch models with /model", detailSuffix(resp.RefusalDetail))
			recordHistory(historyEntry{Type: "error", Content: err.Error(), ResponseID: resp.ID, Status: "refused", Provider: providerName})
			return conversationState{conversation: resp.Conversation, pendingToolResults: req.ToolResults}, err
		}

		var results []provider.ToolResult
		var stopErr error
		switch resp.Stop {
		case provider.StopToolUse:
			results = runToolCalls(ctx, providerName, model, resp)
		case provider.StopMaxTokens:
			stopErr = errors.New("the response was cut off at the output token limit")
		case provider.StopContextExceeded:
			stopErr = errContextExceeded
		case provider.StopOther:
			stopErr = fmt.Errorf("the response stopped unexpectedly%s", detailSuffix(resp.RefusalDetail))
		}
		if resp.Stop != provider.StopToolUse && len(resp.ToolCalls) > 0 {
			results = notRunResults(resp.ToolCalls, "the response was cut off before the call was complete")
		}

		next := conversationState{conversation: resp.Conversation, pendingToolResults: results}
		if len(results) == 0 {
			recordHistory(withUsage(historyEntry{Type: "assistant", Content: resp.Text, ResponseID: resp.ID, Provider: providerName}, resp.Usage, model))
			if stopErr != nil {
				recordHistory(historyEntry{Type: "error", Content: stopErr.Error(), ResponseID: resp.ID, Status: "error", Provider: providerName})
			}
			return next, stopErr
		}
		if turnCheckpoint != nil {
			turnCheckpoint(next)
		}

		if stopErr != nil {
			recordHistory(historyEntry{Type: "error", Content: stopErr.Error(), ResponseID: resp.ID, Status: "error", Provider: providerName})
			return next, stopErr
		}
		if err := ctx.Err(); err != nil {
			recordHistory(historyEntry{Type: "error", Content: "turn interrupted by the user", ResponseID: resp.ID, Status: "interrupted", Provider: providerName})
			return next, err
		}
		if round >= maxToolRoundsPerTurn {
			err := fmt.Errorf("stopped after %d tool rounds in one turn; send another message to let the agent continue", maxToolRoundsPerTurn)
			recordHistory(historyEntry{Type: "error", Content: err.Error(), ResponseID: resp.ID, Status: "error", Provider: providerName})
			return next, err
		}

		req.Conversation = next.conversation
		req.ToolResults = next.pendingToolResults
		req.UserText = ""
		if takeSteering != nil {
			if text := takeSteering(); text != "" {
				fmt.Println("\n" + renderUserMessage(text, terminalWidth()))
				recordHistory(historyEntry{Type: "user", Content: text, Provider: providerName})
				req.UserText = text
			}
		}
	}
}

func detailSuffix(detail string) string {
	if strings.TrimSpace(detail) == "" {
		return ""
	}
	return " (" + strings.TrimSpace(detail) + ")"
}
