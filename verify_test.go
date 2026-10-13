package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/azs06/goat/internal/provider"
)

// verifyTestResponses are a turn that writes a file, then ends; after a verify failure
// the model writes again and ends again.
func writeAndEnd(id string) []*provider.Response {
	return []*provider.Response{
		{ID: id + "-tools", Stop: provider.StopToolUse, ToolCalls: []provider.ToolCall{{ID: id + "-call", Name: "write_file", Arguments: `{"path": "out.txt", "content": "` + id + `"}`}}},
		{ID: id + "-done", Stop: provider.StopEndTurn, Text: "done " + id},
	}
}

// fakeVerify records its runs and fails the first failures of them.
type fakeVerify struct {
	runs     int
	failures int
}

func (f *fakeVerify) check(ctx context.Context) (string, error) {
	f.runs++
	if f.runs <= f.failures {
		return "verify failed: run " + string(rune('0'+f.runs)), nil
	}
	return "", nil
}

func TestVerifyRunsOnlyAfterAChange(t *testing.T) {
	chdirForTest(t, t.TempDir())
	verify := &fakeVerify{}
	fake := &fakeProvider{name: provider.Anthropic, responses: []*provider.Response{
		{ID: "read", Stop: provider.StopToolUse, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "read_dir", Arguments: `{"path": "."}`}}},
		{ID: "preview", Stop: provider.StopToolUse, ToolCalls: []provider.ToolCall{{ID: "c2", Name: "write_file", Arguments: `{"path": "a.txt", "content": "x", "preview": true}`}}},
		{ID: "done", Stop: provider.StopEndTurn, Text: "nothing changed"},
	}}
	captureActivity(t, func() {
		if _, err := sendPromptStream(context.Background(), fake, "look", conversationState{}, turnOptions{verify: verify.check}); err != nil {
			t.Fatal(err)
		}
	})
	if verify.runs != 0 {
		t.Fatalf("reads and previews change nothing, yet verify ran %d times", verify.runs)
	}
}

func TestVerifyFailureGoesBackToTheModel(t *testing.T) {
	chdirForTest(t, t.TempDir())
	verify := &fakeVerify{failures: 1}
	fake := &fakeProvider{name: provider.Anthropic, responses: append(writeAndEnd("one"), writeAndEnd("two")...)}
	captureActivity(t, func() {
		if _, err := sendPromptStream(context.Background(), fake, "write it", conversationState{}, turnOptions{verify: verify.check}); err != nil {
			t.Fatal(err)
		}
	})
	if verify.runs != 2 || len(fake.requests) != 4 {
		t.Fatalf("expected a failed and a passing run over 4 requests, got %d runs, %d requests", verify.runs, len(fake.requests))
	}
	// The failure goes as the next user message, after the model's finished reply.
	failed := fake.requests[2]
	if failed.UserText != "verify failed: run 1" || len(failed.ToolResults) != 0 {
		t.Fatalf("expected the failure as a user message, got %q with %d results", failed.UserText, len(failed.ToolResults))
	}
}

func TestVerifyGivesUpAfterTheLastRun(t *testing.T) {
	chdirForTest(t, t.TempDir())
	verify := &fakeVerify{failures: 10}
	var responses []*provider.Response
	for _, id := range []string{"a", "b", "c", "d"} {
		responses = append(responses, writeAndEnd(id)...)
	}
	fake := &fakeProvider{name: provider.Anthropic, responses: responses}
	captureActivity(t, func() {
		_, err := sendPromptStream(context.Background(), fake, "write it", conversationState{}, turnOptions{verify: verify.check})
		if !errors.Is(err, errVerifyFailed) || !strings.Contains(err.Error(), "after 3 runs") {
			t.Fatalf("expected verification to give up, got %v", err)
		}
	})
	if verify.runs != maxVerifyRuns {
		t.Fatalf("expected %d runs, got %d", maxVerifyRuns, verify.runs)
	}
}

