package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/azs06/goat/internal/jsonfile"
	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/tools"
	"github.com/azs06/goat/internal/workspace"
)

// The verify command is the user's own check, such as "go vet ./... && go test ./...",
// that goat runs itself when the model ends a turn that may have changed the workspace.
// A failure goes back to the model, which fixes it and finishes again.
//
// It comes only from the user's own settings: the shell's GOAT_VERIFY, or config.json's
// "verify", which maps workspaces to commands. Never from the workspace itself, so a
// cloned repository cannot get a command run without the user's approval.

// verifyToolName is the name the verify run is shown and recorded under, like a tool's.
const verifyToolName = "verify"

// maxVerifyRuns is how often one turn runs the verify command before goat stops and
// leaves the failure to the user.
const maxVerifyRuns = 3

// defaultVerifyTimeout is how long the verify command may run unless
// GOAT_VERIFY_TIMEOUT (config.json's verify_timeout) says otherwise, in seconds.
const defaultVerifyTimeout = 5 * time.Minute

// verifyCommands are config.json's verify commands by workspace, with ~ expanded.
var verifyCommands = map[string]string{}

// verifyFailedNote starts the message the model gets when the verify command fails.
const verifyFailedNote = "[goat ran the user's verify command after your changes, and it failed. Fix what it reports, then finish your turn; goat runs it again when you stop. If the failure has nothing to do with your changes, or you cannot fix it, say so and stop without changing anything.]"

// parseVerifySetting reads config.json's verify object: workspace paths, absolute or
// starting with ~/, to the command to run there.
func parseVerifySetting(raw json.RawMessage, valueLine int) (map[string]string, error) {
	members, err := jsonfile.Object(raw, `verify must be an object of workspaces and commands, such as {"~/code/app": "go test ./..."}`)
	if err != nil {
		return nil, jsonfile.Shift(err, valueLine)
	}
	commands := map[string]string{}
	for _, member := range members {
		// Lines inside verify count from its own first line.
		fail := func(message string) error {
			return &jsonfile.Error{Line: member.Line + valueLine - 1, Message: message}
		}
		var command string
		if json.Unmarshal(member.Value, &command) != nil || strings.TrimSpace(command) == "" {
			return nil, fail("verify." + member.Name + " must be a command such as \"go test ./...\"")
		}
		path := expandHome(strings.TrimSpace(member.Name))
		if !filepath.IsAbs(path) {
			return nil, fail(fmt.Sprintf("verify has %s; name each workspace by its full path or a path starting with ~/", jsonfile.Quote(member.Name, "a workspace")))
		}
		commands[filepath.Clean(path)] = strings.TrimSpace(command)
	}
	return commands, nil
}

// expandHome turns a leading ~/ into the home directory.
func expandHome(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, rest)
		}
	}
	return path
}

// sameDirectory reports whether two paths name the same directory, through symbolic
// links, and ignoring case where file names do by default (macOS, Windows).
func sameDirectory(a, b string) bool {
	canonical := func(path string) string {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		return filepath.Clean(path)
	}
	a, b = canonical(a), canonical(b)
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// verifyCommand is the verify command for the workspace goat runs in, and where it was
// set; "" when there is none. GOAT_VERIFY applies to every workspace and wins; "off"
// turns verification off for the run.
func verifyCommand() (command, source string) {
	if value := strings.TrimSpace(os.Getenv("GOAT_VERIFY")); value != "" {
		if strings.EqualFold(value, "off") {
			return "", ""
		}
		source = envSources["GOAT_VERIFY"]
		if source == "" {
			source = "the shell"
		}
		return value, "GOAT_VERIFY from " + source
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", ""
	}
	for path, command := range verifyCommands {
		if sameDirectory(path, wd) {
			return command, displayPath(configPath())
		}
	}
	return "", ""
}

// verifyTimeout is how long the verify command may run.
func verifyTimeout() time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(os.Getenv("GOAT_VERIFY_TIMEOUT"))); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return defaultVerifyTimeout
}

// changesWorkspace reports whether a tool call may have changed the workspace: anything
// but a read-only tool or a preview. run_code counts, since its script may change files.
func changesWorkspace(call provider.ToolCall) bool {
	if tools.PlanModePolicyFor(call.Name) == tools.PlanModeAllowed {
		return false
	}
	var args struct {
		Preview bool `json:"preview"`
	}
	return json.Unmarshal([]byte(call.Arguments), &args) != nil || !args.Preview
}

// errVerifyFailed is returned when the verify command fails and goat runs no more.
var errVerifyFailed = errors.New("the verify command fails")

// runVerify runs the verify command in the workspace, showing and recording it like a
// tool call, so /output shows its full output. It returns the command's output and
// whether it passed; err is set only when the run was interrupted.
func runVerify(ctx context.Context, command string) (output string, passed bool, err error) {
	arguments, _ := json.Marshal(map[string]string{"command": command})
	call := provider.ToolCall{ID: verifyToolName, Name: verifyToolName, Arguments: string(arguments)}
	logToolCallStart(call)
	recordHistory(historyEntry{Type: "tool_call", ToolName: verifyToolName, Arguments: call.Arguments})

	started, waitedBefore := time.Now(), inputWaitTotal
	startSpinner("Verifying")
	output, runErr := workspace.RunBashFor(ctx, command, ".", verifyTimeout())
	stopSpinner()
	logToolCallResult(call, output, runErr, time.Since(started)-(inputWaitTotal-waitedBefore))
	if runErr != nil {
		recordHistory(historyEntry{Type: "tool_output", ToolName: verifyToolName, Content: runErr.Error(), Status: "error"})
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return runErr.Error(), false, nil
	}
	recordHistory(historyEntry{Type: "tool_output", ToolName: verifyToolName, Content: output, Status: "ok"})
	return output, true, nil
}

