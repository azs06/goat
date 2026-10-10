package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/workspace"
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

func getTools() []provider.ToolSpec {
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
		functionTool("run_bash", "Run a bash command inside the workspace and return combined stdout and stderr. The user may be asked to approve each command. Commands time out after 30 seconds; start long-running processes in the background with their output redirected to a file. Prefer concise commands because large outputs may be truncated for token safety.",
			map[string]any{"command": stringParam, "workdir": stringParam}, "command"),
		functionTool("find_files", "Recursively find files within the workspace. Pattern can be a glob like *.go or a plain substring. Files .gitignore excludes are skipped unless include_ignored=true.",
			map[string]any{"path": stringParam, "pattern": stringParam, "limit": limitParam, "include_ignored": booleanParam}, "path"),
		functionTool("grep_files", "Recursively search inside files in the workspace and return matching lines as path:line:text. The query is plain text, or a Go regular expression with regex=true. glob limits the files searched (e.g. *.go, or cmd/*.go). Files .gitignore excludes are skipped unless include_ignored=true.",
			map[string]any{"path": stringParam, "query": stringParam, "regex": booleanParam, "glob": stringParam, "case_sensitive": booleanParam, "limit": limitParam, "include_ignored": booleanParam}, "path", "query"),
	}
}

var agentTools = getTools()

func availableToolNames() []string {
	names := make([]string, 0, len(agentTools))
	for _, tool := range agentTools {
		names = append(names, tool.Name)
	}
	return names
}

