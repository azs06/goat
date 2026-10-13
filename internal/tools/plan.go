package tools

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/azs06/goat/internal/provider"
)

// ErrPlanMode is the error for a call plan mode refuses.
var ErrPlanMode = errors.New("plan mode is on, so this tool cannot run; describe the change in the plan instead")

// readOnlyTools never change anything.
var readOnlyTools = map[string]bool{"read_dir": true, "tree_dir": true, "read_file": true, "find_files": true, "grep_files": true, "read_output": true}

// PlanModePolicy is what plan mode does with calls to a tool.
type PlanModePolicy int

const (
	// PlanModeAllowed tools only read, so plan mode runs them as they are.
	PlanModeAllowed PlanModePolicy = iota
	// PlanModePreview tools change the workspace, so in plan mode they run only when
	// the call asks for a preview.
	PlanModePreview
	// PlanModeBlocked tools never run in plan mode; what run_bash does cannot be known
	// in advance.
	PlanModeBlocked
	// PlanModePerCall tools run in plan mode, and each call they make is checked on
	// its own: run_code scripts can read and preview, but not change anything.
	PlanModePerCall
)

func (p PlanModePolicy) String() string {
	switch p {
	case PlanModeAllowed:
		return "allowed"
	case PlanModePreview:
		return "preview"
	case PlanModeBlocked:
		return "blocked"
	case PlanModePerCall:
		return "per call"
	}
	return fmt.Sprintf("PlanModePolicy(%d)", int(p))
}

// PlanModePolicyFor reports what plan mode does with calls to the named tool. Names
// outside Specs get PlanModePreview.
func PlanModePolicyFor(name string) PlanModePolicy {
	if readOnlyTools[name] {
		return PlanModeAllowed
	}
	switch name {
	case "run_bash":
		return PlanModeBlocked
	case RunCode:
		return PlanModePerCall
	}
	return PlanModePreview
}

// allowedInPlanMode reports whether a call leaves the workspace unchanged: a read-only
// tool, or a file tool asked only for a preview. run_bash is never allowed, since what
// a command does cannot be known in advance. run_code is, because the calls its script
// makes are checked one by one.
func allowedInPlanMode(call provider.ToolCall) bool {
	switch PlanModePolicyFor(call.Name) {
	case PlanModeAllowed, PlanModePerCall:
		return true
	case PlanModeBlocked:
		return false
	}
	var args struct {
		Preview bool `json:"preview"`
	}
	return json.Unmarshal([]byte(call.Arguments), &args) == nil && args.Preview
}
