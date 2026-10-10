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
// it names, and forgets the sources loadEnvFiles records.
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
	_, global := envFilesTest(t, "GOAT_T_SHELL", "GOAT_T_PROJECT", "GOAT_T_GLOBAL", "GOAT_T_EMPTY")
	t.Setenv("GOAT_T_SHELL", "shell")
	os.WriteFile(".env", []byte("GOAT_T_SHELL=project\nGOAT_T_PROJECT=project\nGOAT_T_EMPTY=\n"), 0o600)
	os.WriteFile(global, []byte("GOAT_T_SHELL=global\nGOAT_T_PROJECT=global\nGOAT_T_GLOBAL=global\nexport GOAT_T_EMPTY=global\n"), 0o600)

	warnings, err := loadEnvFiles()
	if err != nil || len(warnings) != 0 {
		t.Fatalf("got %v %v", warnings, err)
	}
	for name, want := range map[string]string{
		"GOAT_T_SHELL":   "shell",   // the environment wins
		"GOAT_T_PROJECT": "project", // then the workspace file
		"GOAT_T_GLOBAL":  "global",  // then the global file
		"GOAT_T_EMPTY":   "global",  // an empty value does not count
	} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if envSources["GOAT_T_SHELL"] != "" || envSources["GOAT_T_PROJECT"] != ".env" || envSources["GOAT_T_GLOBAL"] != displayPath(global) {
		t.Errorf("sources %v", envSources)
	}
}

func TestEnvFilesFillAnEmptyShellVariable(t *testing.T) {
	_, global := envFilesTest(t, "DEEPSEEK_API_KEY")
	os.WriteFile(global, []byte("DEEPSEEK_API_KEY=sk-global\n"), 0o600)
	if _, err := loadEnvFiles(); err != nil {
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
	_, global := envFilesTest(t, "GOAT_T_PROJECT_ONLY", "GOAT_T_GLOBAL_ONLY")
	os.WriteFile(".env", []byte("GOAT_T_PROJECT_ONLY=1\n"), 0o600)
	os.WriteFile(global, []byte("GOAT_T_GLOBAL_ONLY=1\n"), 0o600)
	if _, err := loadEnvFiles(); err != nil {
		t.Fatal(err)
	}
	env := workspace.CommandEnvironment(os.Environ())
	if slices.ContainsFunc(env, func(entry string) bool {
		return strings.HasPrefix(entry, "GOAT_T_PROJECT_ONLY=") || strings.HasPrefix(entry, "GOAT_T_GLOBAL_ONLY=")
	}) {
		t.Error("variables from either .env file must not reach run_bash")
	}
}

func TestEnvFilesWarnAboutASharedGlobalFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes mean nothing on Windows")
	}
	_, global := envFilesTest(t, "GOAT_T_KEY")
	os.WriteFile(global, []byte("GOAT_T_KEY=1\n"), 0o644)
	os.Chmod(global, 0o644)
	warnings, err := loadEnvFiles()
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "chmod 600 "+global) {
		t.Fatalf("got %v %v", warnings, err)
	}

	os.Chmod(global, 0o600)
	if warnings, _ := loadEnvFiles(); len(warnings) != 0 {
		t.Errorf("a private file should not warn: %v", warnings)
	}
}

func TestEnvFilesRefuseABrokenFile(t *testing.T) {
	_, global := envFilesTest(t)
	os.WriteFile(global, []byte("GOAT_T_KEY=\"unterminated\n"), 0o600)
	if _, err := loadEnvFiles(); err == nil || !strings.Contains(err.Error(), global) {
		t.Fatalf("expected an error naming %s, got %v", global, err)
	}
}

