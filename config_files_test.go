package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
)

// configTest gives a test an empty config directory and clears the credentials, the
// settings config.json can set, and what the loaders remember. It returns the config
// directory.
func configTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	for _, name := range append([]string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "DEEPSEEK_API_KEY", "GOAT_THEME", "GOAT_AUTO_APPROVE_BASH", "GOAT_CODEMODE", "GOAT_AUTO_COMPACT_TOKENS", "GOAT_ANTHROPIC_FALLBACKS", "OPENAI_BASE_URL", "ANTHROPIC_BASE_URL"}, provider.PresetKeyVars()...) {
		t.Setenv(name, "")
		t.Setenv(name+"_COMMAND", "")
	}
	saved := struct {
		sources                map[string]string
		commands, origins      map[string]string
		settled                map[string]bool
		startupModel, startupE string
	}{envSources, authCommands, authOrigins, authSettled, configStartup.model, configStartup.effort}
	envSources, authCommands, authOrigins, authSettled = map[string]string{}, map[string]string{}, map[string]string{}, map[string]bool{}
	configStartup.model, configStartup.effort = "", ""
	provider.ClearStoredKeys()
	provider.ResetEntries()
	t.Cleanup(func() {
		envSources, authCommands, authOrigins, authSettled = saved.sources, saved.commands, saved.origins, saved.settled
		configStartup.model, configStartup.effort = saved.startupModel, saved.startupE
		provider.ClearStoredKeys()
		provider.ResetEntries()
	})
	os.MkdirAll(filepath.Join(dir, "goat"), 0o700)
	return filepath.Join(dir, "goat")
}

func writeConfigFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// loadAll runs the loaders in startup order.
func loadAll(t *testing.T) []string {
	t.Helper()
	entries, warnings, err := loadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	more, err := loadProviderEntries(entries)
	if err != nil {
		t.Fatal(err)
	}
	warnings = append(warnings, more...)
	more, err = loadAuthFile()
	if err != nil {
		t.Fatal(err)
	}
	warnings = append(warnings, more...)
	more, err = loadGlobalEnv()
	if err != nil {
		t.Fatal(err)
	}
	warnings = append(warnings, more...)
	return append(warnings, runKeyCommands()...)
}

func TestAuthFileKeysStayOutOfTheEnvironment(t *testing.T) {
	dir := configTest(t)
	writeConfigFile(t, dir, "auth.json", `{"openai": {"key": "sk-from-auth"}, "deepseek": "ds-from-auth", "anthropic": {"auth_token": "tok-from-auth"}}`)
	if warnings := loadAll(t); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	for _, name := range []string{provider.OpenAI, provider.DeepSeek, provider.Anthropic} {
		if source := provider.CredentialSource(name); source != provider.StoredSource {
			t.Errorf("%s: got source %q", name, source)
		}
	}
	if _, value, token := provider.Credential(provider.Anthropic); value != "tok-from-auth" || !token {
		t.Errorf("expected Anthropic's auth token, got %q token=%v", value, token)
	}
	for _, entry := range os.Environ() {
		if strings.Contains(entry, "from-auth") {
			t.Fatalf("a key from auth.json reached the environment: %s", strings.SplitN(entry, "=", 2)[0])
		}
	}
	if origin := credentialOrigin(provider.OpenAI); origin != "key from "+displayPath(filepath.Join(dir, "auth.json")) {
		t.Errorf("got origin %q", origin)
	}
}

