package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/workspace"
)

// Options controls how Execute runs a call.
type Options struct {
	// PlanMode refuses tools that change anything; read-only tools and previews run.
	PlanMode bool
	// CodeMode lets run_code run. Without it run_code is refused with ErrCodeModeOff.
	CodeMode bool
	// ApproveBash, when set, is asked before every run_bash command; a non-nil error
	// rejects the command.
	ApproveBash func(ctx context.Context, command, workdir string) error
	// CallID is the ID of the call being run; run_code names its calls after it.
	CallID string
	// Nested, when set, is told about each call a run_code script makes.
	Nested NestedObserver
}

// toolHandler runs one tool with its raw JSON arguments, already validated against the
// tool's schema.
type toolHandler func(ctx context.Context, rawArgs string, opts Options) (string, error)

// toolHandlers maps each tool in Specs to the code that runs it.
var toolHandlers = map[string]toolHandler{
	"read_dir":         runReadDir,
	"tree_dir":         runTreeDir,
	"read_file":        runReadFile,
	"write_file":       runWriteFile,
	"edit_file":        runEditFile,
	"edit_json_file":   runEditJSONFile,
	"copy_file":        transferTool("File", "copied", workspace.PreviewCopyFile, workspace.CopyFile),
	"move_file":        transferTool("File", "moved", workspace.PreviewMoveFile, workspace.MoveFile),
	"delete_file":      runDeleteFile,
	"copy_dir":         transferTool("Directory", "copied", workspace.PreviewCopyDir, workspace.CopyDir),
	"move_dir":         transferTool("Directory", "moved", workspace.PreviewMoveDir, workspace.MoveDir),
	"delete_dir":       runDeleteDir,
	"undo_last_change": runUndoLastChange,
	"run_bash":         runBash,
	"find_files":       runFindFiles,
	"grep_files":       runGrepFiles,
}

// Execute validates and runs one tool call. In plan mode only the read-only tools and
// previews run, run_bash commands go through opts.ApproveBash first, and run_code only
// runs in code mode.
func Execute(ctx context.Context, toolCall provider.ToolCall, opts Options) (string, error) {
	spec, ok := findSpec(toolCall.Name)
	handler := toolHandlers[toolCall.Name]
	if !ok || handler == nil {
		return "", fmt.Errorf("unsupported tool: %s", toolCall.Name)
	}
	if err := validateToolArguments(spec, toolCall.Arguments); err != nil {
		return "", err
	}
	if toolCall.Name == RunCode && !opts.CodeMode {
		return "", ErrCodeModeOff
	}
	if opts.PlanMode && !allowedInPlanMode(toolCall) {
		return "", ErrPlanMode
	}
	return handler(ctx, toolCall.Arguments, opts)
}

// decodeArgs decodes a tool's JSON arguments into a T.
func decodeArgs[T any](raw string) (T, error) {
	var args T
	err := decodeToolArguments(raw, &args)
	return args, err
}

