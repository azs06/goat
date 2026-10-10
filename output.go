package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/terminal"
	"github.com/azs06/goat/internal/workspace"
)

// toolRun is a tool call of the active conversation with its recorded output.
type toolRun struct {
	call   historyEntry
	output historyEntry
}

// recentToolRuns returns the conversation's tool calls that have output, newest first.
func recentToolRuns(entries []historyEntry) []toolRun {
	entries = historyEntriesSinceLastReset(entries)
	var runs []toolRun
	var pending []historyEntry
	for _, entry := range entries {
		switch entry.Type {
		case "tool_call":
			pending = append(pending, entry)
		case "tool_output":
			// Outputs follow their calls in order.
			for i, call := range pending {
				if call.ToolName == entry.ToolName && call.ResponseID == entry.ResponseID {
					runs = append(runs, toolRun{call: call, output: entry})
					pending = append(pending[:i], pending[i+1:]...)
					break
				}
			}
		}
	}
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}
	return runs
}

func (r toolRun) title() string {
	summary, _ := toolArgSummary(r.call.ToolName, r.call.Arguments)
	return toolDisplayName(r.call.ToolName) + "(" + style.TruncateToWidth(summary, 80) + ")"
}

// commandOutput shows the full output of a recent tool call: /output for the latest
// (or a picker on a terminal), /output N for the Nth latest.
func commandOutput(ctx context.Context, c *config, args []string) error {
	entries, err := loadConversationHistoryEntries()
	if err != nil {
		return err
	}
	runs := recentToolRuns(entries)
	if len(runs) == 0 {
		return errors.New("no tool has run in this conversation yet")
	}

	index := 0
	if len(args) > 0 {
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 1 || n > len(runs) {
			return fmt.Errorf("choose a number between 1 (the latest) and %d", len(runs))
		}
		index = n - 1
	} else if len(runs) > 1 {
		options := make([]selectOption, 0, min(len(runs), 50))
		for i, run := range runs[:min(len(runs), 50)] {
			options = append(options, selectOption{label: fmt.Sprintf("%d. %s", i+1, escapeControlChars(run.title())), hint: run.output.Status})
		}
		chosen, err := selectOptionInteractive(ctx, c, "Show the output of", options)
		switch {
		case errors.Is(err, errSelectorUnavailable):
		case err != nil:
			return err
		case chosen < 0:
			return nil
		default:
			index = chosen
		}
	}

	run := runs[index]
	text := style.Strong.Render(escapeControlChars(run.title())) + "\n" + escapeControlChars(run.output.Content)
	if len(run.output.Content) >= maxHistoryFieldChars {
		text += "\n" + style.Muted.Render(fmt.Sprintf("(the history keeps the first %d characters)", maxHistoryFieldChars))
	}
	return showLong(ctx, c, text)
}

// showLong prints text. When it is taller than the terminal it is paged like more:
// a screenful, then Space for the next one, Enter for one more line, G for the rest or q
// to stop. Earlier pages stay in the scrollback. Keys are read through goat's own
// reader, so no other program competes with it for the terminal.
func showLong(ctx context.Context, c *config, text string) error {
	lines := strings.Split(text, "\n")
	rows := terminalRows()
	if c.input == nil || !c.interactive || !style.Enabled() || len(lines) <= rows-2 {
		fmt.Println(text)
		return nil
	}
	resume := suspendInputGuard()
	defer resume()
	restore, err := terminal.EnterCbreak()
	if err != nil {
		fmt.Println(text)
		return nil
	}
	defer restore()

	width := terminalWidth()
	// heightOf is how many rows a line takes once the terminal wraps it.
	heightOf := func(line string) int { return max(1, (style.VisibleWidth(line)+width-1)/width) }
	shown := 0
	budget := rows - 2
	for shown < len(lines) {
		for used := 0; shown < len(lines) && (used == 0 || used+heightOf(lines[shown]) <= budget); shown++ {
			fmt.Println(lines[shown])
			used += heightOf(lines[shown])
		}
		if shown == len(lines) {
			break
		}
		fmt.Print(style.Subtle.Render(fmt.Sprintf("-- %d%% %s space next page %s enter next line %s G all %s q stop --", shown*100/len(lines), style.Glyphs().Dot, style.Glyphs().Dot, style.Glyphs().Dot, style.Glyphs().Dot)))
		key, err := c.input.readKey(ctx)
		fmt.Print("\r\x1b[2K")
		switch {
		case err != nil, key == "q", key == "Q", key == "\x1b", key == "\x03":
			fmt.Println(style.Muted.Render(fmt.Sprintf("%s %s not shown", style.Glyphs().Ellipsis, plural(len(lines)-shown, "line"))))
			return nil
		case key == "\r", key == "\n", key == "j", key == "\x1b[B", key == "\x1bOB":
			budget = 1
		case key == "G":
			budget = len(lines) * width
		default:
			budget = rows - 2
		}
	}
	return nil
}