func TestKeyPrecedenceShellThenAuthThenGlobalEnv(t *testing.T) {
	t.Run("a shell key beats auth.json", func(t *testing.T) {
		dir := configTest(t)
		t.Setenv("OPENAI_API_KEY", "sk-shell")
		writeConfigFile(t, dir, "auth.json", `{"openai": {"key": "sk-auth"}}`)
		loadAll(t)
		if source, value, _ := provider.Credential(provider.OpenAI); source != "OPENAI_API_KEY" || value != "sk-shell" {
			t.Fatalf("got %s=%s", source, value)
		}
	})
	t.Run("a shell key command beats a key in auth.json", func(t *testing.T) {
		keyCommandsTest(t)
		dir := configTest(t)
		t.Setenv("OPENAI_API_KEY_COMMAND", "echo sk-shell-command")
		writeConfigFile(t, dir, "auth.json", `{"openai": {"key": "sk-auth"}}`)
		loadAll(t)
		if _, value, _ := provider.Credential(provider.OpenAI); value != "sk-shell-command" {
			t.Fatalf("got %s", value)
		}
	})
	t.Run("auth.json beats the global .env", func(t *testing.T) {
		dir := configTest(t)
		writeConfigFile(t, dir, "auth.json", `{"openai": {"key": "sk-auth"}}`)
		writeConfigFile(t, dir, ".env", "OPENAI_API_KEY=sk-env\nOPENAI_API_KEY_COMMAND=echo sk-env-command\n")
		loadAll(t)
		if source, value, _ := provider.Credential(provider.OpenAI); source != provider.StoredSource || value != "sk-auth" || os.Getenv("OPENAI_API_KEY") != "" {
			t.Fatalf("got %s=%s", source, value)
		}
	})
	t.Run("an auth.json key command beats a key in the global .env", func(t *testing.T) {
		keyCommandsTest(t)
		dir := configTest(t)
		writeConfigFile(t, dir, "auth.json", `{"openai": {"key_command": "printf 'sk-auth-command\\n'"}}`)
		writeConfigFile(t, dir, ".env", "OPENAI_API_KEY=sk-env\n")
		if warnings := loadAll(t); len(warnings) != 0 {
			t.Fatal(warnings)
		}
		if source, value, _ := provider.Credential(provider.OpenAI); source != provider.StoredSource || value != "sk-auth-command" || os.Getenv("OPENAI_API_KEY") != "" {
			t.Fatalf("got %s=%s", source, value)
		}
		if origin := credentialOrigin(provider.OpenAI); !strings.HasPrefix(origin, "key from key_command in ") {
			t.Fatalf("got origin %q", origin)
		}
	})
	t.Run("the global .env still works alone", func(t *testing.T) {
		dir := configTest(t)
		writeConfigFile(t, dir, ".env", "OPENAI_API_KEY=sk-env\n")
		loadAll(t)
		if source, value, _ := provider.Credential(provider.OpenAI); source != "OPENAI_API_KEY" || value != "sk-env" {
			t.Fatalf("got %s=%s", source, value)
		}
	})
}

func TestConfigFileSettingsSitBetweenTheShellAndGlobalEnv(t *testing.T) {
	dir := configTest(t)
	t.Setenv("GOAT_CODEMODE", "false")
	writeConfigFile(t, dir, "config.json", `{"theme": "one-dark", "code_mode": true, "auto_approve_bash": false, "auto_compact_tokens": 50000, "anthropic_fallbacks": false, "base_urls": {"anthropic": "https://proxy.example"}}`)
	writeConfigFile(t, dir, ".env", "GOAT_THEME=default\nGOAT_AUTO_COMPACT_TOKENS=1\n")
	loadAll(t)
	for name, want := range map[string]string{
		"GOAT_THEME":               "one-dark",
		"GOAT_CODEMODE":            "false", // the shell's
		"GOAT_AUTO_APPROVE_BASH":   "false",
		"GOAT_AUTO_COMPACT_TOKENS": "50000",
		"GOAT_ANTHROPIC_FALLBACKS": "off",
		"ANTHROPIC_BASE_URL":       "https://proxy.example",
	} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if variableOrigin("GOAT_THEME") != "GOAT_THEME from "+displayPath(filepath.Join(dir, "config.json")) {
		t.Errorf("got %q", variableOrigin("GOAT_THEME"))
	}
}

func TestConfigFileModelIsADefaultNotAnOverride(t *testing.T) {
	configTest(t)
	t.Setenv("OPENAI_API_KEY", "sk")
	t.Setenv("ANTHROPIC_API_KEY", "sk")
	t.Setenv("GOAT_MODEL", "")
	t.Setenv("GOAT_EFFORT", "")
	configStartup.model, configStartup.effort = "anthropic:claude-sonnet-5-5", "high"
	if model, source := startupModel(&savedSession{Model: "openai:gpt-6-luna"}, ""); model.String() != "openai:gpt-6-luna" || source != "saved session" {
		t.Fatalf("the saved session should win, got %s from %s", model, source)
	}
	if model, source := startupModel(nil, ""); model.String() != "anthropic:claude-sonnet-5-5" || source != "config.json" {
		t.Fatalf("got %s from %s", model, source)
	}
	if effort := startupEffort(&savedSession{Effort: "low"}, ""); effort != "low" {
		t.Fatalf("got %s", effort)
	}
	if effort := startupEffort(nil, ""); effort != "high" {
		t.Fatalf("got %s", effort)
	}
}

