package main

import (
	"fmt"
	"time"

	"github.com/azs06/goat/internal/markdown"
	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/style"
)

// agentSink is where a turn shows its work. The main conversation shows it on the
// terminal; a subagent's turn keeps it for later, and never writes to the terminal.
//
// Text gets the reply as it streams, raw: the sink makes control characters safe
// itself. EndText ends one streamed response, whether or not any text came. Status
// shows what the turn is doing, such as "Thinking"; Status("") clears it. A tool call
// is shown with ToolStart and, once it finished, ToolResult; a run_code script's
// calls come between its own ToolStart and ToolResult. User is a message the user sent
// partway through the turn, and Notice a line about the turn, such as a retry.
type agentSink interface {
	Text(s string)
	EndText()
	ToolStart(c provider.ToolCall)
	ToolResult(c provider.ToolCall, out string, err error, elapsed time.Duration)
	User(text string)
	Notice(text string)
	Status(label string)
}

// terminalSink shows a turn the way the main conversation always has: the reply
// through the markdown renderer above the spinner, tool calls on activityOut.
type terminalSink struct {
	// md renders the response being streamed; each response gets a fresh one.
	md *markdown.Stream
	// printed is set once the response being streamed showed any text.
	printed bool
}

func (t *terminalSink) Text(s string) {
	if t.md == nil {
		t.md = markdown.NewStream(terminalWidth())
	}
	t.printed = true
	printModelText(t.md, s)
}

// EndText prints the partial last line the renderer holds. Plain streamed text may
// not end its line, so it gets one.
func (t *terminalSink) EndText() {
	stopSpinner()
	if style.Enabled() {
		if t.md != nil {
			fmt.Print(t.md.Flush())
		}
	} else if t.printed {
		fmt.Println()
	}
	t.md, t.printed = nil, false
}

func (t *terminalSink) ToolStart(c provider.ToolCall) {
	logToolCallStart(c)
}

func (t *terminalSink) ToolResult(c provider.ToolCall, out string, err error, elapsed time.Duration) {
	logToolCallResult(c, out, err, elapsed)
}

func (t *terminalSink) User(text string) {
	fmt.Println("\n" + renderUserMessage(text, terminalWidth()))
}

func (t *terminalSink) Notice(text string) {
	fmt.Fprintln(activityOut, formatNotice(text))
}

func (t *terminalSink) Status(label string) {
	if label == "" {
		stopSpinner()
		return
	}
	startSpinner(label)
}

// isTerminalSink reports whether sink shows the turn on the terminal.
func isTerminalSink(sink agentSink) bool {
	_, ok := sink.(*terminalSink)
	return ok
}

// sinkWarnf warns about a turn: on stderr for the terminal, as a notice otherwise.
func sinkWarnf(sink agentSink, format string, args ...any) {
	if isTerminalSink(sink) {
		warnf(format, args...)
		return
	}
	// Errors may quote a base URL whose path holds a token.
	sink.Notice(provider.HideEndpointPaths(fmt.Sprintf(format, args...)))
}
