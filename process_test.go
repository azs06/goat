package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
)

// TestMainProcess runs goat's main in a child process when GOAT_TEST_MAIN is set; the
// other tests in this file start it that way, with the arguments in GOAT_TEST_ARGS.
func TestMainProcess(t *testing.T) {
	if os.Getenv("GOAT_TEST_MAIN") != "1" {
		t.Skip("only runs as a child process")
	}
	os.Args = append([]string{"goat"}, strings.Fields(os.Getenv("GOAT_TEST_ARGS"))...)
	main()
	os.Exit(0)
}

// runGoat runs main in a child process in workspace, feeding it stdin, and returns its
// stdout, stderr and exit code. The workspace is its home directory: HOME, and
// USERPROFILE on Windows.
func runGoat(t *testing.T, workspace, stdin string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainProcess$")
	cmd.Dir = workspace
	cmd.Env = append(cleanGoatEnv(), "GOAT_TEST_MAIN=1", "GOAT_TEST_ARGS="+strings.Join(args, " "), "HOME="+workspace, "USERPROFILE="+workspace, "XDG_CONFIG_HOME="+workspace+"/config", "XDG_STATE_HOME="+workspace+"/state")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), code
}

// cleanGoatEnv is the environment without credentials or goat settings, so the child
// sees only what a test gives it.
func cleanGoatEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		base := strings.TrimSuffix(name, "_COMMAND")
		if strings.HasPrefix(name, "GOAT_") || strings.HasPrefix(name, "OPENAI_") || strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "DEEPSEEK_") || slices.Contains(provider.PresetKeyVars(), base) {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func TestProcessFlags(t *testing.T) {
	workspace := t.TempDir()
	if out, _, code := runGoat(t, workspace, "", nil, "--version"); code != 0 || !strings.HasPrefix(out, "goat ") {
		t.Fatalf("--version: %q (exit %d)", out, code)
	}
	if out, _, code := runGoat(t, workspace, "", nil, "--help"); code != 0 || !strings.Contains(out, "Usage:") {
		t.Fatalf("--help: %q (exit %d)", out, code)
	}
	if _, errOut, code := runGoat(t, workspace, "", nil, "--effort", "huge"); code != 2 || !strings.Contains(errOut, "unsupported reasoning effort") {
		t.Fatalf("bad flag: %q (exit %d)", errOut, code)
	}
}

func TestProcessNeedsAKey(t *testing.T) {
	_, errOut, code := runGoat(t, t.TempDir(), "", nil)
	if code == 0 || !strings.Contains(errOut, "no API key found") {
		t.Fatalf("expected a missing-key error, got %q (exit %d)", errOut, code)
	}
}

func TestProcessPipedCommands(t *testing.T) {
	workspace := t.TempDir()
	out, _, code := runGoat(t, workspace, "/status\n/help\n/tools\n/nosuch\n/exit\n", []string{"ANTHROPIC_API_KEY=test"})
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{"Welcome to G.O.A.T 🐐 agent", "Model:     anthropic:claude-opus-5-5", "Conversation", "read_file", "unknown command", "Goodbye"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Error("piped output contains escape sequences")
	}
}

func TestProcessOneShot(t *testing.T) {
	workspace := t.TempDir()
	fake := newFakeAnthropic(t, newSSE("msg_1").text("The answer is 4.").stop("end_turn", ""))
	env := []string{"ANTHROPIC_API_KEY=test", "ANTHROPIC_BASE_URL=" + fake.server.URL}

	out, errOut, code := runGoat(t, workspace, "what is 2+2", env, "-p", "-m", "claude-opus-5-5")
	if code != 0 || strings.TrimSpace(out) != "The answer is 4." {
		t.Fatalf("stdout %q stderr %q exit %d", out, errOut, code)
	}
	if matches, _ := filepath.Glob(workspace + "/state/goat/projects/*/session.json"); len(matches) != 0 {
		t.Error("expected -p to leave the session alone")
	}
	if !strings.Contains(fake.request(0).raw, "what is 2+2") {
		t.Errorf("expected the piped prompt to be sent: %s", fake.request(0).raw)
	}

	// Nothing listens on port 1, so this run fails and says so.
	_, errOut, code = runGoat(t, workspace, "", []string{"ANTHROPIC_API_KEY=test", "ANTHROPIC_BASE_URL=http://127.0.0.1:1"}, "-p", "again")
	if code != 1 || errOut == "" {
		t.Fatalf("expected a failing run to exit 1 with an error, got %d %q", code, errOut)
	}
}

func TestProcessMovesALegacyDotGoatOut(t *testing.T) {
	workspace := t.TempDir()
	os.MkdirAll(workspace+"/.goat", 0o755)
	os.WriteFile(workspace+"/.goat/prompts.jsonl", []byte("\"an old prompt\"\n"), 0o600)
	out, _, code := runGoat(t, workspace, "/exit\n", []string{"ANTHROPIC_API_KEY=test"})
	if code != 0 || !strings.Contains(out, "Moved goat's files from .goat to") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(workspace + "/.goat"); !os.IsNotExist(err) {
		t.Fatal("expected .goat to be gone")
	}
	matches, _ := filepath.Glob(workspace + "/state/goat/projects/*/prompts.jsonl")
	if len(matches) != 1 {
		t.Fatalf("expected the prompts in the state directory, found %v", matches)
	}
}
