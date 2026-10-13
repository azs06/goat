package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azs06/goat/internal/codemode"
	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/workspace"
)

func runCodeCall(code string) provider.ToolCall {
	return provider.ToolCall{ID: "call_1", Name: RunCode, Arguments: fmt.Sprintf(`{"code":%q}`, code)}
}

// recordingObserver keeps every nested call and its result.
type recordingObserver struct {
	mu      sync.Mutex
	calls   []provider.ToolCall
	outputs []string
}

func (o *recordingObserver) Start(call provider.ToolCall) func(string, error) {
	o.mu.Lock()
	o.calls = append(o.calls, call)
	o.mu.Unlock()
	return func(output string, err error) {
		o.mu.Lock()
		defer o.mu.Unlock()
		if err != nil {
			output = "error: " + err.Error()
		}
		o.outputs = append(o.outputs, output)
	}
}

func TestRunCodeNeedsCodeMode(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := Execute(context.Background(), runCodeCall(`async () => 1`), Options{}); !errors.Is(err, ErrCodeModeOff) {
		t.Fatalf("got %v", err)
	}
}

func TestRunCodeCallsTheTools(t *testing.T) {
	t.Chdir(t.TempDir())
	os.WriteFile("a.txt", []byte("alpha\n"), 0o644)
	os.WriteFile("b.txt", []byte("beta\n"), 0o644)

	observer := &recordingObserver{}
	output, err := Execute(context.Background(), runCodeCall(`async () => {
		const found = await goat.find_files({ path: ".", pattern: "*.txt" });
		const paths = found.split("\n").slice(1);
		const sizes = {};
		for (const path of paths) {
			const text = await goat.read_file({ path });
			sizes[path] = text.split("\n").slice(1).join("\n").trim();
		}
		await goat.write_file({ path: "out.txt", content: JSON.stringify(sizes) });
		console.log("wrote", paths.length);
		return sizes;
	}`), Options{CodeMode: true, CallID: "call_1", Nested: observer})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output, "run_code ran 4 goat calls.\nResult:\n{") || !strings.Contains(output, `"a.txt": "alpha"`) || !strings.HasSuffix(output, "Console output:\nwrote 2") {
		t.Errorf("output %q", output)
	}
	if data, _ := os.ReadFile("out.txt"); string(data) != `{"a.txt":"alpha","b.txt":"beta"}` {
		t.Errorf("out.txt = %q", data)
	}

	var ids, names []string
	for _, call := range observer.calls {
		ids, names = append(ids, call.ID), append(names, call.Name)
	}
	if !slices.Equal(ids, []string{"call_1.1", "call_1.2", "call_1.3", "call_1.4"}) || !slices.Equal(names, []string{"find_files", "read_file", "read_file", "write_file"}) {
		t.Errorf("observed %v %v", ids, names)
	}
	if len(observer.outputs) != 4 || !strings.HasPrefix(observer.outputs[1], "File contents for a.txt") {
		t.Errorf("outputs %q", observer.outputs)
	}
}

func TestRunCodeFollowsPlanMode(t *testing.T) {
	t.Chdir(t.TempDir())
	os.WriteFile("a.txt", []byte("hello\n"), 0o644)

	output, err := Execute(context.Background(), runCodeCall(`async () => {
		const results = [];
		for (const call of [
			() => goat.write_file({ path: "a.txt", content: "changed" }),
			() => goat.run_bash({ command: "touch ran.txt" }),
			() => goat.edit_file({ path: "a.txt", old_text: "hello", new_text: "bye", preview: true }),
			() => goat.read_file({ path: "a.txt" }),
		]) {
			try { results.push((await call()).split("\n")[0]) } catch (e) { results.push(e.message) }
		}
		return results.join("\n");
	}`), Options{CodeMode: true, PlanMode: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"goat.write_file: " + ErrPlanMode.Error(), "goat.run_bash: " + ErrPlanMode.Error(), "Preview for edit_file on a.txt:", "File contents for a.txt:"} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in %q", want, output)
		}
	}
	if data, _ := os.ReadFile("a.txt"); string(data) != "hello\n" {
		t.Errorf("plan mode changed a.txt: %q", data)
	}
	if _, err := os.Stat("ran.txt"); err == nil {
		t.Error("plan mode ran a bash command")
	}
}

