package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/workspace"
)

// envFilesTest gives a test a workspace and a config directory, clears the variables
// it names, and forgets the sources loadGlobalEnv records.
func envFilesTest(t *testing.T, names ...string) (workDir, globalEnv string) {
	t.Helper()
	workDir = t.TempDir()
	chdirForTest(t, workDir)
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	for _, name := range names {
		t.Setenv(name, "")
	}
	previous := envSources
	envSources = map[string]string{}
	t.Cleanup(func() { envSources = previous })
	os.MkdirAll(filepath.Join(config, "goat"), 0o700)
	return workDir, filepath.Join(config, "goat", ".env")
}

func TestEnvFilesPrecedence(t *testing.T) {
	_, global := envFilesTest(t, "GOAT_T_SHELL", "GOAT_T_GLOBAL", "GOAT_T_EMPTY")
	t.Setenv("GOAT_T_SHELL", "shell")
	os.WriteFile(global, []byte("GOAT_T_SHELL=global\nGOAT_T_GLOBAL=global\nexport GOAT_T_EMPTY=global\n"), 0o600)

	warnings, err := loadGlobalEnv()
	if err != nil || len(warnings) != 0 {
		t.Fatalf("got %v %v", warnings, err)
	}
	for name, want := range map[string]string{
		"GOAT_T_SHELL":  "shell",  // the environment wins
		"GOAT_T_GLOBAL": "global", // then the global file
		"GOAT_T_EMPTY":  "global", // an empty value in the environment does not count
	} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if envSources["GOAT_T_SHELL"] != "" || envSources["GOAT_T_GLOBAL"] != displayPath(global) {
		t.Errorf("sources %v", envSources)
	}
}

func TestEnvFilesIgnoreTheWorkspaceFile(t *testing.T) {
	_, global := envFilesTest(t, "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "GOAT_T_PROJECT")
	os.WriteFile(".env", []byte("ANTHROPIC_BASE_URL=https://attacker.example\nANTHROPIC_AUTH_TOKEN=project-token\nGOAT_T_PROJECT=1\n"), 0o600)
	os.WriteFile(global, []byte("ANTHROPIC_API_KEY=global-key\n"), 0o600)
	if _, err := loadGlobalEnv(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"ANTHROPIC_BASE_URL":   "",
		"ANTHROPIC_AUTH_TOKEN": "",
		"GOAT_T_PROJECT":       "",
		"ANTHROPIC_API_KEY":    "global-key",
	} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestEnvFilesFillAnEmptyShellVariable(t *testing.T) {
	_, global := envFilesTest(t, "DEEPSEEK_API_KEY")
	os.WriteFile(global, []byte("DEEPSEEK_API_KEY=sk-global\n"), 0o600)
	if _, err := loadGlobalEnv(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("DEEPSEEK_API_KEY") != "sk-global" {
		t.Fatal("an exported but empty key should be filled from the global file")
	}
	if origin := credentialOrigin("deepseek"); origin != "DEEPSEEK_API_KEY from "+displayPath(global) {
		t.Errorf("got %q", origin)
	}
}

func TestEnvFilesAreHiddenFromCommands(t *testing.T) {
	_, global := envFilesTest(t, "GOAT_T_GLOBAL_ONLY")
	os.WriteFile(global, []byte("GOAT_T_GLOBAL_ONLY=1\n"), 0o600)
	if _, err := loadGlobalEnv(); err != nil {
		t.Fatal(err)
	}
	env := workspace.CommandEnvironment(os.Environ())
	if slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, "GOAT_T_GLOBAL_ONLY=") }) {
		t.Error("variables from the global .env must not reach run_bash")
	}
}

func TestEnvFilesWarnAboutASharedGlobalFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes mean nothing on Windows")
	}
	_, global := envFilesTest(t, "GOAT_T_KEY")
	os.WriteFile(global, []byte("GOAT_T_KEY=1\n"), 0o644)
	os.Chmod(global, 0o644)
	warnings, err := loadGlobalEnv()
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "chmod 600 "+global) {
		t.Fatalf("got %v %v", warnings, err)
	}

	os.Chmod(global, 0o600)
	if warnings, _ := loadGlobalEnv(); len(warnings) != 0 {
		t.Errorf("a private file should not warn: %v", warnings)
	}
}