func TestConfigFileErrorsNameTheLineAndNeverAKey(t *testing.T) {
	for _, test := range []struct{ content, want string }{
		{`{"openai_api_key": "sk-SECRET"}`, `line 1: config.json has a field for a key "openai_api_key"; keys go in auth.json`},
		{"{\n  \"sk-proj-SECRET\": 1\n}", "line 2: config.json has a field for a key; keys go in auth.json"},
		{`{"colour": "red"}`, `an unknown field "colour"`},
		{`{"theme": "solarized"}`, "theme must be a built-in theme (default, one-dark) or the full path"},
		{`{"theme": "ondedark"}`, "full path of a theme file; did you mean one-dark?"},
		{`{"code_mode": "yes"}`, "code_mode must be true or false"},
		{`{"auto_compact_tokens": -5}`, "auto_compact_tokens must be a whole number above 0"},
		{`{"effort": "turbo"}`, "effort must be one of"},
		{`{"base_urls": {"anthropic": "https://me:SECRET@x.example"}}`, "base_urls.anthropic must not contain a user name or password"},
		{`{"base_urls": {"gemini": "https://x.example"}}`, "it takes openai, anthropic and deepseek"},
		{"{\n  \"theme\": \"one-dark\",\n  \"providers\": {\n    \"local\": {\"api\": \"openai\"}\n  }\n}", "line 4: local needs a base_url"},
		{`{"theme": "one-dark", "theme": "default"}`, `a name "theme" appears twice`},
		{"{\"theme\": sk-SECRET}", "line 1: not valid JSON"},
	} {
		dir := configTest(t)
		writeConfigFile(t, dir, "config.json", test.content)
		_, _, err := loadConfigFile()
		if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "config.json: ") {
			t.Errorf("%s: expected %q, got %v", test.content, test.want, err)
		} else if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: the error repeats the file: %v", test.content, err)
		}
	}
}

func TestAuthFileErrorsNeverQuoteIt(t *testing.T) {
	for _, test := range []struct{ content, want string }{
		{`{"openai": {"key": "sk-SECRET", "key_command": "x"}}`, "openai needs exactly one of key, auth_token, key_command"},
		{`{"openai": {"api_key": "sk-SECRET"}}`, "openai has an unknown field"},
		{`{"openai": {"key": ""}}`, "openai.key is empty"},
		{`{"openai": {"auth_token": "SECRET"}}`, "auth_token is only for anthropic"},
		{`{"openai": 12}`, "openai must be an object"},
		{"{\n\"openai\": {\"key\": sk-SECRET}}", "line 2: not valid JSON"},
	} {
		dir := configTest(t)
		writeConfigFile(t, dir, "auth.json", test.content)
		_, err := loadAuthFile()
		if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "auth.json: ") {
			t.Errorf("%s: expected %q, got %v", test.content, test.want, err)
		} else if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: the error repeats the file: %v", test.content, err)
		}
	}
}

func TestAuthFileWarnsAboutUnknownProvidersAndOpenPermissions(t *testing.T) {
	dir := configTest(t)
	writeConfigFile(t, dir, "auth.json", `{"gone": {"key": "x"}, "sk-SECRET-pasted": "y"}`)
	os.Chmod(filepath.Join(dir, "auth.json"), 0o644)
	warnings, err := loadAuthFile()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "open to other users") || !strings.Contains(joined, `the name "gone" is not a provider`) || strings.Contains(joined, "SECRET") {
		t.Fatalf("got %q", warnings)
	}
}

func TestAuthFileGivesAnEntryItsKey(t *testing.T) {
	dir := configTest(t)
	writeConfigFile(t, dir, "config.json", `{"providers": {"router": {"api": "openai", "base_url": "https://router.example/v1"}}}`)
	writeConfigFile(t, dir, "auth.json", `{"router": {"key": "rk-auth"}}`)
	loadAll(t)
	if source, value, _ := provider.Credential("router"); source != provider.StoredSource || value != "rk-auth" {
		t.Fatalf("got %s=%s", source, value)
	}
}