func TestRunCodeAsksBeforeEachCommand(t *testing.T) {
	t.Chdir(t.TempDir())
	var asked []string
	approve := func(ctx context.Context, command, workdir string) error {
		asked = append(asked, command+" in "+workdir)
		if strings.Contains(command, "rm") {
			return errors.New("the user declined to run this command; their feedback: use git clean")
		}
		return nil
	}
	output, err := Execute(context.Background(), runCodeCall(`async () => (await goat.run_bash({ command: "echo hi" })).split("\n")[1]`), Options{CodeMode: true, ApproveBash: approve})
	if err != nil || !strings.HasSuffix(output, "Result:\nhi") {
		t.Fatalf("got %q %v", output, err)
	}

	// A decline stops the script, even one that catches the error, so the user is not
	// asked again and the model hears their answer.
	asked = nil
	_, err = Execute(context.Background(), runCodeCall(`async () => {
		for (const command of ["rm -rf x", "rm -rf y"]) {
			try { await goat.run_bash({ command, workdir: "." }) } catch (e) {}
		}
		await goat.write_file({ path: "after.txt", content: "x" });
		return "kept going";
	}`), Options{CodeMode: true, ApproveBash: approve})
	if err == nil || !strings.Contains(err.Error(), "the script was stopped: goat.run_bash: the user declined to run this command; their feedback: use git clean") {
		t.Fatalf("got %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Error("a decline is not an interrupt")
	}
	if !slices.Equal(asked, []string{"rm -rf x in ."}) {
		t.Errorf("asked %q", asked)
	}
	if _, statErr := os.Stat("after.txt"); statErr == nil {
		t.Error("the script went on after the decline")
	}

	// The stop names the tool whose approval was declined.
	if workspace.TUIAvailable {
		_, err = Execute(context.Background(), runCodeCall(`async () => goat.run_tui({ command: "rm -rf z" })`), Options{CodeMode: true, ApproveBash: approve})
		if err == nil || !strings.Contains(err.Error(), "the script was stopped: goat.run_tui: the user declined") {
			t.Fatalf("got %v", err)
		}
	}
}

func TestRunCodeShowsRefusedCalls(t *testing.T) {
	t.Chdir(t.TempDir())
	observer := &recordingObserver{}
	_, err := Execute(context.Background(), runCodeCall(`async () => { try { await goat.read_file({ pth: "x" }) } catch (e) {} return 1 }`), Options{CodeMode: true, Nested: observer})
	if err != nil {
		t.Fatal(err)
	}
	if len(observer.outputs) != 1 || !strings.Contains(observer.outputs[0], `unknown argument "pth"`) {
		t.Errorf("observed %q", observer.outputs)
	}
}

func TestRunCodeDoesNotCountApprovalTime(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := codemode.DefaultLimits
	codemode.DefaultLimits.Timeout = 1500 * time.Millisecond
	t.Cleanup(func() { codemode.DefaultLimits = previous })

	approve := func(ctx context.Context, command, workdir string) error {
		time.Sleep(2 * time.Second)
		return nil
	}
	output, err := Execute(context.Background(), runCodeCall(`async () => (await goat.run_bash({ command: "echo slow" })).split("\n")[1]`), Options{CodeMode: true, ApproveBash: approve})
	if err != nil || !strings.Contains(output, "Result:\nslow") {
		t.Fatalf("got %q %v", output, err)
	}
}

func TestRunCodeRejectsUnknownArguments(t *testing.T) {
	t.Chdir(t.TempDir())
	os.WriteFile("a.txt", []byte("x"), 0o644)
	_, err := Execute(context.Background(), runCodeCall(`async () => await goat.read_file({ file: "a.txt" })`), Options{CodeMode: true})
	if err == nil || !strings.Contains(err.Error(), `goat.read_file: unknown argument "file"; valid: path, limit, offset`) {
		t.Fatalf("got %v", err)
	}
	_, err = Execute(context.Background(), runCodeCall(`async () => await goat.undo_last_change({ all: true })`), Options{CodeMode: true})
	if err == nil || !strings.Contains(err.Error(), `unknown argument "all"; valid: preview`) {
		t.Fatalf("got %v", err)
	}
}

func TestRunCodeFailureKeepsLogsAndCounts(t *testing.T) {
	t.Chdir(t.TempDir())
	os.WriteFile("a.txt", []byte("x"), 0o644)
	_, err := Execute(context.Background(), runCodeCall("async () => {\n  console.log('reading');\n  await goat.read_file({ path: 'a.txt' });\n  await goat.read_file({ path: 'missing.txt' });\n}"), Options{CodeMode: true})
	if err == nil {
		t.Fatal("expected an error")
	}
	message := err.Error()
	for _, want := range []string{"uncaught error at line 4", "goat.read_file:", "Console output:\nreading", "(2 goat calls ran before the failure, 1 of them failed; changes they made are kept)"} {
		if !strings.Contains(message, want) {
			t.Errorf("missing %q in %q", want, message)
		}
	}
}

func TestRunCodeInterruptIsCancellation(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := Execute(ctx, runCodeCall(`async () => { try { await goat.run_bash({ command: "sleep 5" }) } catch (e) {} return "kept going" }`), Options{CodeMode: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestRunCodeReturnsFullNestedOutput(t *testing.T) {
	t.Chdir(t.TempDir())
	os.WriteFile("big.txt", []byte(strings.Repeat("0123456789\n", 3000)), 0o644)
	output, err := Execute(context.Background(), runCodeCall(`async () => (await goat.read_file({ path: "big.txt" })).length`), Options{CodeMode: true})
	if err != nil || !strings.HasSuffix(output, "Result:\n33027") {
		t.Fatalf("got %q %v", output, err)
	}
}

func TestCodeModeGuide(t *testing.T) {
	guide := CodeModeGuide(16000, 400)
	for _, want := range []string{
		"declare const goat: {",
		"  read_file(input: { path: string; limit?: number; offset?: number }): Promise<string>;",
		"  undo_last_change(input?: { preview?: boolean }): Promise<string>;",
		`edit_json_file(input: { path: string; json_path: string; operation: "set" | "delete"; preview?: boolean; value_json?: string }): Promise<string>;`,
		"50 goat calls and 2m0s of run time",
		"about 16000 characters and 400 lines",
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("guide is missing %q", want)
		}
	}
	if strings.Contains(guide, "run_code(") {
		t.Error("run_code must not be callable from a script")
	}
	for _, spec := range Specs {
		if codeDocs[spec.Name].Returns == "" {
			t.Errorf("%s has no @returns doc", spec.Name)
		}
	}
	for i := 0; i < 20; i++ {
		if CodeModeGuide(16000, 400) != guide {
			t.Fatal("the guide is part of the cached prompt and must not change between calls")
		}
	}
}
