package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/azs06/goat/internal/markdown"
	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/tools"

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
	// pendingText is what a request that failed carried for the model besides the
	// prompt and the tool results: messages steered into the turn and subagents' reports
	// taken partway through it. The next request sends it first. It lives in memory only.
	pendingText string
}

var streamRetrySleeper = sleepWithContext

// projectInstructions, when set, returns the project instructions sent at the start of
// a new conversation.
var projectInstructions func() string

// turnOptions configures one turn of sendPromptStream.
type turnOptions struct {
	model  string
	effort provider.Effort
	// planMode refuses tools that change anything and asks the model for a plan.
	planMode bool
	// codeMode lets the model run run_code scripts.
	codeMode bool
	// takeSteering, when set, returns what the user typed while the turn was working,
	// to be sent to the model with the next tool results.
	takeSteering func() string
	// checkpoint, when set, receives the state after every tool round so it can be
	// saved before the turn finishes.
	checkpoint func(conversationState)
	// compact, when set, is asked after every tool round's checkpoint whether the
	// conversation was compacted. If it was, it returns the conversation started from
	// the summary, and the turn goes on there; the round's tool results answer calls of
	// the old conversation, so they go as text.
	compact func(ctx context.Context) (provider.Conversation, bool)
	// approveBash, when set, is asked before every run_bash command; a non-nil error
	// rejects the command.
	approveBash func(ctx context.Context, command, workdir string) error
	// contextNote, when set, goes before the prompt in the turn's first request, such as
	// the output of the ! commands the user ran since the last message.
	contextNote string
	// vision is set when the model reads images: view_image works, and images reach it.
	vision bool
	// images go with the prompt, such as ones pasted at the prompt.
	images []provider.Image
	// verify, when set, checks the work when the model finishes a turn that may have
	// changed the workspace. It returns the message to send the model when the check
	// fails, which the turn then goes on with, or "" when it passed.
	verify func(ctx context.Context) (string, error)
	// verified, when set, holds what verify knows about the turn. A turn retried after
	// compacting passes the same one, so changes made before the retry are still
	// checked and earlier runs still count.
	verified *verifyState

	// conversationID, when set, is the conversation the turn's history entries belong
	// to instead of the active one, and parentID the conversation that started it; a
	// subagent's turn sets both.
	conversationID string
	parentID       string
	// sink, when set, is where the turn shows its work instead of the terminal.
	sink agentSink
	// inputWait, when set, counts the time the turn waited on the user instead of
	// inputWaitTotal, so it is not counted as tool run time.
	inputWait *atomic.Int64
	// livePlanMode, when set, is asked before each tool call whether plan mode is on,
	// in place of planMode, so a change partway through the turn applies to the next call.
	livePlanMode func() bool
	// serveAgents, when set, is called after every tool round, such as to answer the
	// approvals subagents are waiting on.
	serveAgents func(ctx context.Context)
	// takeNotices, when set, returns the reports of background subagents that finished,
	// to go to the model with the next tool results. Unlike takeSteering's text they are
	// not shown as a message from the user.
	takeNotices func() string
	// agents runs the subagent tools; only the top-level agent has it.
	agents tools.AgentHost
	// subagent is set for a subagent's turn: it gets the subagent prompt, no subagent
	// tools, and cannot undo.
	subagent bool
}

// record saves entry to the history as part of the turn's conversation.
func (o turnOptions) record(entry historyEntry) {
	recordHistoryWarn(o.stamp(entry), o.warnf)
}

// warnf warns about the turn through its sink: on the terminal for main, in the
// agent's events for a subagent, whose goroutine must never touch the terminal.
func (o turnOptions) warnf(format string, args ...any) {
	if o.sink == nil {
		warnf(format, args...)
		return
	}
	sinkWarnf(o.sink, format, args...)
}

// stamp marks entry as part of the turn's conversation when the turn has its own.
func (o turnOptions) stamp(entry historyEntry) historyEntry {
	if o.conversationID != "" && entry.ConversationID == "" {
		entry.ConversationID = o.conversationID
	}
	if o.parentID != "" && entry.ParentID == "" {
		entry.ParentID = o.parentID
	}
	return entry
}

