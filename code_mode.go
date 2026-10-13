package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/tools"
)

func commandCodeMode(ctx context.Context, c *config, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "on":
			c.codeMode = true
		case "off":
			c.codeMode = false
		default:
			return fmt.Errorf("use /codemode, /codemode on or /codemode off")
		}
	} else {
		c.codeMode = !c.codeMode
	}
	if c.codeMode {
		fmt.Println(formatNotice("Code mode on: goat can batch tool calls in one run_code script. ") + style.Muted.Render("Scripts run inside goat, and a runaway one can still crash it; goat -c continues. /codemode again to turn it off."))
	} else {
		fmt.Println(formatSuccess("Code mode off: goat calls tools one at a time."))
	}
	return nil
}

// maxNestedCallsShown is how many of a script's calls are drawn; the rest are counted.
const maxNestedCallsShown = 20

// nestedRenderer draws the calls a run_code script makes, indented under the script's
// own header, while the script runs.
type nestedRenderer struct {
	shown, hidden int
}

const nestedIndent = "  "

func (r *nestedRenderer) Start(call provider.ToolCall) func(string, error) {
	started, waitedBefore := time.Now(), inputWaitTotal
	if r.shown >= maxNestedCallsShown {
		r.hidden++
		return func(string, error) {}
	}
	r.shown++
	width := toolRenderWidth() - len(nestedIndent)
	r.print(indentBlock(renderToolHeader(call, width), nestedIndent))
	return func(output string, err error) {
		elapsed := time.Since(started) - (inputWaitTotal - waitedBefore)
		first, _, _ := strings.Cut(renderToolResult(call, output, err, elapsed, width), "\n")
		r.print(nestedIndent + first)
	}
}

// finish notes the calls that were not drawn.
func (r *nestedRenderer) finish() {
	if r.hidden > 0 {
		g := style.Glyphs()
		r.print(nestedIndent + style.Subtle.Render(fmt.Sprintf("%s +%s not shown", g.Ellipsis, plural(r.hidden, "call"))))
	}
}

// print writes a line without drawing over the "Running" spinner.
func (r *nestedRenderer) print(line string) {
	resume := pauseSpinner()
	fmt.Fprintln(activityOut, line)
	resume()
}

var _ tools.NestedObserver = (*nestedRenderer)(nil)
