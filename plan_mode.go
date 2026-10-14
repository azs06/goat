package main

import (
	"context"
	"fmt"

	"github.com/azs06/goat/internal/style"
)

// planModeNote goes in front of each prompt sent in plan mode.
const planModeNote = "[Plan mode is on: investigate with the read-only tools and answer with a plan. Do not change files or run commands; the user turns plan mode off to carry the plan out.]\n\n"

// subagentReadOnlyNote goes in front of each prompt of a subagent that may not change
// anything: an explore agent, or a general one while main is in plan mode. It is not
// main's note: a subagent does not talk with the user, and owes a report, not a plan.
const subagentReadOnlyNote = "[Read-only: you can read, search and preview changes, but not change files or run commands. Do the task with that and report your findings.]\n\n"

// readOnlyNote is the note in front of a read-only turn's prompts.
func readOnlyNote(opts turnOptions) string {
	if opts.subagent {
		return subagentReadOnlyNote
	}
	return planModeNote
}

func commandPlan(ctx context.Context, c *config, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "on":
			c.planMode = true
		case "off":
			c.planMode = false
		default:
			return fmt.Errorf("use /plan, /plan on or /plan off")
		}
	} else {
		c.planMode = !c.planMode
	}
	if c.planMode {
		fmt.Println(formatNotice("Plan mode on: goat reads and plans but changes nothing. ") + style.Muted.Render("/plan again to let it make changes."))
	} else {
		fmt.Println(formatSuccess("Plan mode off: goat can change files and run commands again."))
	}
	return nil
}
