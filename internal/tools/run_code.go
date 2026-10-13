package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/azs06/goat/internal/codemode"
	"github.com/azs06/goat/internal/provider"
)

// run_code runs a JavaScript script that calls the other tools as goat.<name>(args).
// Every call goes back through Execute, so plan mode, run_bash approval and the
// workspace checks apply to it as they do to a direct call. It is offered to the model
// only in code mode, and refuses to run outside it.

// RunCode is the name of the code-mode tool.
const RunCode = "run_code"

// ErrCodeModeOff is the error for run_code when code mode is off.
var ErrCodeModeOff = errors.New("code mode is off, so run_code cannot run; call the tools directly (the user can turn code mode on with /codemode)")

// NestedObserver is told about every call a run_code script makes, so the caller can
// show it. Start is called before the call runs and the function it returns after.
type NestedObserver interface {
	Start(call provider.ToolCall) (done func(output string, err error))
}

func init() {
	// Registered here rather than in the toolHandlers literal: runCode calls Execute,
	// which reads toolHandlers, and Go rejects that initialization cycle.
	toolHandlers[RunCode] = runCode
}

// CodeModeSpecs are the tools offered in code mode: Specs, then run_code.
var CodeModeSpecs = append(slices.Clip(Specs), runCodeSpec())

// SpecsFor returns the tools offered to the model with code mode on or off.
func SpecsFor(codeMode bool) []provider.ToolSpec {
	if codeMode {
		return CodeModeSpecs
	}
	return Specs
}

func runCodeSpec() provider.ToolSpec {
	return functionTool(RunCode, "Run a JavaScript async function that calls the workspace tools as goat.<tool>({...}) and get back only what it returns and logs. The system prompt describes the goat API and its limits.",
		map[string]any{"code": map[string]any{"type": "string", "description": "An async arrow function in plain JavaScript, e.g. async () => { const out = await goat.read_file({ path: \"go.mod\" }); return out.split(\"\\n\")[1]; }"}}, "code")
}

// codeDocs describe what each tool returns, for the goat API declaration.
var codeDocs = map[string]codemode.Doc{
	"read_dir":         {Returns: `"Directory listing for <path>:\n<one entry per line>"; "(empty)" in place of the entries for an empty directory.`},
	"tree_dir":         {Returns: `"Directory tree for <path>:\n<tree lines>", then "\n(truncated after N line(s))" when the limit is hit.`},
	"read_file":        {Returns: `"File contents for <path>:\n<content>", or with offset or limit "File contents for <path> (lines A-B of N):\n<lines>". Lines carry no line numbers.`},
	"write_file":       {Returns: `"File <path> written successfully."; with preview "Preview for write_file on <path>:\n<diff>".`},
	"edit_file":        {Returns: `"File <path> edited successfully. Replaced N occurrence(s)."; with preview "Preview for edit_file on <path>:\nReplacements: N\n<diff>".`, Throws: "When old_text is empty, is not found, or matches more than once without replace_all: true."},
	"edit_json_file":   {Returns: `"JSON file <path> updated successfully."; with preview "Preview for edit_json_file on <path>:\n<diff>". value_json is JSON text, e.g. JSON.stringify(value).`},
	"copy_file":        {Returns: `"File <source> copied to <destination> successfully."; with preview "Preview for copy_file from <source> to <destination>:\n<diff>".`},
	"move_file":        {Returns: `"File <source> moved to <destination> successfully."; with preview "Preview for move_file from <source> to <destination>:\n<diff>...".`},
	"delete_file":      {Returns: `"File <path> deleted successfully."; with preview "Preview for delete_file on <path>:\n<diff>".`},
	"copy_dir":         {Returns: `"Directory <source> copied to <destination> successfully."; with preview "Preview for copy_dir from <source> to <destination>:\n<changes>".`},
	"move_dir":         {Returns: `"Directory <source> moved to <destination> successfully."; with preview "Preview for move_dir from <source> to <destination>:\n<changes>".`},
	"delete_dir":       {Returns: `"Directory <path> deleted successfully."; with preview "Preview for delete_dir on <path>:\n<changes>".`},
	"undo_last_change": {Returns: `"Undid <kind> for <paths>." or "No recorded operations available to undo."; with preview "Preview for undo_last_change on <kind> (<paths>):\n<changes>".`},
	"run_bash":         {Returns: `"Bash output for \"<command>\" in <dir>:\n<stdout and stderr>" ("(no output)" when empty).`, Throws: "On a non-zero exit (the message is \"goat.run_bash: exit status N\" followed by the output), a timeout, or a signal. A command the user declines stops the whole script."},
	"find_files":       {Returns: `"Found N file(s) under <path>:\n<one path per line>", then "\n(truncated after N result(s))" when limited; "No files found ..." when there are none.`},
	ViewImage:          {Returns: `"Image <path> (<width>x<height> <format>, <size>) is attached."; the image itself reaches you with run_code's result, not the script.`, Throws: "When the file is not an image, or the model in use cannot see images."},
	"grep_files":       {Returns: `"Found N match(es) for \"<query>\" under <path>:\n<path>:<line>:<text> per line", then "\n(truncated after N match(es))" when limited; "No matches found ..." when there are none.`},
}

