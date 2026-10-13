// Package tools defines the tools the model can call, their JSON schemas, and the
// handlers that run them against the workspace.
package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/azs06/goat/internal/provider"
)

var (
	stringParam  = map[string]any{"type": "string"}
	booleanParam = map[string]any{"type": "boolean"}
	limitParam   = map[string]any{"type": "integer", "minimum": 1}
)

func functionTool(name, description string, properties map[string]any, required ...string) provider.ToolSpec {
	parameters := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		parameters["required"] = required
	}

	return provider.ToolSpec{Name: name, Description: description, Parameters: parameters}
}

func buildSpecs() []provider.ToolSpec {
	transferProperties := map[string]any{
		"source_path":      stringParam,
		"destination_path": stringParam,
		"overwrite":        booleanParam,
		"preview":          booleanParam,
	}

	return []provider.ToolSpec{
		functionTool("read_dir", "List files and directories for a relative path in the workspace.",
			map[string]any{"path": stringParam}, "path"),
		functionTool("tree_dir", "Render a recursive directory tree for a relative workspace path. Paths .gitignore excludes are left out.",
			map[string]any{"path": stringParam, "limit": limitParam}, "path"),
		functionTool("read_file", "Read a file from a relative path in the workspace. For long files pass offset (first line, starting at 1) and limit (number of lines) to read part of it; whole files over 1 MiB are refused.",
			map[string]any{"path": stringParam, "offset": limitParam, "limit": limitParam}, "path"),
		functionTool("write_file", "Write or overwrite a file at a relative path in the workspace with provided content. Set preview=true to get a diff without changing the file.",
			map[string]any{"path": stringParam, "content": stringParam, "preview": booleanParam}, "path", "content"),
		functionTool("edit_file", "Edit an existing workspace file by replacing exact text. Set preview=true to get a diff without changing the file.",
			map[string]any{"path": stringParam, "old_text": stringParam, "new_text": stringParam, "replace_all": booleanParam, "preview": booleanParam}, "path", "old_text", "new_text"),
		functionTool("edit_json_file", "Edit a JSON file by setting or deleting a value at a JSON path. Use value_json for set operations and preview=true to inspect the diff first.",
			map[string]any{"path": stringParam, "json_path": stringParam, "operation": map[string]any{"type": "string", "enum": []string{"set", "delete"}}, "value_json": stringParam, "preview": booleanParam}, "path", "json_path", "operation"),
		functionTool("copy_file", "Copy a file to another relative workspace path. Set preview=true to inspect the destination diff first.",
			transferProperties, "source_path", "destination_path"),
		functionTool("move_file", "Move a file to another relative workspace path. Set preview=true to inspect the destination diff first.",
			transferProperties, "source_path", "destination_path"),
		functionTool("delete_file", "Delete a file from the workspace. Set preview=true to inspect a deletion diff first.",
			map[string]any{"path": stringParam, "preview": booleanParam}, "path"),
		functionTool("copy_dir", "Copy a directory to another relative workspace path. Set preview=true to inspect the destination changes first.",
			transferProperties, "source_path", "destination_path"),
		functionTool("move_dir", "Move a directory to another relative workspace path. Set preview=true to inspect the changes first.",
			transferProperties, "source_path", "destination_path"),
		functionTool("delete_dir", "Delete a directory from the workspace (not the workspace root). Set preview=true to inspect the removal first.",
			map[string]any{"path": stringParam, "preview": booleanParam}, "path"),
		functionTool("undo_last_change", "Undo the most recent mutating file or directory operation using saved snapshots. Set preview=true to inspect what would be restored first.",
			map[string]any{"preview": booleanParam}),
		functionTool("run_bash", "Run a bash command inside the workspace and return combined stdout and stderr. The user may be asked to approve each command. Commands time out after 30 seconds unless timeout (seconds, up to 600) allows longer, as a build or test suite may need; start servers and other long-running processes in the background with their output redirected to a file. Prefer concise commands because large outputs may be truncated for token safety.",
			map[string]any{"command": stringParam, "workdir": stringParam, "timeout": map[string]any{"type": "integer", "minimum": 1, "maximum": maxBashTimeoutSeconds}}, "command"),
		functionTool("find_files", "Recursively find files within the workspace. Pattern can be a glob like *.go or a plain substring. Files .gitignore excludes are skipped unless include_ignored=true.",
			map[string]any{"path": stringParam, "pattern": stringParam, "limit": limitParam, "include_ignored": booleanParam}, "path"),
		functionTool("grep_files", "Recursively search inside files in the workspace and return matching lines as path:line:text. The query is plain text, or a Go regular expression with regex=true. glob limits the files searched (e.g. *.go, or cmd/*.go). Files .gitignore excludes are skipped unless include_ignored=true.",
			map[string]any{"path": stringParam, "query": stringParam, "regex": booleanParam, "glob": stringParam, "case_sensitive": booleanParam, "limit": limitParam, "include_ignored": booleanParam}, "path", "query"),
		functionTool("read_output", "Read a tool output that was shortened for you, in full or a part at a time. output is the number in the note that says it was shortened; offset (first line, starting at 1) and limit (number of lines) pick a part.",
			map[string]any{"output": limitParam, "offset": limitParam, "limit": limitParam}, "output"),
	}
}

// Specs are the tools offered to the model, in the order they are listed. Code mode
// offers CodeModeSpecs instead.
var Specs = append(buildSpecs(), tuiSpecs()...)

// Names returns the name of every tool in Specs.
func Names() []string {
	names := make([]string, 0, len(Specs))
	for _, tool := range Specs {
		names = append(names, tool.Name)
	}
	return names
}

// RequiredArguments returns the names of the arguments a tool requires, in the order
// its schema lists them.
func RequiredArguments(spec provider.ToolSpec) []string {
	required, _ := spec.Parameters["required"].([]string)
	return required
}

func findSpec(name string) (provider.ToolSpec, bool) {
	for _, tool := range CodeModeSpecs {
		if tool.Name == name {
			return tool, true
		}
	}
	return provider.ToolSpec{}, false
}

// validateToolArguments checks that raw is a JSON object holding every required
// argument, so a truncated or malformed call never runs with zero values.
func validateToolArguments(spec provider.ToolSpec, raw string) error {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	var arguments map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &arguments); err != nil {
		return fmt.Errorf("tool arguments are not a valid JSON object: %w", err)
	}

	required := RequiredArguments(spec)
	var missing []string
	for _, name := range required {
		if value, ok := arguments[name]; !ok || string(value) == "null" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required argument(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

func decodeToolArguments(raw string, target any) error {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("decode tool arguments: %w", err)
	}
	return nil
}
