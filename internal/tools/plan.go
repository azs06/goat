package tools

import (
	"encoding/json"
	"errors"

	"github.com/azs06/goat/internal/provider"
)

// ErrPlanMode is the error for a call plan mode refuses.
var ErrPlanMode = errors.New("plan mode is on, so this tool cannot run; describe the change in the plan instead")

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