func findToolSpec(name string) (provider.ToolSpec, bool) {
	for _, tool := range agentTools {
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

	required, _ := spec.Parameters["required"].([]string)
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

// bashCommandApprover is asked before every run_bash command; a non-nil error rejects
// the command. main installs an interactive approver, and nil allows everything.
var bashCommandApprover func(ctx context.Context, command, workdir string) error

type transferArgs struct {
	SourcePath      string `json:"source_path"`
	DestinationPath string `json:"destination_path"`
	Overwrite       bool   `json:"overwrite"`
	Preview         bool   `json:"preview"`
}

func executeToolCall(ctx context.Context, toolCall provider.ToolCall) (string, error) {
	spec, ok := findToolSpec(toolCall.Name)
	if !ok {
		return "", fmt.Errorf("unsupported tool: %s", toolCall.Name)
	}
	if err := validateToolArguments(spec, toolCall.Arguments); err != nil {
		return "", err
	}
	if planMode && !allowedInPlanMode(toolCall) {
		return "", errPlanMode
	}

	switch toolCall.Name {
	case "read_dir":
		var args struct {
			Path string `json:"path"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		entries, err := workspace.ReadDir(args.Path)
		if err != nil {
			return "", err
		}
		if len(entries) == 0 {
			return fmt.Sprintf("Directory listing for %s:\n(empty)", filepath.Clean(args.Path)), nil
		}
		return fmt.Sprintf("Directory listing for %s:\n%s", filepath.Clean(args.Path), strings.Join(entries, "\n")), nil
	case "tree_dir":
		var args struct {
			Path  string `json:"path"`
			Limit int    `json:"limit"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		lines, truncated, err := workspace.TreeDir(args.Path, args.Limit)
		if err != nil {
			return "", err
		}
		message := fmt.Sprintf("Directory tree for %s:\n%s", filepath.Clean(args.Path), strings.Join(lines, "\n"))
		if truncated {
			message += fmt.Sprintf("\n(truncated after %d line(s))", len(lines))
		}
		return message, nil
	case "read_file":
		var args struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Offset > 0 || args.Limit > 0 {
			text, first, last, total, err := workspace.ReadFileLines(args.Path, args.Offset, args.Limit)
			if err != nil {
				return "", err
			}
			if total == 0 {
				return fmt.Sprintf("File contents for %s:\n", filepath.Clean(args.Path)), nil
			}
			return fmt.Sprintf("File contents for %s (lines %d-%d of %d):\n%s", filepath.Clean(args.Path), first, last, total, text), nil
		}
		content, err := workspace.ReadFileForModel(args.Path)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("File contents for %s:\n%s", filepath.Clean(args.Path), content), nil
	case "write_file":
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			Preview bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if strings.TrimSpace(args.Path) == "" {
			return "", fmt.Errorf("missing path")
		}
		if args.Preview {
			return workspace.PreviewWriteFile(args.Path, args.Content)
		}
		if err := workspace.WriteFile(args.Path, args.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s written successfully.", filepath.Clean(args.Path)), nil
	case "edit_file":
		var args struct {
			Path       string `json:"path"`
			OldText    string `json:"old_text"`
			NewText    string `json:"new_text"`
			ReplaceAll bool   `json:"replace_all"`
			Preview    bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return workspace.PreviewEditFile(args.Path, args.OldText, args.NewText, args.ReplaceAll)
		}
		replaced, err := workspace.EditFile(args.Path, args.OldText, args.NewText, args.ReplaceAll)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s edited successfully. Replaced %d occurrence(s).", filepath.Clean(args.Path), replaced), nil
	case "edit_json_file":
		var args struct {
			Path      string `json:"path"`
			JSONPath  string `json:"json_path"`
			Operation string `json:"operation"`
			ValueJSON string `json:"value_json"`
			Preview   bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return workspace.PreviewEditJSONFile(args.Path, args.JSONPath, args.Operation, args.ValueJSON)
		}
		if err := workspace.EditJSONFile(args.Path, args.JSONPath, args.Operation, args.ValueJSON); err != nil {
			return "", err
		}
		return fmt.Sprintf("JSON file %s updated successfully.", filepath.Clean(args.Path)), nil
	case "copy_file":
		var args transferArgs
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return workspace.PreviewCopyFile(args.SourcePath, args.DestinationPath, args.Overwrite)
		}
		if err := workspace.CopyFile(args.SourcePath, args.DestinationPath, args.Overwrite); err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s copied to %s successfully.", filepath.Clean(args.SourcePath), filepath.Clean(args.DestinationPath)), nil
	case "move_file":
		var args transferArgs
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return workspace.PreviewMoveFile(args.SourcePath, args.DestinationPath, args.Overwrite)
		}
		if err := workspace.MoveFile(args.SourcePath, args.DestinationPath, args.Overwrite); err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s moved to %s successfully.", filepath.Clean(args.SourcePath), filepath.Clean(args.DestinationPath)), nil
	case "delete_file":
		var args struct {
			Path    string `json:"path"`
			Preview bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return workspace.PreviewDeleteFile(args.Path)
		}
		if err := workspace.DeleteFile(args.Path); err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s deleted successfully.", filepath.Clean(args.Path)), nil
	case "copy_dir":
		var args transferArgs
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return workspace.PreviewCopyDir(args.SourcePath, args.DestinationPath, args.Overwrite)
		}
		if err := workspace.CopyDir(args.SourcePath, args.DestinationPath, args.Overwrite); err != nil {
			return "", err
		}
		return fmt.Sprintf("Directory %s copied to %s successfully.", filepath.Clean(args.SourcePath), filepath.Clean(args.DestinationPath)), nil
	case "move_dir":
		var args transferArgs
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return workspace.PreviewMoveDir(args.SourcePath, args.DestinationPath, args.Overwrite)
		}
		if err := workspace.MoveDir(args.SourcePath, args.DestinationPath, args.Overwrite); err != nil {
			return "", err
		}
		return fmt.Sprintf("Directory %s moved to %s successfully.", filepath.Clean(args.SourcePath), filepath.Clean(args.DestinationPath)), nil
	case "delete_dir":
		var args struct {
			Path    string `json:"path"`
			Preview bool   `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return workspace.PreviewDeleteDir(args.Path)
		}
		if err := workspace.DeleteDir(args.Path); err != nil {
			return "", err
		}
		return fmt.Sprintf("Directory %s deleted successfully.", filepath.Clean(args.Path)), nil
	case "undo_last_change":
		var args struct {
			Preview bool `json:"preview"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if args.Preview {
			return workspace.PreviewUndoLastChange()
		}
		return workspace.UndoLastChange()
	case "run_bash":
		var args struct {
			Command string `json:"command"`
			Workdir string `json:"workdir"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		if strings.TrimSpace(args.Command) == "" {
			return "", fmt.Errorf("missing command")
		}
		if bashCommandApprover != nil {
			if err := bashCommandApprover(ctx, args.Command, args.Workdir); err != nil {
				return "", err
			}
		}
		return workspace.RunBash(ctx, args.Command, args.Workdir)
	case "find_files":
		var args struct {
			Path           string `json:"path"`
			Pattern        string `json:"pattern"`
			Limit          int    `json:"limit"`
			IncludeIgnored bool   `json:"include_ignored"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		matches, truncated, err := workspace.FindFiles(args.Path, args.Pattern, args.Limit, args.IncludeIgnored)
		if err != nil {
			return "", err
		}
		if len(matches) == 0 {
			if strings.TrimSpace(args.Pattern) == "" {
				return fmt.Sprintf("No files found under %s.", filepath.Clean(args.Path)), nil
			}
			return fmt.Sprintf("No files found for pattern %q under %s.", args.Pattern, filepath.Clean(args.Path)), nil
		}

		message := fmt.Sprintf("Found %d file(s) under %s:\n%s", len(matches), filepath.Clean(args.Path), strings.Join(matches, "\n"))
		if truncated {
			message += fmt.Sprintf("\n(truncated after %d result(s))", len(matches))
		}
		return message, nil
	case "grep_files":
		var args struct {
			Path           string `json:"path"`
			Query          string `json:"query"`
			Regex          bool   `json:"regex"`
			Glob           string `json:"glob"`
			CaseSensitive  bool   `json:"case_sensitive"`
			Limit          int    `json:"limit"`
			IncludeIgnored bool   `json:"include_ignored"`
		}
		if err := decodeToolArguments(toolCall.Arguments, &args); err != nil {
			return "", err
		}
		matches, truncated, err := workspace.GrepFiles(args.Path, workspace.GrepOptions{Query: args.Query, Regex: args.Regex, Glob: args.Glob, CaseSensitive: args.CaseSensitive, Limit: args.Limit, IncludeIgnored: args.IncludeIgnored})
		if err != nil {
			return "", err
		}
		if len(matches) == 0 {
			return fmt.Sprintf("No matches found for %q under %s.", args.Query, filepath.Clean(args.Path)), nil
		}

		message := fmt.Sprintf("Found %d match(es) for %q under %s:\n%s", len(matches), args.Query, filepath.Clean(args.Path), strings.Join(matches, "\n"))
		if truncated {
			message += fmt.Sprintf("\n(truncated after %d match(es))", len(matches))
		}
		return message, nil
	default:
		return "", fmt.Errorf("unsupported tool: %s", toolCall.Name)
	}
}
