package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/azs06/goat/internal/workspace"
)

// keyCommandsTest clears the credentials and their commands, and forgets the sources
// runKeyCommands records. Commands are written for sh, so it skips on Windows; the
// Windows shell is only checked by the cross-build.
func keyCommandsTest(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the test commands are sh")
	}
	for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "DEEPSEEK_API_KEY"} {
		t.Setenv(name, "")
		t.Setenv(name+"_COMMAND", "")
	}
	previous := envSources
	envSources = map[string]string{}
	t.Cleanup(func() { envSources = previous })
}

func TestKeyCommandFillsTheKey(t *testing.T) {
	keyCommandsTest(t)
	t.Setenv("OPENAI_API_KEY_COMMAND", `printf 'sk-from-command\n'`)
	if warnings := runKeyCommands(); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	if os.Getenv("OPENAI_API_KEY") != "sk-from-command" || credentialOrigin("openai") != "OPENAI_API_KEY from OPENAI_API_KEY_COMMAND" {
		t.Fatalf("got key %q, origin %q", os.Getenv("OPENAI_API_KEY"), credentialOrigin("openai"))
	}
	if slices.Contains(workspace.CommandEnvironment(os.Environ()), "OPENAI_API_KEY=sk-from-command") {
		t.Error("a key from a command must not reach run_bash")
	}
}

func TestKeyCommandIsNotRunWhenTheKeyIsSet(t *testing.T) {
	keyCommandsTest(t)
	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "shell-token")
	t.Setenv("ANTHROPIC_API_KEY_COMMAND", "touch "+marker+"; echo sk-command")
	if warnings := runKeyCommands(); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	if _, err := os.Stat(marker); err == nil || os.Getenv("ANTHROPIC_API_KEY") != "" {
		t.Error("a provider with a credential should run none of its commands")
	}
}

func TestKeyCommandFailuresDoNotQuoteTheOutput(t *testing.T) {
	for command, want := range map[string]string{
		"echo sk-partial-SECRET; exit 3":        "OPENAI_API_KEY_COMMAND failed: exit status 3",
		"true":                                  "OPENAI_API_KEY_COMMAND printed nothing",
		"printf 'sk-SECRET-1\\nsk-SECRET-2\\n'": "OPENAI_API_KEY_COMMAND printed more than one line",
	} {
		t.Run(command, func(t *testing.T) {
			keyCommandsTest(t)
			t.Setenv("OPENAI_API_KEY_COMMAND", command)
			warnings := runKeyCommands()
			if len(warnings) != 1 || !strings.HasPrefix(warnings[0], want) || strings.Contains(warnings[0], "SECRET") {
				t.Fatalf("got %q", warnings)
			}
			if os.Getenv("OPENAI_API_KEY") != "" {
				t.Error("a failed command must not set the key")
			}
		})
	}
}

func TestKeyCommandTimesOut(t *testing.T) {
	keyCommandsTest(t)
	previous := keyCommandTimeout
	keyCommandTimeout = 200 * time.Millisecond
	t.Cleanup(func() { keyCommandTimeout = previous })
	t.Setenv("DEEPSEEK_API_KEY_COMMAND", "sleep 5; echo sk-late")
	start := time.Now()
	warnings := runKeyCommands()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "gave no key within") || time.Since(start) > 3*time.Second {
		t.Fatalf("got %q after %s", warnings, time.Since(start))
	}
}

func TestKeyCommandsTryTheNextCredential(t *testing.T) {
	keyCommandsTest(t)
	t.Setenv("ANTHROPIC_API_KEY_COMMAND", "exit 1")
	t.Setenv("ANTHROPIC_AUTH_TOKEN_COMMAND", "echo proxy-token")
	if warnings := runKeyCommands(); len(warnings) != 1 {
		t.Fatalf("expected the failed command to warn, got %q", warnings)
	}
	if os.Getenv("ANTHROPIC_AUTH_TOKEN") != "proxy-token" || os.Getenv("ANTHROPIC_API_KEY") != "" {
		t.Error("the token command should settle the provider")
	}
}

func TestProcessSendsTheKeyFromACommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test command is sh")
	}
	workDir := t.TempDir()
	fake := newFakeAnthropic(t, newSSE("msg_1").text("ok").stop("end_turn", ""))
	config := filepath.Join(workDir, "config", "goat")
	os.MkdirAll(config, 0o700)
	os.WriteFile(filepath.Join(config, ".env"), []byte("ANTHROPIC_API_KEY_COMMAND=\"printf 'sk-ant-from-command\\\\n'\"\nANTHROPIC_BASE_URL="+fake.server.URL+"\n"), 0o600)
	out, errOut, code := runGoat(t, workDir, "hi", nil, "-p", "-m", "claude-opus-5-5")
	if code != 0 || strings.TrimSpace(out) != "ok" {
		t.Fatalf("stdout %q stderr %q exit %d", out, errOut, code)
	}
	if key := fake.request(0).header.Get("X-Api-Key"); key != "sk-ant-from-command" {
		t.Errorf("the request carried %q", key)
	}
}

func TestProcessExplainsAFailedKeyCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test command is sh")
	}
	_, errOut, code := runGoat(t, t.TempDir(), "", []string{"OPENAI_API_KEY_COMMAND=exit 7"})
	if code == 0 || !strings.Contains(errOut, "OPENAI_API_KEY_COMMAND failed: exit status 7") || !strings.Contains(errOut, "no API key found") {
		t.Fatalf("got %q (exit %d)", errOut, code)
	}
}

func TestProcessKeepsPipedInputFromAKeyCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test command is sh")
	}
	workDir := t.TempDir()
	fake := newFakeAnthropic(t, newSSE("msg_1").text("ok").stop("end_turn", ""))
	// cat would swallow the piped prompt if the command shared goat's input.
	env := []string{"ANTHROPIC_API_KEY_COMMAND=cat >/dev/null; echo sk-test", "ANTHROPIC_BASE_URL=" + fake.server.URL}
	out, errOut, code := runGoat(t, workDir, "what is 2+2", env, "-p", "-m", "claude-opus-5-5")
	if code != 0 || strings.TrimSpace(out) != "ok" || !strings.Contains(fake.request(0).raw, "what is 2+2") {
		t.Fatalf("stdout %q stderr %q exit %d", out, errOut, code)
	}
}

func TestKeyCommandsAreHiddenFromCommands(t *testing.T) {
	env := workspace.CommandEnvironment([]string{"OPENAI_API_KEY_COMMAND=echo sk-literal", "ANTHROPIC_AUTH_TOKEN_COMMAND=op read x", "MY_SERVICE_API_KEY_COMMAND=x", "BUILD_COMMAND=make"})
	if !slices.Equal(env, []string{"BUILD_COMMAND=make"}) {
		t.Errorf("got %q", env)
	}
}

func TestKeyCommandMayLeaveAHelperRunning(t *testing.T) {
	keyCommandsTest(t)
	t.Setenv("OPENAI_API_KEY_COMMAND", "echo sk-early; sleep 5 &")
	if warnings := runKeyCommands(); len(warnings) != 0 || os.Getenv("OPENAI_API_KEY") != "sk-early" {
		t.Fatalf("got %q, key %q", warnings, os.Getenv("OPENAI_API_KEY"))
	}
}

func TestAShellKeyCommandBeatsTheGlobalFile(t *testing.T) {
	keyCommandsTest(t)
	_, global := envFilesTest(t)
	t.Setenv("OPENAI_API_KEY_COMMAND", "echo sk-from-command")
	os.WriteFile(global, []byte("OPENAI_API_KEY=sk-from-file\n"), 0o600)
	if _, err := loadGlobalEnv(); err != nil {
		t.Fatal(err)
	}
	runKeyCommands()
	if os.Getenv("OPENAI_API_KEY") != "sk-from-command" {
		t.Errorf("the shell's command should win, got %q", os.Getenv("OPENAI_API_KEY"))
	}
}

func TestBaseURLsAreHiddenFromCommands(t *testing.T) {
	env := workspace.CommandEnvironment([]string{"OPENAI_BASE_URL=https://proxy.example/t/FAKE/v1", "anthropic_base_url=https://p", "SITE_URL=https://x"})
	if !slices.Equal(env, []string{"SITE_URL=https://x"}) {
		t.Errorf("got %q", env)
	}
}

func TestAShellKeyCommandKeepsTheFilesCommandsOut(t *testing.T) {
	keyCommandsTest(t)
	_, global := envFilesTest(t)
	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("ANTHROPIC_AUTH_TOKEN_COMMAND", "echo shell-token")
	os.WriteFile(global, []byte("ANTHROPIC_API_KEY_COMMAND=\"touch "+marker+"; echo file-key\"\n"), 0o600)
	if _, err := loadGlobalEnv(); err != nil {
		t.Fatal(err)
	}
	runKeyCommands()
	if _, err := os.Stat(marker); err == nil || os.Getenv("ANTHROPIC_AUTH_TOKEN") != "shell-token" || os.Getenv("ANTHROPIC_API_KEY") != "" {
		t.Errorf("the shell's command should settle Anthropic; token %q, key %q", os.Getenv("ANTHROPIC_AUTH_TOKEN"), os.Getenv("ANTHROPIC_API_KEY"))
	}
}

func TestProcessKeepsKeyCommandErrorsOffARedirectedStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test command is sh")
	}
	// runGoat captures stderr, so it is not a terminal, as when it goes to a log.
	_, errOut, _ := runGoat(t, t.TempDir(), "", []string{"OPENAI_API_KEY_COMMAND=echo sk-FAKE-SECRET >&2; exit 1"})
	if strings.Contains(errOut, "FAKE-SECRET") || !strings.Contains(errOut, "OPENAI_API_KEY_COMMAND failed") {
		t.Fatalf("got %q", errOut)
	}
}
