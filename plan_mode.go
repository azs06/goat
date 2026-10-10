package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/style"
)

// planMode, toggled with /plan or started with --plan, lets the agent read and
// investigate but not change anything: tools that would change files, and run_bash,
// are refused, and each prompt tells the model so. The tool list itself stays the same,
// so switching costs no prompt cache.
var planMode bool

// planModeNote goes in front of each prompt sent in plan mode.
const planModeNote = "[Plan mode is on: investigate with the read-only tools and answer with a plan. Do not change files or run commands; the user turns plan mode off to carry the plan out.]\n\n"

var errPlanMode = errors.New("plan mode is on, so this tool cannot run; describe the change in the plan instead")

// readOnlyTools never change anything.
var readOnlyTools = map[string]bool{"read_dir": true, "tree_dir": true, "read_file": true, "find_files": true, "grep_files": true}

// allowedInPlanMode reports whether a call leaves the workspace unchanged: a read-only
// tool, or a file tool asked only for a preview. run_bash is never allowed, since what
// a command does cannot be known in advance.
func allowedInPlanMode(call provider.ToolCall) bool {
	if readOnlyTools[call.Name] {
		return true
	}
	if call.Name == "run_bash" {
		return false
	}
	var args struct {
		Preview bool `json:"preview"`
	}
	return json.Unmarshal([]byte(call.Arguments), &args) == nil && args.Preview
}

func commandPlan(ctx context.Context, c *config, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "on":
			planMode = true
		case "off":
			planMode = false
		default:
			return fmt.Errorf("use /plan, /plan on or /plan off")
		}
	} else {
		planMode = !planMode
	}
	if planMode {
		fmt.Println(formatNotice("Plan mode on: goat reads and plans but changes nothing. ") + style.Muted.Render("/plan again to let it make changes."))
	} else {
		fmt.Println(formatSuccess("Plan mode off: goat can change files and run commands again."))
	}
	return nil
}