// verifyGate returns the check a turn runs when the model finishes after changing the
// workspace, or nil when this workspace has no verify command. The check returns the
// message for the model when the command fails, or "" when it passed.
func verifyGate() func(ctx context.Context) (string, error) {
	command, _ := verifyCommand()
	if command == "" {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		output, passed, err := runVerify(ctx, command)
		if err != nil || passed {
			return "", err
		}
		return verifyFailedNote + "\n\nCommand: " + command + "\n" + prepareToolOutputForModel(verifyToolName, output), nil
	}
}

// commandVerify shows, sets, clears or runs the workspace's verify command.
func commandVerify(ctx context.Context, c *config, args []string) error {
	command, source := verifyCommand()
	if len(args) == 0 {
		if command == "" {
			fmt.Println("No verify command for this workspace. " + style.Muted.Render("/verify <command> sets one, such as /verify go test ./..."))
			return nil
		}
		fmt.Println("Verify command: " + escapeControlChars(command) + style.Muted.Render(" ("+source+")"))
		fmt.Println(style.Muted.Render(fmt.Sprintf("goat runs it when a turn that changed something ends, for up to %s, and sends a failure back to the model (at most %d runs a turn). /verify run runs it now; /verify off removes it.", verifyTimeout(), maxVerifyRuns)))
		return nil
	}

	switch strings.ToLower(args[0]) {
	case "run":
		if len(args) == 1 {
			if command == "" {
				return errors.New("this workspace has no verify command; /verify <command> sets one")
			}
			if _, passed, err := runVerify(ctx, command); err != nil {
				return err
			} else if !passed {
				return errVerifyFailed
			}
			return nil
		}
	case "off":
		if len(args) == 1 {
			return saveVerifyCommand("")
		}
	}
	return saveVerifyCommand(commandArguments(c.commandText))
}

// commandArguments is what follows the command word in the line the user typed, with
// its spacing and quotes kept.
func commandArguments(line string) string {
	line = strings.TrimSpace(line)
	if i := strings.IndexFunc(line, unicode.IsSpace); i >= 0 {
		return strings.TrimSpace(line[i:])
	}
	return ""
}

// saveVerifyCommand sets the current workspace's verify command, or removes it when
// command is "", and saves it in config.json.
func saveVerifyCommand(command string) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	removed := false
	for path := range verifyCommands {
		if sameDirectory(path, wd) {
			delete(verifyCommands, path)
			removed = true
		}
	}
	if command == "" && !removed {
		fmt.Println("This workspace has no verify command in " + displayPath(configPath()) + ".")
		return nil
	}
	if command != "" {
		verifyCommands[filepath.Clean(wd)] = command
	}

	message := "Removed this workspace's verify command."
	if command != "" {
		message = "Verify command set: " + escapeControlChars(command)
	}
	if strings.TrimSpace(os.Getenv("GOAT_VERIFY")) != "" {
		message += style.Muted.Render(" GOAT_VERIFY is set, and wins over it.")
	}
	if configPath() == "" {
		fmt.Println(formatSuccess(message) + style.Muted.Render(" There is no config directory to save it in, so it lasts for this session."))
		return nil
	}
	if err := writeVerifyCommands(); err != nil {
		return fmt.Errorf("changed for this session, but saving it failed: %w", err)
	}
	fmt.Println(formatSuccess(message) + style.Muted.Render(" Saved in "+displayPath(configPath())+"."))
	return nil
}

// writeVerifyCommands saves verifyCommands as config.json's verify field. Workspaces
// under the home directory are saved as ~/..., so a config.json kept in a dotfiles
// repository works under another home directory.
func writeVerifyCommands() error {
	home, _ := os.UserHomeDir()
	saved := map[string]string{}
	for path, command := range verifyCommands {
		if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok && home != "" {
			path = "~/" + filepath.ToSlash(rest)
		}
		saved[path] = command
	}
	// Commands are kept as typed: && stays && rather than \u0026\u0026.
	var value bytes.Buffer
	encoder := json.NewEncoder(&value)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(saved); err != nil {
		return err
	}
	return writeJSONField(configPath(), "config.json", "verify", bytes.TrimSpace(value.Bytes()), configFileMode())
}

// verifyTurn runs the verify command for a turn the model just finished. It returns the
// message to send the model when the command failed and the turn should go on, or ""
// when the turn is done. It stops with an error once the command has failed
// maxVerifyRuns times, when the model stopped after a failure without changing
// anything, or when the turn is out of rounds.
func verifyTurn(ctx context.Context, opts turnOptions, changed *bool, runs *int, failed *bool, round int) (string, error) {
	if !*changed {
		return "", fmt.Errorf("%w, and the model stopped without changing anything; /output shows its output", errVerifyFailed)
	}
	*changed = false
	*runs++
	failure, err := opts.verify(ctx)
	if err != nil {
		return "", err
	}
	*failed = failure != ""
	switch {
	case failure == "":
		return "", nil
	case *runs >= maxVerifyRuns:
		return "", fmt.Errorf("%w after %d runs this turn; /output shows its output", errVerifyFailed, *runs)
	case round >= maxToolRoundsPerTurn:
		return "", fmt.Errorf("%w, and the turn is out of tool rounds; send another message to let the agent continue", errVerifyFailed)
	}
	return failure, nil
}