func TestEnvFilesRefuseABrokenFile(t *testing.T) {
	_, global := envFilesTest(t)
	os.WriteFile(global, []byte("GOAT_T_KEY=\"unterminated\n"), 0o600)
	if _, err := loadGlobalEnv(); err == nil || !strings.Contains(err.Error(), global) {
		t.Fatalf("expected an error naming %s, got %v", global, err)
	}
}

func TestEnvFilesWithoutFiles(t *testing.T) {
	envFilesTest(t)
	if warnings, err := loadGlobalEnv(); err != nil || len(warnings) != 0 || len(envSources) != 0 {
		t.Fatalf("got %v %v %v", warnings, err, envSources)
	}
}

func TestGoatConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	if dir := goatConfigDir(); dir != filepath.Join("/cfg", "goat") {
		t.Errorf("got %q", dir)
	}
	if path := globalEnvPath(); path != filepath.Join("/cfg", "goat", ".env") {
		t.Errorf("got %q", path)
	}
	if path := userPricesPath(); path != filepath.Join("/cfg", "goat", "prices.json") {
		t.Errorf("got %q", path)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	home, _ := os.UserHomeDir()
	if dir := goatConfigDir(); dir != filepath.Join(home, ".config", "goat") {
		t.Errorf("got %q", dir)
	}
	if shown := displayPath(filepath.Join(home, ".config", "goat", ".env")); shown != filepath.Join("~", ".config", "goat", ".env") {
		t.Errorf("got %q", shown)
	}
}

func TestProcessUsesTheGlobalEnvFile(t *testing.T) {
	workDir := t.TempDir()
	config := filepath.Join(workDir, "config", "goat")
	os.MkdirAll(config, 0o700)
	os.WriteFile(filepath.Join(config, ".env"), []byte("ANTHROPIC_API_KEY=test\n"), 0o600)

	project := filepath.Join(workDir, "project")
	os.MkdirAll(project, 0o755)
	out, errOut, code := runGoatWithHome(t, project, workDir, "/status\n/exit\n")
	if code != 0 || !strings.Contains(out, "ANTHROPIC_API_KEY from "+filepath.Join("~", "config", "goat", ".env")) {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if strings.Contains(errOut, "chmod") {
		t.Errorf("a private file should not warn: %s", errOut)
	}
}

func TestProcessNamesTheGlobalEnvFile(t *testing.T) {
	workDir := t.TempDir()
	_, errOut, code := runGoat(t, workDir, "", nil)
	if code == 0 || !strings.Contains(errOut, "or in "+filepath.Join(workDir, "config", "goat", ".env")) || strings.Contains(errOut, "does not read") {
		t.Fatalf("the missing-key error should name the global file, got %q (exit %d)", errOut, code)
	}

	// Keys left in the project's .env are the likely mistake, so the error says why they
	// were not used.
	os.WriteFile(filepath.Join(workDir, ".env"), []byte("ANTHROPIC_API_KEY=test\n"), 0o600)
	_, errOut, code = runGoat(t, workDir, "", nil)
	if code == 0 || !strings.Contains(errOut, "goat does not read the .env in this folder") {
		t.Fatalf("expected the workspace .env to be ignored and named, got %q (exit %d)", errOut, code)
	}
}

func TestProcessSendsKeysOnlyWhereTheGlobalFileSays(t *testing.T) {
	workDir := t.TempDir()
	fake := newFakeAnthropic(t, newSSE("msg_1").text("ok").stop("end_turn", ""))
	config := filepath.Join(workDir, "config", "goat")
	os.MkdirAll(config, 0o700)
	os.WriteFile(filepath.Join(config, ".env"), []byte("ANTHROPIC_API_KEY=test\nANTHROPIC_BASE_URL="+fake.server.URL+"\n"), 0o600)
	// A cloned repository pointing the provider elsewhere; nothing listens on port 1.
	os.WriteFile(filepath.Join(workDir, ".env"), []byte("ANTHROPIC_BASE_URL=http://127.0.0.1:1\n"), 0o600)

	out, errOut, code := runGoat(t, workDir, "hi", nil, "-p", "-m", "claude-opus-5-5")
	if code != 0 || strings.TrimSpace(out) != "ok" {
		t.Fatalf("the request should go to the global file's URL: stdout %q stderr %q exit %d", out, errOut, code)
	}
}

// runGoatWithHome is runGoat with HOME and the config directory under home rather than
// the workspace.
func runGoatWithHome(t *testing.T, workDir, home, stdin string) (string, string, int) {
	t.Helper()
	return runGoat(t, workDir, stdin, []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, "config")})
}

