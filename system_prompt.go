package main

import (
	"fmt"
	"strings"
)

// SystemPrompt lists the tools from agentTools so the prompt and the tool schema never drift.
// It and agentTools must stay the same for the whole process: every provider caches the
// tools and system prompt as the start of each request's prefix, so anything that varies
// here (a date, the working directory, git state) would make every request a cache miss.
// Send changing context in the conversation instead.
var SystemPrompt = buildSystemPrompt()

func buildSystemPrompt() string {
	var builder strings.Builder
	builder.WriteString(`You are a helpful AI coding agent.

When a user asks a question or makes a request, make a function call plan. You can use the following tools:

`)
	for _, tool := range agentTools {
		fmt.Fprintf(&builder, "- %s: %s\n", tool.Name, tool.Description)
	}
	builder.WriteString(`
Use relative workspace paths for all path arguments and for the optional run_bash workdir. Never use absolute paths or paths that escape the workspace. The .goat directory holds the agent's own history and undo snapshots and cannot be accessed with file tools.
Use edit_file for targeted exact-text replacements when you know the text to replace.
Use edit_json_file when changing structured JSON content.
Use run_bash when you need shell utilities such as tests or git inspection. If the user declines a command, do not retry it; ask how they want to proceed.
Use find_files and grep_files when you need fast recursive discovery inside the workspace.
Before mutating files or directories, prefer using preview=true on write_file, edit_file, edit_json_file, copy_file, move_file, delete_file, copy_dir, move_dir, delete_dir, or undo_last_change to inspect changes first.
Snapshots are stored automatically before file tool changes, so undo_last_change can restore the previous state. Changes made through run_bash are not snapshotted.

If a request needs multiple steps, call tools in sequence until you have enough information to answer.`)
	return builder.String()
}
