package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
// verifyCommandsMu guards it, since turns running beside the main one read it.
var (
	verifyCommandsMu sync.RWMutex
	verifyCommands   = map[string]string{}
)

// setVerifyCommands replaces the verify commands and returns the ones it replaced.
func setVerifyCommands(commands map[string]string) map[string]string {
	verifyCommandsMu.Lock()
	defer verifyCommandsMu.Unlock()
	previous := verifyCommands
	verifyCommands = commands
	return previous
}

// snapshotVerifyCommands is a copy of the verify commands.
func snapshotVerifyCommands() map[string]string {
	verifyCommandsMu.RLock()
	defer verifyCommandsMu.RUnlock()
	return maps.Clone(verifyCommands)
}

// lookupVerifyCommand is the verify command for the workspace at dir, if any.
func lookupVerifyCommand(dir string) (string, bool) {
	verifyCommandsMu.RLock()
	defer verifyCommandsMu.RUnlock()
	for path, command := range verifyCommands {
		if sameDirectory(path, dir) {
			return command, true
		}
	}
	return "", false
}

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

// sameDirectory reports whether two paths name the same directory. When both exist the
// file system decides, which follows symbolic links and ignores case only where the
// volume does. A path that does not exist matches only its exact spelling: guessing
// that case does not matter could run another workspace's command.
func sameDirectory(a, b string) bool {
	aInfo, aErr := os.Stat(a)
	bInfo, bErr := os.Stat(b)
	if aErr == nil && bErr == nil {
		return os.SameFile(aInfo, bInfo)
	}
	canonical := func(path string) string {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		return filepath.Clean(path)
	}
	return canonical(a) == canonical(b)
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
	if command, ok := lookupVerifyCommand(wd); ok {
		return command, displayPath(configPath())
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
// but a read-only tool or a preview. run_bash and run_code always count: they take no
// preview, so a "preview" argument given to them is ignored and the command still runs.
func changesWorkspace(call provider.ToolCall) bool {
	return changesWorkspaceWith(call, nil)
}

// changesWorkspaceWith is changesWorkspace for a turn whose subagents' modes modeOf
// knows. Only a general subagent can change anything: spawning one counts, and so does
// a message to one, which starts or steers its work. Without modeOf a message counts
// for nothing, since there is no agent to send it to.
func changesWorkspaceWith(call provider.ToolCall, modeOf func(id string) (string, bool)) bool {
	switch call.Name {
	case tools.SpawnAgent:
		var args struct {
			Mode string `json:"mode"`
		}
		_ = json.Unmarshal([]byte(call.Arguments), &args)
		return strings.EqualFold(strings.TrimSpace(args.Mode), tools.AgentModeGeneral)
	case tools.SendMessage:
		var args struct {
			Agent string `json:"agent"`
		}
		if json.Unmarshal([]byte(call.Arguments), &args) != nil || modeOf == nil {
			return false
		}
		mode, ok := modeOf(strings.TrimSpace(args.Agent))
		return ok && mode == tools.AgentModeGeneral
	case tools.StopAgent, tools.AgentStatus, tools.WaitAgents:
		return false
	}
	switch tools.PlanModePolicyFor(call.Name) {
	case tools.PlanModeAllowed:
		return false
	case tools.PlanModeBlocked, tools.PlanModePerCall:
		return true
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

	started, waitedBefore := time.Now(), time.Duration(inputWaitTotal.Load())
	startSpinner("Verifying")
	output, runErr := workspace.RunBashFor(ctx, command, ".", verifyTimeout())
	stopSpinner()
	logToolCallResult(call, output, runErr, time.Since(started)-(time.Duration(inputWaitTotal.Load())-waitedBefore))
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
	verifyCommandsMu.Lock()
	removed := false
	for path := range verifyCommands {
		if sameDirectory(path, wd) {
			delete(verifyCommands, path)
			removed = true
		}
	}
	if command != "" {
		verifyCommands[filepath.Clean(wd)] = command
	}
	verifyCommandsMu.Unlock()
	if command == "" && !removed {
		fmt.Println("This workspace has no verify command in " + displayPath(configPath()) + ".")
		return nil
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
	for path, command := range snapshotVerifyCommands() {
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

// verifyState is what the verify gate knows about one turn.
type verifyState struct {
	// changed is whether a tool call since the last verify run may have changed the
	// workspace.
	changed bool
	// runs counts the turn's verify runs, and failed is whether the latest one failed.
	runs   int
	failed bool
}

// verifyTurn runs the verify command for a turn the model just finished. It returns the
// message to send the model when the command failed and the turn should go on, or ""
// when the turn is done. It stops with an error once the command has failed
// maxVerifyRuns times, when the model stopped after a failure without changing
// anything, or when the turn is out of rounds.
func verifyTurn(ctx context.Context, opts turnOptions, state *verifyState, round int) (string, error) {
	if !state.changed {
		return "", fmt.Errorf("%w, and the model stopped without changing anything; /output shows its output", errVerifyFailed)
	}
	state.changed = false
	state.runs++
	failure, err := opts.verify(ctx)
	if err != nil {
		return "", err
	}
	state.failed = failure != ""
	switch {
	case failure == "":
		return "", nil
	case state.runs >= maxVerifyRuns:
		return "", fmt.Errorf("%w after %d runs this turn; /output shows its output", errVerifyFailed, state.runs)
	case round >= maxToolRoundsPerTurn:
		return "", fmt.Errorf("%w, and the turn is out of tool rounds; send another message to let the agent continue", errVerifyFailed)
	}
	return failure, nil
}