func TestEnvFilesKeepAShellCredential(t *testing.T) {
	_, global := envFilesTest(t, "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "shell-token")
	os.WriteFile(global, []byte("ANTHROPIC_API_KEY=global-key\n"), 0o600)
	if _, err := loadGlobalEnv(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		t.Fatal("a token from the shell should keep the global key out")
	}
}

func TestEnvFileErrorsDoNotQuoteTheFile(t *testing.T) {
	for name, content := range map[string]string{
		"unclosed quote": "OPENAI_API_KEY=\"sk-SECRET-1\nDEEPSEEK_API_KEY=sk-SECRET-2\n",
		"pasted key":     "sk-proj-SECRET-1\nANTHROPIC_API_KEY=sk-ant-SECRET-2\n",
		"pasted last":    "ANTHROPIC_API_KEY=sk-ant-SECRET-2\nsk_live_SECRET_1",
	} {
		t.Run(name, func(t *testing.T) {
			_, global := envFilesTest(t)
			os.WriteFile(global, []byte(content), 0o600)
			_, err := loadGlobalEnv()
			if err == nil || !strings.Contains(err.Error(), global) || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("got %v", err)
			}
			want := map[string]string{"pasted key": "check line 1", "pasted last": "check line 2"}[name]
			if want != "" && !strings.Contains(err.Error(), want) {
				t.Errorf("expected %q, got %v", want, err)
			}
		})
	}
}

func TestEnvFilesAcceptAByteOrderMark(t *testing.T) {
	_, global := envFilesTest(t, "GOAT_T_BOM")
	os.WriteFile(global, []byte("\xef\xbb\xbfGOAT_T_BOM=yes\r\n"), 0o600)
	if _, err := loadGlobalEnv(); err != nil || os.Getenv("GOAT_T_BOM") != "yes" {
		t.Fatalf("got %q %v", os.Getenv("GOAT_T_BOM"), err)
	}
}

func TestEnvFilesCannotMoveTheConfigDirectory(t *testing.T) {
	_, global := envFilesTest(t, "GOAT_T_WHERE")
	config, home := os.Getenv("XDG_CONFIG_HOME"), os.Getenv("HOME")
	os.WriteFile(global, []byte("XDG_CONFIG_HOME=/elsewhere\nHOME=/elsewhere\nGOAT_T_WHERE=global\n"), 0o600)
	if _, err := loadGlobalEnv(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("XDG_CONFIG_HOME") != config || os.Getenv("HOME") != home || os.Getenv("GOAT_T_WHERE") != "global" {
		t.Fatalf("XDG_CONFIG_HOME %q, HOME %q, GOAT_T_WHERE %q", os.Getenv("XDG_CONFIG_HOME"), os.Getenv("HOME"), os.Getenv("GOAT_T_WHERE"))
	}
}

func TestEnvFilesLeaveShellValuesToCommands(t *testing.T) {
	_, global := envFilesTest(t, "GOAT_T_SHELL_PROXY")
	t.Setenv("GOAT_T_SHELL_PROXY", "http://proxy:3128")
	os.WriteFile(global, []byte("GOAT_T_SHELL_PROXY=http://other:3128\n"), 0o600)
	if _, err := loadGlobalEnv(); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(workspace.CommandEnvironment(os.Environ()), "GOAT_T_SHELL_PROXY=http://proxy:3128") {
		t.Error("a value from the shell should still reach commands")
	}
}

func TestSharedFileWarningQuotesThePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes mean nothing on Windows")
	}
	dir := filepath.Join(t.TempDir(), "My Config")
	os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, ".env")
	os.WriteFile(path, nil, 0o644)
	os.Chmod(path, 0o644)
	if warning := sharedFileWarning(path); !strings.HasSuffix(warning, "chmod 600 '"+path+"'") {
		t.Errorf("got %q", warning)
	}
	if quoted := shellQuote("/a/it's"); quoted != `'/a/it'\''s'` {
		t.Errorf("got %s", quoted)
	}
}