func TestVerifyStopsWhenTheModelGivesUp(t *testing.T) {
	chdirForTest(t, t.TempDir())
	verify := &fakeVerify{failures: 10}
	fake := &fakeProvider{name: provider.Anthropic, responses: append(writeAndEnd("a"),
		&provider.Response{ID: "explain", Stop: provider.StopEndTurn, Text: "That failure is not mine to fix."})}
	captureActivity(t, func() {
		_, err := sendPromptStream(context.Background(), fake, "write it", conversationState{}, turnOptions{verify: verify.check})
		if !errors.Is(err, errVerifyFailed) || !strings.Contains(err.Error(), "without changing anything") {
			t.Fatalf("expected the turn to stop, got %v", err)
		}
	})
	if verify.runs != 1 {
		t.Fatalf("an unchanged workspace should not be checked again, got %d runs", verify.runs)
	}
}

func TestVerifyInterruptionEndsTheTurn(t *testing.T) {
	chdirForTest(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	interrupted := func(context.Context) (string, error) {
		cancel()
		return "", context.Canceled
	}
	fake := &fakeProvider{name: provider.Anthropic, responses: writeAndEnd("a")}
	captureActivity(t, func() {
		if _, err := sendPromptStream(ctx, fake, "write it", conversationState{}, turnOptions{verify: interrupted}); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestChangesWorkspace(t *testing.T) {
	for _, test := range []struct {
		call provider.ToolCall
		want bool
	}{
		{provider.ToolCall{Name: "read_file", Arguments: `{"path": "a"}`}, false},
		{provider.ToolCall{Name: "grep_files", Arguments: `{}`}, false},
		{provider.ToolCall{Name: "edit_file", Arguments: `{"path": "a", "preview": true}`}, false},
		{provider.ToolCall{Name: "edit_file", Arguments: `{"path": "a"}`}, true},
		{provider.ToolCall{Name: "run_bash", Arguments: `{"command": "ls"}`}, true},
		{provider.ToolCall{Name: "run_code", Arguments: `{"code": "1"}`}, true},
		{provider.ToolCall{Name: "run_bash", Arguments: `{"command": "rm a", "preview": true}`}, true},
		{provider.ToolCall{Name: "run_code", Arguments: `{"code": "1", "preview": true}`}, true},
		{provider.ToolCall{Name: "run_tui", Arguments: `{"command": "vim a", "preview": true}`}, true},
		{provider.ToolCall{Name: "write_file", Arguments: `not json`}, true},
	} {
		if got := changesWorkspace(test.call); got != test.want {
			t.Errorf("%s %s: got %v", test.call.Name, test.call.Arguments, got)
		}
	}
}

// verifyConfigTest gives a test a config directory and a workspace, with verification
// settings cleared.
func verifyConfigTest(t *testing.T) (configDir, workDir string) {
	t.Helper()
	configDir = configTest(t)
	t.Setenv("GOAT_VERIFY", "")
	t.Setenv("GOAT_VERIFY_TIMEOUT", "")
	saved := verifyCommands
	verifyCommands = map[string]string{}
	t.Cleanup(func() { verifyCommands = saved })
	workDir = t.TempDir()
	chdirForTest(t, workDir)
	return configDir, workDir
}

func TestVerifyCommandComesFromConfigJSONByWorkspace(t *testing.T) {
	configDir, workDir := verifyConfigTest(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writeConfigFile(t, configDir, "config.json", `{
  "verify": {
    "`+filepath.ToSlash(workDir)+`": "go vet ./... && go test ./...",
    "~/elsewhere": "npm test"
  },
  "verify_timeout": 90
}`)
	if _, _, err := loadConfigFile(); err != nil {
		t.Fatal(err)
	}
	command, source := verifyCommand()
	if command != "go vet ./... && go test ./..." || !strings.Contains(source, "config.json") {
		t.Fatalf("got %q from %q", command, source)
	}
	if verifyCommands[filepath.Join(home, "elsewhere")] != "npm test" {
		t.Errorf("expected ~ to stand for the home directory: %v", verifyCommands)
	}
	if verifyTimeout() != 90*time.Second {
		t.Errorf("got timeout %s", verifyTimeout())
	}

	// The shell's GOAT_VERIFY applies everywhere and wins; "off" turns it off.
	t.Setenv("GOAT_VERIFY", "make check")
	if command, source := verifyCommand(); command != "make check" || source != "GOAT_VERIFY from the shell" {
		t.Errorf("got %q from %q", command, source)
	}
	t.Setenv("GOAT_VERIFY", "off")
	if command, _ := verifyCommand(); command != "" || verifyGate() != nil {
		t.Errorf("GOAT_VERIFY=off should turn verification off, got %q", command)
	}

	// Another workspace has none.
	t.Setenv("GOAT_VERIFY", "")
	chdirForTest(t, t.TempDir())
	if command, _ := verifyCommand(); command != "" {
		t.Errorf("another workspace got %q", command)
	}
}

func TestVerifySettingErrors(t *testing.T) {
	for _, test := range []struct{ content, want string }{
		{`{"verify": "go test ./..."}`, "verify must be an object"},
		{`{"verify": {"relative/dir": "go test"}}`, "full path"},
		{`{"verify": {"/abs": ""}}`, "must be a command"},
		{"{\"verify\": {\n\"/abs\": 3}}", "line 2"},
		{`{"verify_timeout": 0}`, "verify_timeout must be a whole number"},
	} {
		configDir, _ := verifyConfigTest(t)
		writeConfigFile(t, configDir, "config.json", test.content)
		if _, _, err := loadConfigFile(); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: expected %q, got %v", test.content, test.want, err)
		}
	}
}

func TestVerifyCommandSetsShowsAndRemoves(t *testing.T) {
	configDir, _ := verifyConfigTest(t)
	cfg := &config{}
	run := func(line string) (string, error) {
		var err error
		out := captureActivity(t, func() {
			cmd, args, lookupErr := lookupCommand(line)
			if lookupErr != nil || cmd == nil {
				t.Fatalf("%s: %v", line, lookupErr)
			}
			cfg.commandText = line
			err = cmd.callback(context.Background(), cfg, args)
		})
		return out, err
	}

	if out, _ := run("/verify"); !strings.Contains(out, "No verify command") {
		t.Fatalf("got %s", out)
	}
	if _, err := run("/verify run"); err == nil {
		t.Fatal("expected /verify run without a command to fail")
	}
	// The command is kept as typed, with its spacing and quotes.
	if out, err := run(`/verify   test -f ok.txt &&  grep -q "a  b" ok.txt`); err != nil || !strings.Contains(out, "Saved in") {
		t.Fatalf("got %s %v", out, err)
	}
	data, _ := os.ReadFile(filepath.Join(configDir, "config.json"))
	if !strings.Contains(string(data), `"test -f ok.txt &&  grep -q \"a  b\" ok.txt"`) {
		t.Fatalf("expected the command saved as typed:\n%s", data)
	}
	if _, err := applyConfig(data, "config.json"); err != nil {
		t.Fatalf("the saved config.json does not load: %v", err)
	}
	if out, _ := run("/verify"); !strings.Contains(out, `grep -q "a  b"`) {
		t.Fatalf("got %s", out)
	}

	// /verify run shows the failure, then the pass.
	out, err := run("/verify run")
	if !errors.Is(err, errVerifyFailed) || !strings.Contains(out, "Verify(") {
		t.Fatalf("expected a failed run, got %v:\n%s", err, out)
	}
	os.WriteFile("ok.txt", []byte("a  b\n"), 0o644)
	if out, err := run("/verify run"); err != nil || !strings.Contains(out, "Passed") {
		t.Fatalf("expected a passing run, got %v:\n%s", err, out)
	}

	if out, err := run("/verify off"); err != nil || !strings.Contains(out, "Removed") {
		t.Fatalf("got %s %v", out, err)
	}
	if command, _ := verifyCommand(); command != "" {
		t.Fatalf("expected no command, got %q", command)
	}
	if out, _ := run("/verify off"); !strings.Contains(out, "has no verify command") {
		t.Fatalf("got %s", out)
	}
	data, _ = os.ReadFile(filepath.Join(configDir, "config.json"))
	if strings.Contains(string(data), "ok.txt") {
		t.Fatalf("expected the command removed from config.json:\n%s", data)
	}
}

func TestVerifyRunsTheRealCommandAfterAChange(t *testing.T) {
	_, workDir := verifyConfigTest(t)
	verifyCommands[workDir] = `grep -q fixed out.txt`
	sessionSaver = func(string, savedSession) error { return nil }
	t.Cleanup(func() { sessionSaver = writeSessionFile })
	fake := &fakeProvider{name: provider.Anthropic, responses: []*provider.Response{
		{ID: "w1", Stop: provider.StopToolUse, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "write_file", Arguments: `{"path": "out.txt", "content": "broken"}`}}},
		{ID: "d1", Stop: provider.StopEndTurn, Text: "Done."},
		{ID: "w2", Stop: provider.StopToolUse, ToolCalls: []provider.ToolCall{{ID: "c2", Name: "write_file", Arguments: `{"path": "out.txt", "content": "fixed"}`}}},
		{ID: "d2", Stop: provider.StopEndTurn, Text: "Fixed it."},
	}}
	cfg := seamConfig(t, fake)
	chdirForTest(t, workDir)
	out := captureActivity(t, func() {
		if err := runPrompt(context.Background(), cfg, "write out.txt"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "Verify(grep -q fixed out.txt)") || !strings.Contains(out, "Passed") {
		t.Fatalf("expected the verify runs in the transcript:\n%s", out)
	}
	failure := fake.requests[2].UserText
	if !strings.HasPrefix(failure, verifyFailedNote) || !strings.Contains(failure, "Command: grep -q fixed out.txt") || !strings.Contains(failure, "exit status 1") {
		t.Fatalf("expected the failure sent to the model, got %q", failure)
	}

	// Both runs are recorded, so /output shows them.
	entries, _ := loadConversationHistoryEntries()
	runs := recentToolRuns(entries)
	if len(runs) != 4 || runs[0].call.ToolName != verifyToolName || runs[0].output.Status != "ok" || runs[2].call.ToolName != verifyToolName || runs[2].output.Status != "error" {
		t.Fatalf("expected the verify runs in history, got %+v", runs)
	}

	// Plan mode changes nothing, so it runs no check.
	cfg.planMode = true
	fake.responses = writeAndEnd("plan")
	captureActivity(t, func() { runPrompt(context.Background(), cfg, "plan it") })
	if after := recentToolRuns(mustHistory(t)); after[0].call.ToolName == verifyToolName {
		t.Fatalf("expected no verify run in plan mode, got %+v", after[0])
	}
}

// captureActivity is captureStdout that also captures tool calls and verify runs, which
// go to activityOut.
func captureActivity(t *testing.T, fn func()) string {
	t.Helper()
	return captureStdout(t, func() {
		saved := activityOut
		activityOut = os.Stdout
		defer func() { activityOut = saved }()
		fn()
	})
}

func mustHistory(t *testing.T) []historyEntry {
	t.Helper()
	entries, err := loadConversationHistoryEntries()
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// recoveryTurn is a fake Claude whose requests fail with a context overflow at the
// given request numbers (counting from 1), after which goat compacts and retries.
func recoveryTurn(overflowAt []int, responses ...*provider.Response) *fakeProvider {
	moved := provider.Conversation{Provider: provider.Anthropic, Messages: []json.RawMessage{json.RawMessage(`{}`)}}
	for _, resp := range responses {
		resp.Conversation = moved
	}
	var errs []error
	for i := 1; i <= slices.Max(overflowAt); i++ {
		var err error
		if slices.Contains(overflowAt, i) {
			err = errors.New("400: This model's maximum context length is 128000 tokens")
		}
		errs = append(errs, err)
	}
	return &fakeProvider{name: provider.Anthropic, summary: "s", sendErrs: errs, responses: responses}
}

// verifyLog makes the verify command append a line to verify.log, failing when fail
// is set, and returns how to count its runs.
func verifyLog(t *testing.T, fail bool) func() int {
	t.Helper()
	command := "echo ran >> verify.log"
	if fail {
		command += " && false"
	}
	t.Setenv("GOAT_VERIFY", command)
	return func() int {
		data, _ := os.ReadFile("verify.log")
		return strings.Count(string(data), "ran")
	}
}

func TestVerifyChecksChangesMadeBeforeARecovery(t *testing.T) {
	chdirForTest(t, t.TempDir())
	runs := verifyLog(t, false)
	// The model writes a file; sending the write's result overflows the context, and
	// the retry after compacting ends the turn without changing anything more.
	p := recoveryTurn([]int{2},
		&provider.Response{ID: "write", Stop: provider.StopToolUse, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "write_file", Arguments: `{"path": "out.txt", "content": "x"}`}}},
		&provider.Response{ID: "done", Stop: provider.StopEndTurn, Text: "done"})
	cfg := newModelTestConfig(t, provider.Model{Provider: provider.Anthropic, ID: "claude-opus-5-5"}, p)
	captureActivity(t, func() {
		if err := runPrompt(context.Background(), cfg, "write it"); err != nil {
			t.Fatalf("expected the retry to succeed, got %v", err)
		}
	})
	if len(p.requests) != 3 {
		t.Fatalf("expected the turn, the failed round and the retry, got %d requests", len(p.requests))
	}
	if runs() != 1 {
		t.Fatalf("the file written before the retry should still be checked, got %d runs", runs())
	}
}

func TestVerifyFailureOutlastsARecovery(t *testing.T) {
	chdirForTest(t, t.TempDir())
	runs := verifyLog(t, true)
	// The check fails; sending the failure overflows the context, and the model ends the
	// retried turn without fixing anything.
	p := recoveryTurn([]int{3},
		&provider.Response{ID: "write", Stop: provider.StopToolUse, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "write_file", Arguments: `{"path": "out.txt", "content": "x"}`}}},
		&provider.Response{ID: "done", Stop: provider.StopEndTurn, Text: "done"},
		&provider.Response{ID: "shrug", Stop: provider.StopEndTurn, Text: "looks fine to me"})
	cfg := newModelTestConfig(t, provider.Model{Provider: provider.Anthropic, ID: "claude-opus-5-5"}, p)
	var err error
	captureActivity(t, func() { err = runPrompt(context.Background(), cfg, "write it") })
	if !errors.Is(err, errVerifyFailed) || !strings.Contains(err.Error(), "without changing anything") {
		t.Fatalf("a failed check must not pass because the turn was retried, got %v", err)
	}
	if runs() != 1 {
		t.Fatalf("nothing changed after the failure, so it should not run again; got %d runs", runs())
	}
}

func TestSameDirectory(t *testing.T) {
	dir := t.TempDir()
	if !sameDirectory(dir, dir+string(filepath.Separator)) {
		t.Error("a directory should match itself")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err == nil && !sameDirectory(link, dir) {
		t.Error("a symbolic link should match its target")
	}
	if sameDirectory(dir, t.TempDir()) {
		t.Error("two directories should not match")
	}

	// A path that does not exist matches only its own spelling, whatever the system:
	// on a case-sensitive volume /code/App and /code/app are different workspaces.
	gone := filepath.Join(dir, "Gone", "App")
	if !sameDirectory(gone, gone) {
		t.Error("a missing path should match itself")
	}
	if sameDirectory(gone, filepath.Join(dir, "Gone", "app")) || sameDirectory(gone, filepath.Join(dir, "gone", "App")) {
		t.Error("a missing path must not match another spelling")
	}

	// Two spellings of one existing directory match only where the volume ignores case.
	upper := filepath.Join(dir, "Work")
	os.Mkdir(upper, 0o755)
	lower := filepath.Join(dir, "work")
	_, err := os.Stat(lower)
	ignoresCase := err == nil
	if sameDirectory(upper, lower) != ignoresCase {
		t.Errorf("Work and work should match only on a case-insensitive volume (ignores case: %v)", ignoresCase)
	}
	if !ignoresCase {
		os.Mkdir(lower, 0o755)
		if sameDirectory(upper, lower) {
			t.Error("Work and work are different directories here")
		}
	}
}
