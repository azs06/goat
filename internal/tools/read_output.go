package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// OutputSource finds the conversation's nth tool output, counting from 1 at the start
// of the conversation: what the tool returned, as goat recorded it, and a title naming
// the call, such as "Bash(go test ./...)".
type OutputSource func(n int) (title, content string, err error)

// readOutputCharBudget keeps one read_output result inside the budget the model's tool
// results are cut to, so reading a shortened output never shortens it again.
const readOutputCharBudget = 12000

// readOutputDefaultLines is how many lines read_output returns without a limit.
const readOutputDefaultLines = 300

// ReadOutputMaxLines is the most lines one read_output result holds, whatever limit the
// model asks for. With the header and the "more follows" line it stays under the line
// budget the model's tool results are cut to (400, in agent.go), so a part is never cut
// again and the offset it suggests never skips lines.
const ReadOutputMaxLines = 390

// ShortenedNote ends a tool result goat cut down for the model, telling it how to read
// the rest. n is the output's number for read_output.
func ShortenedNote(n int) string {
	return fmt.Sprintf("\n[This output was shortened. read_output with output %d shows all of it, a part at a time.]", n)
}

func runReadOutput(_ context.Context, raw string, opts Options) (string, error) {
	args, err := decodeArgs[struct {
		Output int `json:"output"`
		Offset int `json:"offset"`
		Limit  int `json:"limit"`
	}](raw)
	if err != nil {
		return "", err
	}
	if opts.Outputs == nil {
		return "", errors.New("tool outputs are not kept in this session")
	}
	if args.Output < 1 {
		return "", errors.New("output must be 1 or more")
	}
	title, content, err := opts.Outputs(args.Output)
	if err != nil {
		return "", err
	}

	lines := strings.Split(content, "\n")
	first := max(args.Offset, 1)
	if first > len(lines) {
		return "", fmt.Errorf("output %d has %d line(s); offset %d is past the end", args.Output, len(lines), first)
	}
	limit := args.Limit
	if limit <= 0 {
		limit = readOutputDefaultLines
	}
	limit = min(limit, ReadOutputMaxLines)

	// Stop early rather than overrun the budget; the model asks for the next part.
	var b strings.Builder
	last := first - 1
	for last < len(lines) && last-first+1 < limit {
		line := lines[last]
		if b.Len() > 0 && b.Len()+len(line)+1 > readOutputCharBudget {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		if len(line) > readOutputCharBudget {
			line = line[:readOutputCharBudget] + fmt.Sprintf(" [... line cut after %d bytes ...]", readOutputCharBudget)
		}
		b.WriteString(line)
		last++
	}

	text := fmt.Sprintf("Output %d, %s (lines %d-%d of %d):\n%s", args.Output, title, first, last, len(lines), b.String())
	if last < len(lines) {
		text += fmt.Sprintf("\n(more follows; continue with offset %d)", last+1)
	}
	return text, nil
}
