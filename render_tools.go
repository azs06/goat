package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/azs06/goat/internal/provider"
)

// Tool calls render as Claude Code style blocks: a bulleted header naming the tool and
// its main argument, then the result hanging under a "⎿" connector. Everything here
// only formats text for the terminal; what the model sees is never styled.

const (
	toolResultPreviewLines = 5
	toolDiffPreviewLines   = 20
)

// toolDisplayName is the friendly name shown for a tool, falling back to its raw name.
func toolDisplayName(name string) string {
	switch name {
	case "read_file":
		return "Read"
	case "write_file":
		return "Write"
	case "edit_file", "edit_json_file":
		return "Update"
	case "run_bash":
		return "Bash"
	case "grep_files":
		return "Grep"
	case "find_files":
		return "Find"
	case "read_dir":
		return "List"
	case "tree_dir":
		return "Tree"
	case "copy_file", "copy_dir":
		return "Copy"
	case "move_file", "move_dir":
		return "Move"
	case "delete_file", "delete_dir":
		return "Delete"
	case "undo_last_change":
		return "Undo"
	default:
		return name
	}
}

// toolArgSummary returns a one-line summary of a call's main arguments and its notable
// flags. Unknown tools and malformed arguments fall back to the compacted raw arguments.
func toolArgSummary(name, rawArgs string) (string, []string) {
	var args map[string]any
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil || args == nil {
		return oneLine(rawArgs), nil
	}
	str := func(key string) string {
		value, _ := args[key].(string)
		return value
	}
	var flags []string
	flag := func(key string) {
		if value, _ := args[key].(bool); value {
			flags = append(flags, strings.ReplaceAll(key, "_", " "))
		}
	}
	limit := func() {
		if value, ok := args["limit"].(float64); ok && value > 0 {
			flags = append(flags, "limit "+strconv.FormatFloat(value, 'f', -1, 64))
		}
	}

	var summary string
	switch name {
	case "read_file", "read_dir", "delete_file", "delete_dir":
		summary = str("path")
	case "tree_dir":
		summary = str("path")
		limit()
	case "write_file":
		summary = str("path")
	case "edit_file":
		summary = str("path")
		flag("replace_all")
	case "edit_json_file":
		summary = str("path")
		if jsonPath := str("json_path"); jsonPath != "" {
			flags = append(flags, strings.TrimSpace(str("operation")+" "+jsonPath))
		}
	case "copy_file", "move_file", "copy_dir", "move_dir":
		summary = str("source_path") + " " + glyphs().arrow + " " + str("destination_path")
		flag("overwrite")
	case "undo_last_change":
	case "run_bash":
		summary = str("command")
		if workdir := str("workdir"); workdir != "" && workdir != "." {
			flags = append(flags, "in "+workdir)
		}
	case "find_files":
		summary = str("path")
		if pattern := str("pattern"); pattern != "" {
			summary = pattern + " in " + summary
		}
		limit()
	case "grep_files":
		summary = strconv.Quote(str("query")) + " in " + str("path")
		flag("case_sensitive")
		limit()
	default:
		return oneLine(rawArgs), nil
	}
	flag("preview")
	return oneLine(summary), flags
}

// oneLine collapses whitespace runs, including newlines, to single spaces.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// renderToolHeader is the "● Name(summary)" line for a tool call, fitted to width.
func renderToolHeader(call provider.ToolCall, width int) string {
	g := glyphs()
	name := escapeControlChars(toolDisplayName(call.Name))
	summary, flags := toolArgSummary(call.Name, call.Arguments)
	summary = escapeControlChars(summary)
	flagText := ""
	if len(flags) > 0 {
		// Flags get at most half the width, so the main argument always has room.
		flagText = truncateToWidth(escapeControlChars(strings.Join(flags, ", ")), width/2)
		if colorEnabled {
			flagText = " " + g.dot + " " + flagText
		} else {
			flagText = " [" + flagText + "]"
		}
	}

	fixed := len([]rune(g.bullet)) + 1 + len([]rune(name)) + len([]rune(flagText))
	if summary != "" {
		fixed += 2
		summary = truncateToWidth(summary, width-fixed)
	}

	line := styleAccent(g.bullet) + " " + styleBold(name)
	if summary != "" {
		line += styleDim("(") + summary + styleDim(")")
	}
	return line + styleSubtle(flagText)
}