// inputWaited is the total time spent waiting on the user, from counter when set or
// else inputWaitTotal.
func inputWaited(counter *atomic.Int64) time.Duration {
	if counter != nil {
		return time.Duration(counter.Load())
	}
	return time.Duration(inputWaitTotal.Load())
}

// addInputWait counts d of waiting on the user against the turn.
func addInputWait(opts turnOptions, d time.Duration) {
	if opts.inputWait != nil {
		opts.inputWait.Add(int64(d))
		return
	}
	inputWaitTotal.Add(int64(d))
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

// printModelText shows streamed assistant text. On a color terminal it goes through
// the markdown renderer, which prints each line once it is complete; plain output is
// printed as it arrives. Either way control characters are made visible first (carriage
// returns are dropped), so model text cannot move the cursor, clear the screen or set
// the window title. Lines are printed above the spinner, which stays under them with
// the input line, so where to type never disappears while the reply streams.
func printModelText(md *markdown.Stream, text string) {
	text = escapeControlChars(strings.ReplaceAll(text, "\r", ""))
	if !style.Enabled() || md == nil {
		fmt.Print(text)
		return
	}
	if out := md.Write(text); out != "" {
		relabelSpinner("Writing")
		printAboveSpinner(out)
	}
}

// streamAttempt makes one provider call, showing "Thinking" until the reply streams.
// Whatever ends the call, the sink ends the response: on the terminal the spinner is
// cleared and a partial last line is printed.
func streamAttempt(ctx context.Context, p provider.Provider, req provider.Request, sink agentSink) (*provider.Response, bool, error) {
	sink.Status("Thinking")
	defer sink.EndText()
	return p.Send(ctx, req, sink.Text)
}

// executeStreamedResponse sends one request, retrying transient failures as long as no
// text has been shown yet.
func executeStreamedResponse(ctx context.Context, p provider.Provider, req provider.Request, sink agentSink) (*provider.Response, bool, error) {
	var lastErr error
	for attempt := 0; attempt <= maxStreamRateLimitRetries; attempt++ {
		resp, printedText, err := streamAttempt(ctx, p, req, sink)
		if err == nil {
			return resp, printedText, nil
		}

		lastErr = err
		if printedText {
			return nil, true, fmt.Errorf("assistant output was interrupted after partial text was already printed: %w", err)
		}

		delay, retryable := p.RetryDelay(err, attempt)
		if !retryable || ctx.Err() != nil {
			return nil, false, err
		}
		if attempt == maxStreamRateLimitRetries {
			break
		}

		sinkWarnf(sink, "%v; retrying in %s (%d/%d)", shortError(err), delay.Round(time.Millisecond), attempt+1, maxStreamRateLimitRetries)
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
	case tools.RunCode:
		// The system prompt quotes this budget to the model (see buildSystemPrompt).
		return maxToolOutputCharsForModel, maxToolOutputLinesForModel
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
func runToolCalls(ctx context.Context, providerName string, opts turnOptions, resp *provider.Response) []provider.ToolResult {
	var results []provider.ToolResult
	if opts.sink == nil {
		opts.sink = &terminalSink{}
	}

	for _, toolCall := range resp.ToolCalls {
		if ctx.Err() != nil {
			skipped := fmt.Sprintf("Tool %s was not run because the user interrupted the turn.", toolCall.Name)
			opts.record(historyEntry{Type: "tool_output", ToolName: toolCall.Name, Content: skipped, ResponseID: resp.ID, Status: "skipped", Provider: providerName})
			results = append(results, provider.ToolResult{CallID: toolCall.ID, Name: toolCall.Name, Output: skipped, IsError: true})
			continue
		}

		opts.sink.ToolStart(toolCall)
		opts.record(withUsage(historyEntry{Type: "tool_call", ToolName: toolCall.Name, Arguments: toolCall.Arguments, ResponseID: resp.ID, Provider: providerName}, resp.Usage, opts.model))

		// Time spent waiting on the user (a run_bash approval) is not the tool's run time.
		started, waitedBefore := time.Now(), inputWaited(opts.inputWait)
		opts.sink.Status("Running " + toolDisplayName(toolCall.Name))
		planMode := opts.planMode
		if opts.livePlanMode != nil {
			planMode = opts.livePlanMode()
		}
		toolOpts := tools.Options{PlanMode: planMode, LivePlanMode: opts.livePlanMode, CodeMode: opts.codeMode, ApproveBash: opts.approveBash, CallID: toolCall.ID, Outputs: toolOutputsWarning(opts.warnf), Agents: opts.agents, Subagent: opts.subagent}
		var shown []provider.Image
		toolOpts.Vision = opts.vision
		toolOpts.AttachImage = func(img provider.Image) { shown = append(shown, img) }
		var nested *nestedRenderer
		if toolCall.Name == tools.RunCode {
			nested = &nestedRenderer{inputWait: opts.inputWait}
			if !isTerminalSink(opts.sink) {
				nested.sink = opts.sink
			}
			toolOpts.Nested = nested
		}
		toolOutput, err := tools.Execute(ctx, toolCall, toolOpts)
		opts.sink.Status("")
		if nested != nil {
			nested.finish()
		}
		opts.sink.ToolResult(toolCall, toolOutput, err, time.Since(started)-(inputWaited(opts.inputWait)-waitedBefore))
		result := provider.ToolResult{CallID: toolCall.ID, Name: toolCall.Name}
		// A run_code script that fails after a nested view_image still shows its image.
		result.Images = shown
		output := historyEntry{Type: "tool_output", ToolName: toolCall.Name, ResponseID: resp.ID, Provider: providerName, Images: len(shown)}
		if err != nil {
			output.Content, output.Status = err.Error(), "error"
			result.Output = fmt.Sprintf("Tool %s failed: %v", toolCall.Name, err)
			result.IsError = true
		} else {
			output.Content, output.Status = toolOutput, "ok"
			result.Output = toolOutput
		}
		full := result.Output
		result.Output = prepareToolOutputForModel(toolCall.Name, result.Output)
		// The history keeps the whole output, so the model can read what was cut. Other
		// agents append to the same history, so the number is taken as it is recorded.
		if result.Output != full && toolCall.Name != "read_output" {
			if n, ok := recordToolOutputNumbered(opts.stamp(output), opts.warnf); ok {
				result.Output += tools.ShortenedNote(n)
			}
		} else {
			opts.record(output)
		}
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

// midTurnCompactionFocus steers the summary made partway through a turn toward the
// work in flight, which the turn continues from.
const midTurnCompactionFocus = "the task in progress and what was about to be done next"

// continueAfterCompactionNote goes to the model, followed by the latest tool results,
// after the conversation was compacted partway through a turn.
const continueAfterCompactionNote = "[The conversation was compacted partway through this task. The results of your latest tool calls follow. Carry on with the task from where you left off.]"

// compactedToolResultsText carries tool results over to a conversation compacted
// partway through a turn. They answer calls the new conversation does not have, so they
// go as text, in full: the summary keeps only part of each.
func compactedToolResultsText(results []provider.ToolResult) string {
	var b strings.Builder
	b.WriteString(continueAfterCompactionNote)
	for _, result := range results {
		status := "ok"
		if result.IsError {
			status = "error"
		}
		fmt.Fprintf(&b, "\n\nResult of %s (%s):\n%s", result.Name, status, result.Output)
	}
	return b.String()
}

var errContextExceeded = errors.New("the conversation no longer fits in the model's context window")

// isContextOverflowError recognises both APIs' "prompt is too long" errors, and the
// "maximum context length" wording of OpenAI-compatible servers.
func isContextOverflowError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "context_length_exceeded") || strings.Contains(message, "prompt is too long") || strings.Contains(message, "context window") || strings.Contains(message, "maximum context length")
}

// continueAfterRecoveryNote goes to the model, followed by any tool results it was
// owed, when a turn that had already started is retried in a compacted conversation.
const continueAfterRecoveryNote = "[The conversation was compacted partway through this task because it could not be continued as it was. Carry on with the task from where you left off.]"

// recoveryPromptText is what a retry in a compacted conversation sends: the prompt
// again when the model never saw it, or a note and the owed tool results when the turn
// was already underway.
func recoveryPromptText(prompt string, moved bool, owed []provider.ToolResult) string {
	if !moved {
		return prompt
	}
	text := compactedToolResultsText(owed)
	return continueAfterRecoveryNote + strings.TrimPrefix(text, continueAfterCompactionNote)
}

// sendPromptStream runs one user turn: it streams the reply and keeps executing tool
// calls until the model answers without any. It always returns the state the next turn
// should continue from, including when it also returns an error.
func sendPromptStream(ctx context.Context, p provider.Provider, prompt string, conv conversationState, opts turnOptions) (conversationState, error) {
	providerName := p.Name()
	if opts.sink == nil {
		opts.sink = &terminalSink{}
	}
	opts.record(historyEntry{Type: "user", Content: prompt, Provider: providerName, Images: len(opts.images)})

	system, specs := promptFor(opts.codeMode, !opts.subagent)
	req := provider.Request{
		Model:        opts.model,
		System:       system,
		Effort:       opts.effort,
		Tools:        specs,
		Conversation: conv.conversation,
		ToolResults:  conv.pendingToolResults,
		UserText:     prompt,
		UserImages:   opts.images,
	}
	// A model that cannot see gets a note in each image's place. Only the request
	// changes: the stored transcript and the results still owed keep their images, so
	// they go again after a switch back to a model that sees.
	req.TextOnly = !opts.vision
	if conv.pendingText != "" {
		req.UserText = conv.pendingText + "\n\n" + req.UserText
	}
	// carried is the text this round's request takes that nothing else keeps: a request
	// that fails hands it back in pendingText.
	carried := conv.pendingText
	req.UserText = modelNote(opts.model, providerName) + opts.contextNote + req.UserText
	if opts.planMode {
		req.UserText = planModeNote + req.UserText
	}
	if conv.conversation.IsEmpty() && len(conv.pendingToolResults) == 0 && projectInstructions != nil {
		if instructions := projectInstructions(); instructions != "" {
			req.UserText = instructions + "\n" + req.UserText
		}
	}

	verified := opts.verified
	if verified == nil {
		verified = &verifyState{}
	}
	for round := 1; ; round++ {
		resp, _, err := executeStreamedResponse(ctx, p, req, opts.sink)
		if err != nil {
			opts.record(historyEntry{Type: "error", Content: err.Error(), ResponseID: req.Conversation.ResponseID, Status: "error", Provider: providerName})
			if isContextOverflowError(err) || provider.IsRequestTooLarge(err) {
				err = fmt.Errorf("%w (%v)", errContextExceeded, shortError(err))
			}
			return conversationState{conversation: req.Conversation, pendingToolResults: req.ToolResults, pendingText: carried}, err
		}

		if resp.Stop == provider.StopRefusal {
			// The declined output is discarded; anything this request carried is still owed.
			err := fmt.Errorf("the model declined to answer%s; rephrase the request or switch models with /model", detailSuffix(resp.RefusalDetail))
			opts.record(historyEntry{Type: "error", Content: err.Error(), ResponseID: resp.ID, Status: "refused", Provider: providerName})
			return conversationState{conversation: resp.Conversation, pendingToolResults: req.ToolResults, pendingText: carried}, err
		}

		var results []provider.ToolResult
		var stopErr error
		switch resp.Stop {
		case provider.StopToolUse:
			results = runToolCalls(ctx, providerName, opts, resp)
			for _, call := range resp.ToolCalls {
				verified.changed = verified.changed || changesWorkspaceWith(call, agentModeLookup(opts.agents))
			}
			if opts.serveAgents != nil {
				opts.serveAgents(ctx)
			}
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
			opts.record(withUsage(historyEntry{Type: "assistant", Content: resp.Text, ResponseID: resp.ID, Provider: providerName}, resp.Usage, opts.model))
			if stopErr != nil {
				opts.record(historyEntry{Type: "error", Content: stopErr.Error(), ResponseID: resp.ID, Status: "error", Provider: providerName})
				return next, stopErr
			}
			if opts.verify == nil || !verified.changed && !verified.failed {
				return next, nil
			}
			failure, err := verifyTurn(ctx, opts, verified, round)
			if err != nil {
				status := "error"
				if ctx.Err() != nil {
					status = "interrupted"
				}
				opts.record(historyEntry{Type: "error", Content: err.Error(), ResponseID: resp.ID, Status: status, Provider: providerName})
				return next, err
			}
			if failure == "" {
				return next, nil
			}
			// The model answers the failure in the same turn.
			if opts.checkpoint != nil {
				opts.checkpoint(next)
			}
			opts.record(historyEntry{Type: "user", Content: failure, Provider: providerName})
			req.Conversation = next.conversation
			req.ToolResults = nil
			req.UserText = failure
			carried = ""
			continue
		}
		if opts.checkpoint != nil {
			opts.checkpoint(next)
		}

		if stopErr != nil {
			opts.record(historyEntry{Type: "error", Content: stopErr.Error(), ResponseID: resp.ID, Status: "error", Provider: providerName})
			return next, stopErr
		}
		if err := ctx.Err(); err != nil {
			opts.record(historyEntry{Type: "error", Content: "turn interrupted by the user", ResponseID: resp.ID, Status: "interrupted", Provider: providerName})
			return next, err
		}
		if round >= maxToolRoundsPerTurn {
			err := fmt.Errorf("stopped after %d tool rounds in one turn; send another message to let the agent continue", maxToolRoundsPerTurn)
			opts.record(historyEntry{Type: "error", Content: err.Error(), ResponseID: resp.ID, Status: "error", Provider: providerName})
			return next, err
		}

		req.Conversation = next.conversation
		req.ToolResults = next.pendingToolResults
		req.UserText = ""
		req.UserImages = nil
		carried = ""
		if opts.compact != nil {
			if compacted, ok := opts.compact(ctx); ok {
				req.Conversation = compacted
				req.ToolResults = nil
				req.UserText = compactedToolResultsText(next.pendingToolResults)
				// The results go as text, and their images with it.
				req.UserImages = resultImages(next.pendingToolResults)
				opts.record(historyEntry{Type: "user", Content: req.UserText, Provider: providerName})
				// The summary's conversation has not been told the model yet.
				req.UserText = modelNote(opts.model, providerName) + req.UserText
				if opts.planMode {
					req.UserText = planModeNote + req.UserText
				}
			}
		}
		if opts.takeSteering != nil {
			if text := opts.takeSteering(); text != "" {
				opts.sink.User(text)
				opts.record(historyEntry{Type: "user", Content: text, Provider: providerName})
				carried = joinNonEmpty(carried, text)
				req.UserText = joinNonEmpty(req.UserText, text)
			}
		}
		if opts.takeNotices != nil {
			if text := opts.takeNotices(); text != "" {
				opts.record(historyEntry{Type: "user", Content: text, Provider: providerName})
				carried = joinNonEmpty(carried, text)
				req.UserText = joinNonEmpty(req.UserText, text)
			}
		}
	}
}

// joinNonEmpty joins a and b with a blank line, or returns the one that is not empty.
func joinNonEmpty(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "\n\n" + b
}

// resultImages collects the images tool results carry, in order.
func resultImages(results []provider.ToolResult) []provider.Image {
	var all []provider.Image
	for _, result := range results {
		all = append(all, result.Images...)
	}
	return all
}

func detailSuffix(detail string) string {
	if strings.TrimSpace(detail) == "" {
		return ""
	}
	return " (" + strings.TrimSpace(detail) + ")"
}