func TestProvidersMayNotBeDefinedInBothFiles(t *testing.T) {
	dir := configTest(t)
	writeConfigFile(t, dir, "config.json", `{"providers": {"local": {"api": "openai", "base_url": "http://localhost:1/v1"}}}`)
	writeConfigFile(t, dir, "providers.json", `{"local": {"api": "openai", "base_url": "http://localhost:2/v1"}}`)
	entries, _, err := loadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadProviderEntries(entries); err == nil || !strings.Contains(err.Error(), "local, which config.json's providers define too") {
		t.Fatalf("got %v", err)
	}
}

func TestProcessUsesTheJSONFiles(t *testing.T) {
	var gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Authorization")
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"pong\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	workDir := t.TempDir()
	config := filepath.Join(workDir, "config", "goat")
	os.MkdirAll(config, 0o700)
	os.WriteFile(filepath.Join(config, "config.json"), []byte(`{"theme": "one-dark", "model": "router:m", "providers": {"router": {"api": "openai", "base_url": "`+server.URL+`/v1"}}}`), 0o600)
	os.WriteFile(filepath.Join(config, "auth.json"), []byte(`{"router": {"key": "rk-auth"}}`), 0o600)

	out, errOut, code := runGoat(t, workDir, "ping", nil, "-p")
	if code != 0 || strings.TrimSpace(out) != "pong" || gotKey != "Bearer rk-auth" {
		t.Fatalf("exit %d, key %q\n%s\n%s", code, gotKey, out, errOut)
	}
	out, _, _ = runGoat(t, workDir, "/status\n/exit\n", nil)
	if !strings.Contains(out, "router:m") || !strings.Contains(out, "available (key from "+filepath.Join("~", "config", "goat", "auth.json")) {
		t.Fatalf("expected the model from config.json and the key from auth.json:\n%s", out)
	}
}

func TestConfigFileModelErrorsNeverRepeatIt(t *testing.T) {
	workDir := t.TempDir()
	config := filepath.Join(workDir, "config", "goat")
	os.MkdirAll(config, 0o700)
	os.WriteFile(filepath.Join(config, "config.json"), []byte(`{"model": "sk-proj-SECRET"}`), 0o600)
	_, errOut, _ := runGoat(t, workDir, "/exit\n", []string{"OPENAI_API_KEY=test"})
	if !strings.Contains(errOut, "ignoring the model in config.json") || strings.Contains(errOut, "SECRET") {
		t.Fatalf("got %q", errOut)
	}

	os.WriteFile(filepath.Join(config, "config.json"), []byte(`{"model": "anthropic:claude-opus-5-5"}`), 0o600)
	_, errOut, _ = runGoat(t, workDir, "/exit\n", []string{"OPENAI_API_KEY=test"})
	if !strings.Contains(errOut, "ignoring the model in config.json: anthropic models need") {
		t.Fatalf("a configured model without a key should be mentioned, got %q", errOut)
	}
}

func TestAuthFileCommandBeatsAnEntrysOwnCommand(t *testing.T) {
	keyCommandsTest(t)
	dir := configTest(t)
	marker := filepath.Join(t.TempDir(), "entry-ran")
	writeConfigFile(t, dir, "config.json", `{"providers": {"proxy": {"api": "openai", "base_url": "https://p.example/v1", "key_command": "touch `+marker+`; echo entry-key"}}}`)
	writeConfigFile(t, dir, "auth.json", `{"proxy": {"key_command": "echo auth-key"}}`)
	loadAll(t)
	if _, value, _ := provider.Credential("proxy"); value != "auth-key" {
		t.Fatalf("got %q", value)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the entry's own command should not run when auth.json supplies the entry")
	}
}

