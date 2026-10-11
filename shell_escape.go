package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/workspace"
)

// A line starting with "!" runs as a shell command, the way run_bash would run it but
// without asking (the user typed it) and without a timeout (Ctrl-C stops it). Its
// output is shown, recorded for /output, and sent to the model with the next message,
// so "!npm test" then "fix the failures" works.

// shellResultPreviewLines is how much of a ! command's output is shown; /output shows
// the rest.
const shellResultPreviewLines = 40

// maxPendingShellRuns is how many ! runs wait for the next message; older ones drop.
const maxPendingShellRuns = 5

// shellEscapeText returns the command of a "!command" line, and the lines after it,
// which are not part of the command: a queue put back in the prompt after Ctrl-C holds
// one message per line. A -p run never treats its prompt as a command: it may come from
// a file.
func shellEscapeText(cfg *config, text string) (command, rest string, ok bool) {
	if cfg.oneShot {
		return "", "", false
	}
	command, ok = strings.CutPrefix(strings.TrimSpace(text), "!")
	command, rest, _ = strings.Cut(command, "\n")
	return strings.TrimSpace(command), strings.TrimSpace(rest), ok
}

// requeueLines queues each line on its own, so the queue joins plain messages into one
// and runs commands on their own, as if they had been queued during a turn.
func requeueLines(text string) {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			enqueue(line)
		}
	}
}

// runShellEscape runs a command the user typed after "!".
func runShellEscape(ctx context.Context, cfg *config, command string) error {
	if command == "" {
		return errors.New("type a shell command after !, such as !git status")
	}
	// Lines piped into goat may come from anywhere, so they run commands only when
	// run_bash commands would run without asking.
	if !cfg.interactive && (cfg.approver == nil || !cfg.approver.autoApprove) {
		return errors.New("! runs shell commands only on an interactive terminal, or with -y")
	}

	arguments, _ := json.Marshal(map[string]string{"command": command})
	call := provider.ToolCall{Name: "run_bash", Arguments: string(arguments)}
	recordHistory(historyEntry{Type: "tool_call", ToolName: call.Name, Arguments: call.Arguments, Status: "user", Provider: cfg.model.Provider})

	started := time.Now()
	startSpinner("Running " + truncateForLog(strings.Join(strings.Fields(command), " "), 40))
	output, err := workspace.RunBashFor(ctx, command, ".", 0)
	stopSpinner()
	interrupted := err != nil && ctx.Err() != nil
	shown := err
	if interrupted {
		shown = context.Canceled
	}
	fmt.Println(renderToolResultLimit(call, output, shown, time.Since(started), toolRenderWidth(), shellResultPreviewLines))

	status, content := "ok", output
	switch {
	case interrupted:
		status, content = "interrupted", err.Error()
	case err != nil:
		status, content = "error", err.Error()
	}
	recordHistory(historyEntry{Type: "tool_output", ToolName: call.Name, Content: content, Status: status, Provider: cfg.model.Provider})
	if interrupted {
		content = "Stopped by the user: " + content
	}
	cfg.pendingShell = append(cfg.pendingShell, prepareToolOutputForModel(call.Name, content))
	if extra := len(cfg.pendingShell) - maxPendingShellRuns; extra > 0 {
		cfg.pendingShell = cfg.pendingShell[extra:]
	}
	if interrupted {
		return context.Canceled
	}
	return nil
}

// shellContextNote is what goes before the next message about the ! commands the user
// ran since the last one, or "" when there are none.
func shellContextNote(runs []string) string {
	if len(runs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[The user ran these shell commands in the workspace themselves. Their output is context for the message that follows, not a request to act on it.]\n")
	for _, run := range runs {
		b.WriteString("<shell>\n" + run + "\n</shell>\n")
	}
	return b.String() + "\n"
}

// pendingShellFooterPart says how many ! runs go with the next message, for the footer.
func pendingShellFooterPart(cfg *config) string {
	if n := len(cfg.pendingShell); n > 0 {
		return plural(n, "shell result") + " for the next message"
	}
	return ""
}

// renderShellHint replaces the footer while a "!" line is typed.
func renderShellHint(width int) string {
	return style.Muted.Render(style.TruncateToWidth("  ! runs this in the workspace shell "+style.Glyphs().Dot+" its output goes to goat with your next message", width-1))
}