func TestEnvFilesWithoutFiles(t *testing.T) {
	envFilesTest(t)
	if warnings, err := loadEnvFiles(); err != nil || len(warnings) != 0 || len(envSources) != 0 {
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
	if code == 0 || !strings.Contains(errOut, filepath.Join(workDir, "config", "goat", ".env")+" to use it from every folder") {
		t.Fatalf("the missing-key error should name the global file, got %q (exit %d)", errOut, code)
	}
}

// runGoatWithHome is runGoat with HOME and the config directory under home rather than
// the workspace.
func runGoatWithHome(t *testing.T, workDir, home, stdin string) (string, string, int) {
	t.Helper()
	return runGoat(t, workDir, stdin, []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, "config")})
}

func TestEnvFilesSettleACredentialAsAWhole(t *testing.T) {
	_, global := envFilesTest(t, "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL")
	os.WriteFile(global, []byte("ANTHROPIC_API_KEY=global-key\n"), 0o600)

	// The project uses a proxy with a token; the global key must not be sent there.
	os.WriteFile(".env", []byte("ANTHROPIC_AUTH_TOKEN=proxy-token\nANTHROPIC_BASE_URL=https://proxy.example\n"), 0o600)
	if _, err := loadEnvFiles(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" || credentialOrigin("anthropic") != "ANTHROPIC_AUTH_TOKEN from .env" {
		t.Fatalf("the project's token should win as a whole, got key %q, origin %q", os.Getenv("ANTHROPIC_API_KEY"), credentialOrigin("anthropic"))
	}
}

func TestEnvFilesKeepAShellCredential(t *testing.T) {
	_, global := envFilesTest(t, "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "shell-token")
	os.WriteFile(global, []byte("ANTHROPIC_API_KEY=global-key\n"), 0o600)
	if _, err := loadEnvFiles(); err != nil {
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
	} {
		t.Run(name, func(t *testing.T) {
			_, global := envFilesTest(t)
			os.WriteFile(global, []byte(content), 0o600)
			_, err := loadEnvFiles()
			if err == nil || !strings.Contains(err.Error(), global) || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("got %v", err)
			}
			if name == "pasted key" && !strings.Contains(err.Error(), "check line 1") {
				t.Errorf("expected the line, got %v", err)
			}
		})
	}
}

func TestEnvFilesAcceptAByteOrderMark(t *testing.T) {
	_, global := envFilesTest(t, "GOAT_T_BOM")
	os.WriteFile(global, []byte("\xef\xbb\xbfGOAT_T_BOM=yes\r\n"), 0o600)
	if _, err := loadEnvFiles(); err != nil || os.Getenv("GOAT_T_BOM") != "yes" {
		t.Fatalf("got %q %v", os.Getenv("GOAT_T_BOM"), err)
	}
}

func TestEnvFilesCannotMoveTheConfigDirectory(t *testing.T) {
	_, global := envFilesTest(t, "GOAT_T_WHERE")
	config := os.Getenv("XDG_CONFIG_HOME")
	os.WriteFile(".env", []byte("XDG_CONFIG_HOME=/elsewhere\n"), 0o600)
	os.WriteFile(global, []byte("GOAT_T_WHERE=global\n"), 0o600)
	if _, err := loadEnvFiles(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("XDG_CONFIG_HOME") != config || os.Getenv("GOAT_T_WHERE") != "global" {
		t.Fatalf("XDG_CONFIG_HOME %q, GOAT_T_WHERE %q", os.Getenv("XDG_CONFIG_HOME"), os.Getenv("GOAT_T_WHERE"))
	}
}

func TestEnvFilesLeaveShellValuesToCommands(t *testing.T) {
	_, global := envFilesTest(t, "GOAT_T_SHELL_PROXY")
	t.Setenv("GOAT_T_SHELL_PROXY", "http://proxy:3128")
	os.WriteFile(global, []byte("GOAT_T_SHELL_PROXY=http://other:3128\n"), 0o600)
	if _, err := loadEnvFiles(); err != nil {
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
	out, errOut, code := runGoat(t, workDir, "/status\n/exit\n", nil)
	if code != 0 || !strings.Contains(out, "\x1b[") {
		t.Fatalf("GOAT_FORCE_COLOR from the global file should style output (exit %d)\n%q\n%s", code, out, errOut)
	}
}