// CodeModeGuide is the system prompt section that describes run_code: the goat API and
// the rules a script runs under. maxChars and maxLines are the budget the result is
// cut to before the model sees it.
func CodeModeGuide(maxChars, maxLines int) string {
	limits := codemode.DefaultLimits
	return fmt.Sprintf(`Code mode is on: run_code runs JavaScript that calls the tools as functions. One run_code call can chain, loop over and filter many tool results without a round trip to you for each; only the value the script returns and what it logs with console.log come back, so intermediate results stay out of the conversation.

The script sees this API:

%s

Each goat function takes the same arguments as the tool of the same name, as one object, and resolves to the same text that tool returns. The rules:
- Write one async arrow function in plain JavaScript: async () => { ...; return value; }. No TypeScript annotations, import, require, fetch, process, timers or file access other than through goat.
- Await each call. Calls run one at a time, in order; Promise.all works but does not run them in parallel. Parse the returned text yourself; @returns shows its format. An unknown argument name is an error.
- A failed call rejects with an Error whose message is "goat.<tool>: <error text>": a missing file, a non-zero bash exit with its output, a plan-mode refusal. Use try/catch where failure is expected, such as a test run. An uncaught error, or a failed call nothing awaited, ends the script; changes already made stay made, and the error comes back with the line it came from and the console output.
- Each call follows the same rules as a direct call: plan mode applies, and a run_bash command may wait for the user's approval. If the user declines a command, the script stops there and you get their answer. Every change is its own undo step.
- Listing tools stop at their limit and then end with a "(truncated after N ...)" line; pass a larger limit when you need everything, and leave that line out when you parse the results.
- Limits: %d goat calls and %s of run time per script (time waiting for the user's approval does not count); each run_bash command still times out on its own. The result and console output together are cut to about %d characters and %d lines, so return a small, focused value: a string, or an object that converts to JSON.
- For a single step, call the tool directly. Use run_code for several dependent calls, loops over many files, or filtering large outputs.

Example: async () => { const out = await goat.grep_files({ path: ".", query: "TODO", glob: "*.go", limit: 1000 }); if (out.startsWith("No matches")) return []; return [...new Set(out.split("\n").slice(1).filter(line => !line.startsWith("(truncated")).map(line => line.split(":")[0]))]; }`,
		codemode.Declarations(Specs, codeDocs), limits.MaxCalls, limits.Timeout, maxChars, maxLines)
}