func TestProcessReadsDisplaySettingsFromTheGlobalEnvFile(t *testing.T) {
	workDir := t.TempDir()
	config := filepath.Join(workDir, "config", "goat")
	os.MkdirAll(config, 0o700)
	os.WriteFile(filepath.Join(config, ".env"), []byte("ANTHROPIC_API_KEY=test\nGOAT_FORCE_COLOR=1\n"), 0o600)
	// Whatever the shell running the tests says about color.
	out, errOut, code := runGoat(t, workDir, "/status\n/exit\n", []string{"NO_COLOR=", "TERM=xterm-256color"})
	if code != 0 || !strings.Contains(out, "\x1b[") {
		t.Fatalf("GOAT_FORCE_COLOR from the global file should style output (exit %d)\n%q\n%s", code, out, errOut)
	}
}

func TestBadLineSkipsQuotedValuesThatSpanLines(t *testing.T) {
	data := []byte("A=1\nB=\"one\ntwo\"\nC=3\nnot a line\nD=4\n")
	if line := badLine(data); line != 5 {
		t.Errorf("got line %d, want 5", line)
	}
	if line := badLine([]byte("A=1\nB=2\n")); line != 0 {
		t.Errorf("a valid file has no bad line, got %d", line)
	}
}

func TestEnvFilesSkipAnUnreadableGlobalFile(t *testing.T) {
	_, global := envFilesTest(t)
	os.Remove(global)
	os.MkdirAll(global, 0o700) // a directory where the file should be
	warnings, err := loadGlobalEnv()
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "skipping") {
		t.Fatalf("got %v %v", warnings, err)
	}
}

func TestGoatConfigDirIgnoresARelativeXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "~/cfg")
	home, _ := os.UserHomeDir()
	if dir := goatConfigDir(); dir != filepath.Join(home, ".config", "goat") {
		t.Errorf("got %q", dir)
	}
}

func TestProcessRefusesABaseURLWithAPassword(t *testing.T) {
	workDir := t.TempDir()
	env := []string{"ANTHROPIC_API_KEY=test", "ANTHROPIC_BASE_URL=https://user:hunter2@proxy.example"}
	_, errOut, code := runGoat(t, workDir, "", env)
	if code == 0 || !strings.Contains(errOut, "no provider can be used; ANTHROPIC_BASE_URL must not contain a user name or password") || strings.Contains(errOut, "hunter2") {
		t.Fatalf("got %q (exit %d)", errOut, code)
	}
}

func TestProcessShowsWhereACustomEndpointSendsTheKey(t *testing.T) {
	workDir := t.TempDir()
	config := filepath.Join(workDir, "config", "goat")
	os.MkdirAll(config, 0o700)
	os.WriteFile(filepath.Join(config, ".env"), []byte("DEEPSEEK_API_KEY=test\nDEEPSEEK_BASE_URL=http://10.0.0.7:4000/t/sk-in-path/anthropic\n"), 0o600)
	out, errOut, code := runGoat(t, workDir, "/status\n/exit\n", nil)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	want := "deepseek requests go to http://10.0.0.7:4000 (DEEPSEEK_BASE_URL from " + filepath.Join("~", "config", "goat", ".env") + "), unencrypted"
	if !strings.Contains(errOut, want) {
		t.Errorf("startup should say where the key goes, want %q in:\n%s", want, errOut)
	}
	if !strings.Contains(out, "requests go to http://10.0.0.7:4000, unencrypted") {
		t.Errorf("/status should show the endpoint:\n%s", out)
	}
	if strings.Contains(out+errOut, "sk-in-path") {
		t.Error("the endpoint's path must not be shown")
	}
}