func runReadDir(_ context.Context, raw string, _ Options) (string, error) {
	args, err := decodeArgs[struct {
		Path string `json:"path"`
	}](raw)
	if err != nil {
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
}

func runTreeDir(_ context.Context, raw string, _ Options) (string, error) {
	args, err := decodeArgs[struct {
		Path  string `json:"path"`
		Limit int    `json:"limit"`
	}](raw)
	if err != nil {
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
}

func runReadFile(_ context.Context, raw string, _ Options) (string, error) {
	args, err := decodeArgs[struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}](raw)
	if err != nil {
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
}

func runWriteFile(_ context.Context, raw string, _ Options) (string, error) {
	args, err := decodeArgs[struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Preview bool   `json:"preview"`
	}](raw)
	if err != nil {
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
}

func runEditFile(_ context.Context, raw string, _ Options) (string, error) {
	args, err := decodeArgs[struct {
		Path       string `json:"path"`
		OldText    string `json:"old_text"`
		NewText    string `json:"new_text"`
		ReplaceAll bool   `json:"replace_all"`
		Preview    bool   `json:"preview"`
	}](raw)
	if err != nil {
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
}

func runEditJSONFile(_ context.Context, raw string, _ Options) (string, error) {
	args, err := decodeArgs[struct {
		Path      string `json:"path"`
		JSONPath  string `json:"json_path"`
		Operation string `json:"operation"`
		ValueJSON string `json:"value_json"`
		Preview   bool   `json:"preview"`
	}](raw)
	if err != nil {
		return "", err
	}
	if args.Preview {
		return workspace.PreviewEditJSONFile(args.Path, args.JSONPath, args.Operation, args.ValueJSON)
	}
	if err := workspace.EditJSONFile(args.Path, args.JSONPath, args.Operation, args.ValueJSON); err != nil {
		return "", err
	}
	return fmt.Sprintf("JSON file %s updated successfully.", filepath.Clean(args.Path)), nil
}

type transferArgs struct {
	SourcePath      string `json:"source_path"`
	DestinationPath string `json:"destination_path"`
	Overwrite       bool   `json:"overwrite"`
	Preview         bool   `json:"preview"`
}

// transferTool builds the handler for a copy or move of a file or directory: noun is
// "File" or "Directory" and verb is how the success message says it went.
func transferTool(noun, verb string, preview func(source, destination string, overwrite bool) (string, error), transfer func(source, destination string, overwrite bool) error) toolHandler {
	return func(_ context.Context, raw string, _ Options) (string, error) {
		args, err := decodeArgs[transferArgs](raw)
		if err != nil {
			return "", err
		}
		if args.Preview {
			return preview(args.SourcePath, args.DestinationPath, args.Overwrite)
		}
		if err := transfer(args.SourcePath, args.DestinationPath, args.Overwrite); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s %s %s to %s successfully.", noun, filepath.Clean(args.SourcePath), verb, filepath.Clean(args.DestinationPath)), nil
	}
}

type deleteArgs struct {
	Path    string `json:"path"`
	Preview bool   `json:"preview"`
}

func runDeleteFile(_ context.Context, raw string, _ Options) (string, error) {
	args, err := decodeArgs[deleteArgs](raw)
	if err != nil {
		return "", err
	}
	if args.Preview {
		return workspace.PreviewDeleteFile(args.Path)
	}
	if err := workspace.DeleteFile(args.Path); err != nil {
		return "", err
	}
	return fmt.Sprintf("File %s deleted successfully.", filepath.Clean(args.Path)), nil
}

func runDeleteDir(_ context.Context, raw string, _ Options) (string, error) {
	args, err := decodeArgs[deleteArgs](raw)
	if err != nil {
		return "", err
	}
	if args.Preview {
		return workspace.PreviewDeleteDir(args.Path)
	}
	if err := workspace.DeleteDir(args.Path); err != nil {
		return "", err
	}
	return fmt.Sprintf("Directory %s deleted successfully.", filepath.Clean(args.Path)), nil
}

func runUndoLastChange(_ context.Context, raw string, _ Options) (string, error) {
	args, err := decodeArgs[struct {
		Preview bool `json:"preview"`
	}](raw)
	if err != nil {
		return "", err
	}
	if args.Preview {
		return workspace.PreviewUndoLastChange()
	}
	return workspace.UndoLastChange()
}

func runBash(ctx context.Context, raw string, opts Options) (string, error) {
	args, err := decodeArgs[struct {
		Command string `json:"command"`
		Workdir string `json:"workdir"`
	}](raw)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Command) == "" {
		return "", fmt.Errorf("missing command")
	}
	if opts.ApproveBash != nil {
		if err := opts.ApproveBash(ctx, args.Command, args.Workdir); err != nil {
			return "", err
		}
	}
	return workspace.RunBash(ctx, args.Command, args.Workdir)
}

func runFindFiles(_ context.Context, raw string, _ Options) (string, error) {
	args, err := decodeArgs[struct {
		Path           string `json:"path"`
		Pattern        string `json:"pattern"`
		Limit          int    `json:"limit"`
		IncludeIgnored bool   `json:"include_ignored"`
	}](raw)
	if err != nil {
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
}

func runGrepFiles(_ context.Context, raw string, _ Options) (string, error) {
	args, err := decodeArgs[struct {
		Path           string `json:"path"`
		Query          string `json:"query"`
		Regex          bool   `json:"regex"`
		Glob           string `json:"glob"`
		CaseSensitive  bool   `json:"case_sensitive"`
		Limit          int    `json:"limit"`
		IncludeIgnored bool   `json:"include_ignored"`
	}](raw)
	if err != nil {
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
}