// lastReply is the text of the conversation's latest assistant reply.
func lastReply(entries []historyEntry) string {
	entries = historyEntriesSinceLastReset(entries)
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type == "assistant" && strings.TrimSpace(entries[i].Content) != "" {
			return entries[i].Content
		}
	}
	return ""
}

// clipboardWriter copies text; a variable so tests do not touch the real clipboard.
var clipboardWriter = copyToClipboard

// copyToClipboard uses the platform's clipboard tool, or the OSC 52 escape sequence,
// which most terminals (also over SSH) turn into a clipboard write.
func copyToClipboard(text string) (string, error) {
	for _, tool := range [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "--clipboard", "--input"}, {"clip.exe"}} {
		if _, err := exec.LookPath(tool[0]); err != nil {
			continue
		}
		cmd := exec.Command(tool[0], tool[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return tool[0], nil
		}
	}
	if !terminal.StdinIsTerminal() {
		return "", errors.New("no clipboard tool found (pbcopy, wl-copy, xclip, xsel)")
	}
	fmt.Print("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a")
	return "the terminal", nil
}

// commandCopy copies the latest reply to the clipboard.
func commandCopy(ctx context.Context, c *config, args []string) error {
	entries, err := loadConversationHistoryEntries()
	if err != nil {
		return err
	}
	reply := lastReply(entries)
	if reply == "" {
		return errors.New("there is no reply to copy yet")
	}
	via, err := clipboardWriter(reply)
	if err != nil {
		return err
	}
	fmt.Println(formatSuccess(fmt.Sprintf("Copied the last reply (%s) using %s.", plural(countLines(reply), "line"), via)))
	return nil
}

// transcriptMarkdown renders the conversation as Markdown.
func transcriptMarkdown(entries []historyEntry, model string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# goat conversation\n\n_%s · exported %s_\n", model, time.Now().Format("2006-01-02 15:04"))
	fence := func(text string) string {
		marker := "```"
		for strings.Contains(text, marker) {
			marker += "`"
		}
		return marker + "\n" + strings.TrimRight(text, "\n") + "\n" + marker
	}
	for _, entry := range historyEntriesSinceLastReset(entries) {
		switch entry.Type {
		case "user":
			fmt.Fprintf(&b, "\n## You\n\n%s\n", strings.TrimSpace(entry.Content))
		case "assistant":
			if text := strings.TrimSpace(entry.Content); text != "" {
				fmt.Fprintf(&b, "\n## goat\n\n%s\n", text)
			}
		case "tool_call":
			summary, _ := toolArgSummary(entry.ToolName, entry.Arguments)
			fmt.Fprintf(&b, "\n**%s**\n", toolDisplayName(entry.ToolName)+"("+summary+")")
		case "tool_output":
			status := ""
			if entry.Status != "" && entry.Status != "ok" {
				status = " (" + entry.Status + ")"
			}
			fmt.Fprintf(&b, "\n<details><summary>output%s</summary>\n\n%s\n\n</details>\n", status, fence(entry.Content))
		case "error":
			fmt.Fprintf(&b, "\n> **Error:** %s\n", strings.TrimSpace(entry.Content))
		case "compact":
			fmt.Fprintf(&b, "\n> **Compacted summary:** %s\n", strings.TrimSpace(entry.Content))
		}
	}
	return b.String()
}

// commandExport writes the conversation to a Markdown file in the workspace.
func commandExport(ctx context.Context, c *config, args []string) error {
	entries, err := loadConversationHistoryEntries()
	if err != nil {
		return err
	}
	if len(historyEntriesSinceLastReset(entries)) == 0 {
		return errors.New("the conversation is empty")
	}
	name := "goat-conversation-" + time.Now().Format("20060102-150405") + ".md"
	if len(args) > 0 {
		name = strings.Join(args, " ")
	}
	resolved, relPath, err := workspace.ResolvePath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(resolved, []byte(transcriptMarkdown(entries, c.model.String())), 0o644); err != nil {
		return err
	}
	fmt.Println(formatSuccess("Exported the conversation to " + filepath.ToSlash(relPath) + "."))
	return nil
}
