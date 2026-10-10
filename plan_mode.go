package main

import (
	"context"
	"fmt"

	"github.com/azs06/goat/internal/style"
)

// planModeNote goes in front of each prompt sent in plan mode.
const planModeNote = "[Plan mode is on: investigate with the read-only tools and answer with a plan. Do not change files or run commands; the user turns plan mode off to carry the plan out.]\n\n"

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