// renderToolResult is the block shown under a tool header once the call finishes.
func renderToolResult(call provider.ToolCall, output string, err error, elapsed time.Duration, width int) string {
	var first string
	var body []string
	var diff bool
	interrupted := errors.Is(err, context.Canceled)
	if interrupted {
		first = "Interrupted"
	} else if err != nil {
		message, rest, _ := strings.Cut(escapeControlChars(err.Error()), "\n")
		first = "Error: " + message
		if rest != "" {
			if _, bashBody, isBash := cutToolOutputHeader(rest, "Bash output for "); isBash {
				rest = bashBody
			}
			body = strings.Split(rest, "\n")
		}
	} else {
		first, body, diff = summarizeToolOutput(call, escapeControlChars(output))
	}

	suffix := ""
	if elapsed >= time.Second {
		// Plain output brackets the time so it cannot be read as the tool's own output.
		if colorEnabled {
			suffix = fmt.Sprintf(" %s %.1fs", glyphs().dot, elapsed.Seconds())
		} else {
			suffix = fmt.Sprintf(" [%.1fs]", elapsed.Seconds())
		}
	}
	first = truncateToWidth(first, width-visibleWidth(toolResultLead())-visibleWidth(suffix))
	switch {
	case interrupted:
		first = styleWarn(first)
	case err != nil:
		first = styleError(first)
	}
	first += styleSubtle(suffix)

	limit := toolResultPreviewLines
	if diff {
		limit = toolDiffPreviewLines
	}
	return layoutToolResult(first, body, limit, diff, width)
}

// toolResultLead and toolResultIndent prefix the first and following lines of a result.
func toolResultLead() string {
	if !colorEnabled {
		return "  " + glyphs().result + " "
	}
	return "  " + glyphs().result + "  "
}

func toolResultIndent() string {
	if !colorEnabled {
		return toolResultLead()
	}
	return strings.Repeat(" ", visibleWidth(toolResultLead()))
}

// summarizeToolOutput turns a successful tool output into a short first line and the
// body lines worth showing under it; diff reports whether the body is a diff.
func summarizeToolOutput(call provider.ToolCall, output string) (string, []string, bool) {
	header, rest, _ := strings.Cut(output, "\n")

	if strings.HasPrefix(header, "Preview for ") {
		if call.Name == "edit_file" {
			if replacements, after, found := strings.Cut(rest, "\n"); found && strings.HasPrefix(replacements, "Replacements: ") {
				rest = after
			}
		}
		return "Preview, no changes made", strings.Split(rest, "\n"), true
	}

	switch call.Name {
	case "read_file":
		if strings.HasPrefix(header, "File contents for ") {
			return "Read " + plural(countLines(rest), "line"), nil, false
		}
	case "read_dir", "tree_dir":
		if strings.HasPrefix(header, "Directory listing for ") || strings.HasPrefix(header, "Directory tree for ") {
			lines := strings.Split(rest, "\n")
			if rest == "(empty)" {
				return "Empty directory", nil, false
			}
			noun := "entry"
			if call.Name == "tree_dir" {
				noun = "line"
			}
			// A truncated tree ends with a "(truncated after N line(s))" note, which is
			// not itself a listed line.
			count := len(lines)
			if strings.HasPrefix(lines[count-1], "(truncated after ") {
				count--
			}
			return "Listed " + plural(count, noun), lines, false
		}
	case "run_bash":
		if strings.HasPrefix(header, "Bash output for ") {
			lines := strings.Split(rest, "\n")
			return lines[0], lines[1:], false
		}
	case "find_files", "grep_files":
		if strings.HasPrefix(header, "Found ") {
			if i := strings.Index(header, " under "); i > 0 && call.Name == "find_files" {
				header = header[:i]
			}
			return strings.TrimSuffix(header, ":"), strings.Split(rest, "\n"), false
		}
	case "edit_file":
		if lines := editDiffLines(call.Arguments); lines != nil && strings.HasPrefix(header, "File ") {
			return editSummary(header, lines), lines, true
		}
	case "write_file":
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if json.Unmarshal([]byte(call.Arguments), &args) == nil && strings.HasPrefix(header, "File ") {
			return fmt.Sprintf("Wrote %s to %s", plural(countLines(args.Content), "line"), escapeControlChars(args.Path)), nil, false
		}
	}

	if rest == "" {
		return header, nil, false
	}
	return header, strings.Split(rest, "\n"), false
}