func TestAFailedAuthCommandLeavesTheProviderUnavailable(t *testing.T) {
	keyCommandsTest(t)
	dir := configTest(t)
	writeConfigFile(t, dir, "config.json", `{"providers": {"router": {"api": "openai", "base_url": "https://r.example/v1", "models": ["m"]}}}`)
	writeConfigFile(t, dir, "auth.json", `{"router": {"key_command": "exit 1"}, "openai": {"key_command": "exit 1"}}`)
	writeConfigFile(t, dir, ".env", "OPENAI_API_KEY=sk-env\n")
	if warnings := loadAll(t); len(warnings) != 2 {
		t.Fatalf("expected a warning per failed command, got %q", warnings)
	}
	for _, name := range []string{"router", provider.OpenAI} {
		if err := provider.Unavailable(name); err == nil || !strings.Contains(err.Error(), "its key_command in auth.json gave none") {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if provider.DefaultModel().Provider == "router" {
		t.Fatal("a keyless default must not be picked once its key command failed")
	}
}

func TestEntriesSharingABuiltInKeyFollowAuthFile(t *testing.T) {
	t.Run("the built-in's key in auth.json reaches the entry", func(t *testing.T) {
		dir := configTest(t)
		writeConfigFile(t, dir, "config.json", `{"providers": {"work": {"api": "openai", "base_url": "https://w.example/v1", "key_env": "OPENAI_API_KEY"}}}`)
		writeConfigFile(t, dir, "auth.json", `{"openai": {"key": "sk-auth"}}`)
		writeConfigFile(t, dir, ".env", "OPENAI_API_KEY=sk-env\n")
		loadAll(t)
		for _, name := range []string{provider.OpenAI, "work"} {
			if _, value, _ := provider.Credential(name); value != "sk-auth" {
				t.Errorf("%s: got %q", name, value)
			}
		}
	})
	t.Run("the entry's key in auth.json leaves the built-in its .env key", func(t *testing.T) {
		dir := configTest(t)
		writeConfigFile(t, dir, "config.json", `{"providers": {"work": {"api": "openai", "base_url": "https://w.example/v1", "key_env": "OPENAI_API_KEY"}}}`)
		writeConfigFile(t, dir, "auth.json", `{"work": {"key": "sk-work"}}`)
		writeConfigFile(t, dir, ".env", "OPENAI_API_KEY=sk-env\n")
		loadAll(t)
		if _, value, _ := provider.Credential(provider.OpenAI); value != "sk-env" {
			t.Errorf("openai: got %q", value)
		}
		if _, value, _ := provider.Credential("work"); value != "sk-work" {
			t.Errorf("work: got %q", value)
		}
	})
}

func TestEntryMessagesNameTheirFile(t *testing.T) {
	dir := configTest(t)
	writeConfigFile(t, dir, "config.json", `{"providers": {"broken": {"api": "openai", "base_url": "https://b.example/v1", "key_command": "exit 1"}}}`)
	keyCommandsTest(t)
	loadAll(t)
	if err := provider.Unavailable("broken"); err == nil || !strings.Contains(err.Error(), "key_command in config.json gave none") {
		t.Fatalf("got %v", err)
	}
	if err := provider.Unavailable("nope"); err == nil || strings.Contains(err.Error(), "providers.json") {
		t.Fatalf("got %v", err)
	}
}

func TestJSONFilesMayBeEmptyAndRefuseNull(t *testing.T) {
	dir := configTest(t)
	writeConfigFile(t, dir, "config.json", "  \n")
	writeConfigFile(t, dir, "auth.json", "")
	if warnings := loadAll(t); len(warnings) != 0 {
		t.Fatalf("got %q", warnings)
	}
	writeConfigFile(t, dir, "config.json", `{"auto_approve_bash": null}`)
	if _, _, err := loadConfigFile(); err == nil || !strings.Contains(err.Error(), "auto_approve_bash must be true or false") {
		t.Fatalf("got %v", err)
	}
}

func TestNestedConfigErrorsNameTheFilesLine(t *testing.T) {
	for _, test := range []struct{ content, want string }{
		{"{\n  \"theme\": \"one-dark\",\n  \"base_urls\": {\n    \"anthropic\": \"https://a.example\",\n    \"mistral\": \"https://m.example\"\n  }\n}", "line 5: base_urls has a provider"},
		{"{\n  \"providers\":\n  {\n    \"x\": {\"api\": \"openai\", \"base_url\": \"https://x.example\"},\n    \"x\": {\"api\": \"openai\", \"base_url\": \"https://y.example\"}\n  }\n}", `line 5: a name "x" appears twice`},
	} {
		dir := configTest(t)
		writeConfigFile(t, dir, "config.json", test.content)
		if _, _, err := loadConfigFile(); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("expected %q, got %v", test.want, err)
		}
	}
}

func TestAStoredAnthropicKeyIgnoresTheSDKsEnvironment(t *testing.T) {
	configTest(t)
	var header http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[],"has_more":false}`)
	}))
	t.Cleanup(server.Close)
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "Authorization: Bearer other-token")
	provider.StoreKey(provider.Anthropic, "sk-ant-stored", false)
	if _, err := provider.NewAnthropic().ListModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	if header.Get("X-Api-Key") != "sk-ant-stored" || header.Get("Authorization") != "" {
		t.Fatalf("expected only the stored key, got %v", header)
	}
}