func runCode(ctx context.Context, raw string, opts Options) (string, error) {
	args, err := decodeArgs[struct {
		Code string `json:"code"`
	}](raw)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Code) == "" {
		return "", errors.New("missing code")
	}

	// A script can call every tool but run_code itself.
	names := Names()

	// A declined command stops the script, so the decline and any feedback reach the
	// model rather than a catch block, and the user is not asked again and again.
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	nested := opts
	nested.Nested = nil
	if approve := opts.ApproveBash; approve != nil {
		nested.ApproveBash = func(callCtx context.Context, command, workdir string) error {
			// Waiting for the user does not count against the script's run time.
			resume := codemode.PauseClock(callCtx)
			err := approve(callCtx, command, workdir)
			resume()
			if err != nil && callCtx.Err() == nil {
				stop(fmt.Errorf("goat.run_bash: %w", err))
			}
			return err
		}
	}

	count := 0
	host := codemode.Host{
		Names: names,
		Call: func(ctx context.Context, name, argsJSON string) (string, error) {
			count++
			call := provider.ToolCall{ID: fmt.Sprintf("%s.%d", opts.CallID, count), Name: name, Arguments: argsJSON}
			var done func(string, error)
			if opts.Nested != nil {
				done = opts.Nested.Start(call)
			}
			spec, _ := findSpec(name)
			err := checkArgumentNames(spec, argsJSON)
			var output string
			if err == nil {
				nested.CallID = call.ID
				output, err = Execute(ctx, call, nested)
			}
			if done != nil {
				done(output, err)
			}
			if err != nil {
				return "", fmt.Errorf("goat.%s: %w", name, err)
			}
			return output, nil
		},
	}

	result, err := codemode.Run(ctx, args.Code, host, codemode.DefaultLimits)
	if err != nil {
		var runErr *codemode.Error
		if !errors.As(err, &runErr) {
			return "", err
		}
		return "", &codeError{err: runErr}
	}
	return formatCodeResult(result), nil
}

// codeError is a failed script, reported with its console output and how many calls
// ran before it failed.
type codeError struct{ err *codemode.Error }

func (e *codeError) Error() string {
	var b strings.Builder
	b.WriteString(e.err.Message)
	if e.err.Logs != "" {
		b.WriteString("\nConsole output:\n" + e.err.Logs)
	}
	switch {
	case e.err.Calls == 0:
		b.WriteString("\n(no goat calls ran)")
	case e.err.Failed > 0:
		fmt.Fprintf(&b, "\n(%s ran before the failure, %d of them failed; changes they made are kept)", callCount(e.err.Calls, 0), e.err.Failed)
	default:
		fmt.Fprintf(&b, "\n(%s ran before the failure; changes they made are kept)", callCount(e.err.Calls, 0))
	}
	return b.String()
}

func (e *codeError) Unwrap() error { return e.err }

// formatCodeResult is the text a finished script returns to the model. Its first line
// always starts "run_code ran ", which the terminal summary relies on.
func formatCodeResult(result codemode.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "run_code ran %s.", callCount(result.Calls, result.Failed))
	if result.HasValue {
		b.WriteString("\nResult:\n" + result.Value)
	} else {
		b.WriteString("\nResult: (no return value)")
	}
	if result.Logs != "" {
		b.WriteString("\nConsole output:\n" + result.Logs)
	}
	return b.String()
}

func callCount(calls, failed int) string {
	text := fmt.Sprintf("%d goat call", calls)
	if calls != 1 {
		text += "s"
	}
	if failed > 0 {
		text += fmt.Sprintf(", %d failed", failed)
	}
	return text
}

// checkArgumentNames refuses argument names the tool does not take. Direct calls ignore
// them, but in a script a misspelt name is far more likely than a deliberate extra.
func checkArgumentNames(spec provider.ToolSpec, argsJSON string) error {
	var arguments map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argsJSON), &arguments); err != nil {
		return nil // validateToolArguments reports it
	}
	properties, _ := spec.Parameters["properties"].(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(arguments)) {
		if _, ok := properties[name]; !ok {
			valid := codemode.ArgumentNames(spec)
			if len(valid) == 0 {
				return fmt.Errorf("unknown argument %q; it takes none", name)
			}
			return fmt.Errorf("unknown argument %q; valid: %s", name, strings.Join(valid, ", "))
		}
	}
	return nil
}