// cutToolOutputHeader splits off a first line starting with prefix.
func cutToolOutputHeader(text, prefix string) (string, string, bool) {
	header, rest, _ := strings.Cut(text, "\n")
	if !strings.HasPrefix(header, prefix) {
		return "", text, false
	}
	return header, rest, true
}

// editDiffLines renders an applied edit_file call's old and new text as diff lines.
func editDiffLines(rawArgs string) []string {
	var args struct {
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
	}
	if json.Unmarshal([]byte(rawArgs), &args) != nil || args.OldText == args.NewText {
		return nil
	}
	ops := computeDiffOps(splitDiffLines(args.OldText), splitDiffLines(args.NewText))
	lines := make([]string, 0, len(ops))
	for _, op := range ops {
		lines = append(lines, string(op.kind)+escapeControlChars(strings.TrimSuffix(op.text, "\n")))
	}
	return lines
}

// editSummary describes an applied edit as "Added 3 lines, removed 1 line".
func editSummary(header string, lines []string) string {
	added, removed := 0, 0
	for _, line := range lines {
		switch line[0] {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	summary := "Added " + plural(added, "line") + ", removed " + plural(removed, "line")
	var replaced int
	if i := strings.Index(header, "Replaced "); i >= 0 {
		if _, err := fmt.Sscanf(header[i:], "Replaced %d", &replaced); err == nil && replaced > 1 {
			summary += fmt.Sprintf(" in %d places", replaced)
		}
	}
	return summary
}

func countLines(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(text, "\n"), "\n") + 1
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if strings.HasSuffix(noun, "y") {
		return fmt.Sprintf("%d %sies", n, strings.TrimSuffix(noun, "y"))
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// layoutToolResult hangs first and up to limit body lines under the result connector,
// clipping each line to width and noting how many lines were left out.
func layoutToolResult(first string, body []string, limit int, diff bool, width int) string {
	g := glyphs()
	indent := toolResultIndent()
	room := width - visibleWidth(indent)

	for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
		body = body[:len(body)-1]
	}
	hidden := 0
	if len(body) > limit {
		hidden = len(body) - limit
		body = body[:limit]
	}

	var b strings.Builder
	b.WriteString(styleSubtle(toolResultLead()) + first)
	for _, line := range body {
		line = truncateToWidth(strings.ReplaceAll(line, "\t", "    "), room)
		if diff {
			line = styleDiffLine(line)
		} else {
			line = styleDim(line)
		}
		b.WriteString("\n" + styleSubtle(indent) + line)
	}
	if hidden > 0 {
		b.WriteString("\n" + styleSubtle(indent) + styleSubtle(fmt.Sprintf("%s +%s", g.ellipsis, plural(hidden, "line"))))
	}
	return b.String()
}

// styleDiffLine colors one diff line: additions green, removals red, file labels and
// truncation markers subtle, context dim.
func styleDiffLine(line string) string {
	switch {
	case strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "..."):
		return styleSubtle(line)
	case strings.HasPrefix(line, "+"):
		return styleSuccess(line)
	case strings.HasPrefix(line, "-"):
		return styleError(line)
	default:
		return styleDim(line)
	}
}
