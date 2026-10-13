package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/workspace"
)

// RunTUI is the tool that runs a program in an emulated terminal and returns its screen.
// It is offered only where workspace.TUIAvailable.
const RunTUI = "run_tui"

func init() {
	toolHandlers[RunTUI] = runTUI
}

func runTUISpec() provider.ToolSpec {
	step := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"keys":     map[string]any{"type": "string", "description": "Text to type; special keys as <Enter>, <Tab>, <Esc>, <Backspace>, <Up>, <Down>, <Left>, <Right>, <PgUp>, <PgDn>, <F1>-<F12>, <C-c>, <M-x>, and <lt> for <."},
			"wait_for": map[string]any{"type": "string", "description": "Then wait until the screen shows this text (up to wait_ms, default 10000)."},
			"wait_ms":  map[string]any{"type": "integer", "minimum": 0, "maximum": 30000, "description": "Then wait this long, or this long at most for wait_for."},
			"snapshot": map[string]any{"type": "boolean", "description": "Also return the screen after this step."},
		},
		"additionalProperties": false,
	}
	return functionTool(RunTUI, "Run a command in an emulated terminal, type keys into it step by step, and get the screen back as text, to check terminal UIs, goat's own included. goat waits for output to settle before typing and before the final screen. Approved like run_bash; stopped after 60 seconds, and whatever still runs after the steps is closed. The program gets a scratch XDG_STATE_HOME.",
		map[string]any{
			"command": stringParam,
			"workdir": stringParam,
			"cols":    map[string]any{"type": "integer", "minimum": 20, "maximum": 300, "description": "Default 100."},
			"rows":    map[string]any{"type": "integer", "minimum": 5, "maximum": 100, "description": "Default 30."},
			"steps":   map[string]any{"type": "array", "items": step},
		}, "command")
}

// tuiSpecs are the run_tui spec where it can run, and nothing elsewhere.
func tuiSpecs() []provider.ToolSpec {
	if !workspace.TUIAvailable {
		return nil
	}
	return []provider.ToolSpec{runTUISpec()}
}

func runTUI(ctx context.Context, raw string, opts Options) (string, error) {
	args, err := decodeArgs[struct {
		Command string          `json:"command"`
		Workdir string          `json:"workdir"`
		Cols    int             `json:"cols"`
		Rows    int             `json:"rows"`
		Steps   json.RawMessage `json:"steps"`
	}](raw)
	if err != nil {
		return "", err
	}
	// A misspelled step field would make a step do nothing and the check pass, so steps
	// are decoded strictly.
	var steps []workspace.TUIStep
	if len(args.Steps) > 0 && string(args.Steps) != "null" {
		decoder := json.NewDecoder(bytes.NewReader(args.Steps))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&steps); err != nil {
			return "", fmt.Errorf("steps: %w (each step takes keys, wait_for, wait_ms and snapshot)", err)
		}
	}
	run := workspace.TUIRun{Command: strings.TrimSpace(args.Command), Workdir: args.Workdir, Cols: args.Cols, Rows: args.Rows, Steps: steps}
	if err := workspace.CheckTUIRun(run); err != nil {
		return "", err
	}
	if opts.ApproveBash != nil {
		if err := opts.ApproveBash(ctx, workspace.TUIApprovalText(run.Command, run.Steps), args.Workdir); err != nil {
			return "", err
		}
	}
	return workspace.RunTUI(ctx, run)
}
