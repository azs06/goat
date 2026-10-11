package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/azs06/goat/internal/provider"
)

func TestWriteAuthKeyCreatesAPrivateFile(t *testing.T) {
	dir := configTest(t)
	os.RemoveAll(dir)
	path := filepath.Join(dir, "auth.json")
	if err := writeAuthKey(path, provider.OpenAI, "sk-test-<&>"); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for target, want := range map[string]os.FileMode{path: 0o600, dir: 0o700} {
			if info, err := os.Stat(target); err != nil || info.Mode().Perm() != want {
				t.Fatalf("%s: mode %v, err %v; want %v", target, info.Mode().Perm(), err, want)
			}
		}
	}
	data, _ := os.ReadFile(path)
	if string(data) != "{\n  \"openai\": {\n    \"key\": \"sk-test-<&>\"\n  }\n}\n" {
		t.Fatalf("got %q", data)
	}
	if warnings := loadAll(t); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	if source, value, _ := provider.Credential(provider.OpenAI); source != provider.StoredSource || value != "sk-test-<&>" {
		t.Fatalf("the written file did not load back: %s", source)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".auth-*")); len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

func TestWriteAuthKeyKeepsTheRestOfTheFile(t *testing.T) {
	dir := configTest(t)
	path := filepath.Join(dir, "auth.json")
	writeConfigFile(t, dir, "auth.json", `{"$comment": "mine", "anthropic": {"key_command": "op read x"}, "openai": "sk-old", "deepseek": {"key": "ds"}}`)
	if err := writeAuthKey(path, provider.OpenAI, "sk-new"); err != nil {
		t.Fatal(err)
	}
	if err := writeAuthKey(path, "openrouter", "or-new"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	want := `{
  "$comment": "mine",
  "anthropic": {
    "key_command": "op read x"
  },
  "openai": {
    "key": "sk-new"
  },
  "deepseek": {
    "key": "ds"
  },
  "openrouter": {
    "key": "or-new"
  }
}
`
	if string(data) != want {
		t.Fatalf("got\n%s", data)
	}
	if kind, err := authEntryKind(path, provider.Anthropic); kind != "key_command" || err != nil {
		t.Fatalf("anthropic: %q, %v", kind, err)
	}
	if kind, _ := authEntryKind(path, "gemini"); kind != "" {
		t.Fatalf("gemini: %q", kind)
	}
}

func TestWriteAuthKeyRefusesABrokenFileWithoutQuotingIt(t *testing.T) {
	dir := configTest(t)
	path := filepath.Join(dir, "auth.json")
	broken := `{"openai": "sk-SECRET-1",` + "\n" + `"deepseek": sk-SECRET-2}`
	writeConfigFile(t, dir, "auth.json", broken)
	err := writeAuthKey(path, provider.OpenAI, "sk-new")
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("got %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != broken {
		t.Fatal("a broken file was changed")
	}
}

func TestWriteAuthKeyFollowsASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	dir := configTest(t)
	target := filepath.Join(t.TempDir(), "dotfiles-auth.json")
	os.WriteFile(target, []byte(`{"deepseek": "ds"}`), 0o600)
	link := filepath.Join(dir, "auth.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeAuthKey(link, provider.OpenAI, "sk-new"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced by a file")
	}
	if data, _ := os.ReadFile(target); !strings.Contains(string(data), "sk-new") || !strings.Contains(string(data), `"deepseek": "ds"`) {
		t.Fatalf("got %s", data)
	}
}

// keyFeed returns keys one at a time, then io.EOF.
func keyFeed(keys ...string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		if len(keys) == 0 {
			return "", io.EOF
		}
		key := keys[0]
		keys = keys[1:]
		return key, nil
	}
}

func TestCollectSecret(t *testing.T) {
	var counts []int
	shown := func(count int) { counts = append(counts, count) }
	secret, err := collectSecret(context.Background(), keyFeed("x", "\x15", "\x1b[200~", "s", "k", "-", "\x1b[D", "\x01", "1", "2", "\x7f", "\x1b[201~", "\r"), shown)
	if err != nil || secret != "sk-1" {
		t.Fatalf("got %q, %v", secret, err)
	}
	if want := []int{1, 0, 1, 2, 3, 4, 5, 4}; !slices.Equal(counts, want) {
		t.Fatalf("counts %v, want %v", counts, want)
	}

	for _, key := range []string{"\x1b", "\x03", "\x04"} {
		if _, err := collectSecret(context.Background(), keyFeed("a", key), shown); err != errSecretCancelled {
			t.Fatalf("%q: got %v", key, err)
		}
	}
	if _, err := collectSecret(context.Background(), keyFeed("a"), shown); err != io.EOF {
		t.Fatalf("got %v", err)
	}
}

func TestApplyLoginUsesTheKeyAtOnce(t *testing.T) {
	dir := configTest(t)
	// A key from the global .env is replaced, as auth.json would replace it at start.
	writeConfigFile(t, dir, ".env", "OPENAI_API_KEY=sk-from-env\n")
	loadAll(t)
	c := &config{providers: map[string]provider.Provider{provider.OpenAI: nil, provider.DeepSeek: nil}}
	if variable := applyLogin(c, provider.OpenAI, "sk-login"); variable != "" {
		t.Fatalf("a key from .env was taken for the shell's: %s", variable)
	}
	if source, value, _ := provider.Credential(provider.OpenAI); source != provider.StoredSource || value != "sk-login" {
		t.Fatalf("got %s", source)
	}
	if _, cached := c.providers[provider.OpenAI]; cached {
		t.Fatal("the client made with the old key was kept")
	}
	if _, cached := c.providers[provider.DeepSeek]; !cached {
		t.Fatal("another provider's client was dropped")
	}
	if origin := credentialOrigin(provider.OpenAI); origin != "key from "+displayPath(filepath.Join(dir, "auth.json")) {
		t.Fatalf("got %q", origin)
	}

	// The shell wins over auth.json, now as at the next start.
	t.Setenv("DEEPSEEK_API_KEY", "ds-shell")
	if variable := applyLogin(c, provider.DeepSeek, "ds-login"); variable != "DEEPSEEK_API_KEY" {
		t.Fatalf("got %q", variable)
	}
	if _, value, _ := provider.Credential(provider.DeepSeek); value != "ds-shell" {
		t.Fatal("the saved key beat the shell's")
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("DEEPSEEK_API_KEY_COMMAND", "echo ds")
	if variable := applyLogin(c, provider.DeepSeek, "ds-login"); variable != "DEEPSEEK_API_KEY_COMMAND" {
		t.Fatalf("got %q", variable)
	}
}

func TestPresetsNeedOnlyAKey(t *testing.T) {
	dir := configTest(t)
	writeConfigFile(t, dir, "config.json", `{"providers": {"mistral": {"api": "openai", "base_url": "http://localhost:11434/v1", "models": ["mistral:7b"]}}}`)
	writeConfigFile(t, dir, "auth.json", `{"openrouter": {"key": "or-key"}}`)
	if warnings := loadAll(t); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	if !provider.Available("openrouter") || provider.Available("gemini") {
		t.Fatalf("available: %v", provider.AvailableNames())
	}
	if provider.Label("openrouter") != "OpenRouter" || provider.Label(provider.OpenAI) != "OpenAI" || provider.Label("mistral") != "mistral" {
		t.Fatal("wrong labels")
	}
	// The user's entry replaces the preset of the same name.
	if entry, _ := provider.LookupEntry("mistral"); entry.Preset || entry.BaseURL != "http://localhost:11434/v1" {
		t.Fatalf("got %+v", entry)
	}
	if err := provider.Unavailable("gemini"); err == nil || !strings.Contains(err.Error(), "/login") || !strings.Contains(err.Error(), "GEMINI_API_KEY") {
		t.Fatalf("got %v", err)
	}
	if loginGroup("gemini") != "More providers" || loginGroup(provider.OpenAI) != "Providers" || loginGroup("mistral") != "Your providers (config.json)" {
		t.Fatal("wrong groups")
	}
	if loginStatus("gemini") != "no key" || !strings.HasPrefix(loginStatus("openrouter"), "key from ") {
		t.Fatalf("got %q, %q", loginStatus("gemini"), loginStatus("openrouter"))
	}
	completions := commandArgumentCompletions(&config{})("login", "open")
	if len(completions) != 2 || completions[1].display != "openrouter" || !strings.Contains(completions[1].description, "OpenRouter") {
		t.Fatalf("got %+v", completions)
	}
}

func TestStatusListsOnlyPresetsWithAKey(t *testing.T) {
	dir := configTest(t)
	writeConfigFile(t, dir, "auth.json", `{"groq": "gsk-test"}`)
	loadAll(t)
	out := captureStdout(t, func() {
		if err := commandStatus(context.Background(), &config{}, nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "groq") || strings.Contains(out, "gemini") || !strings.Contains(out, "openai") {
		t.Fatalf("got\n%s", out)
	}
}

func TestPresetNamesDoNotTakeAnEntrysModel(t *testing.T) {
	dir := configTest(t)
	writeConfigFile(t, dir, "config.json", `{"providers": {"ollama": {"api": "openai", "base_url": "http://localhost:11434/v1", "models": ["mistral:7b"]}}}`)
	loadAll(t)
	if model, err := provider.ResolveModel("mistral:7b"); err != nil || model.Provider != "ollama" {
		t.Fatalf("got %v, %v", model, err)
	}
	if model, err := provider.ResolveModel("mistral:mistral-large-latest"); err != nil || model.Provider != "mistral" {
		t.Fatalf("got %v, %v", model, err)
	}
}

func TestLoginWithoutATerminalSaysWhatToDo(t *testing.T) {
	out, _, code := runGoat(t, t.TempDir(), "/login\n/auth openai\n/exit\n", []string{"OPENAI_API_KEY=test"})
	if code != 0 || strings.Count(out, "needs an interactive terminal") != 2 || strings.Contains(out, "\x1b") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestSelectorSkipsDisabledOptions(t *testing.T) {
	s := &selector{options: []selectOption{{label: "off", disabled: true}, {label: "a"}, {label: "b", disabled: true}, {label: "c"}}}
	s.move(s.visible(), -1, 1)
	if s.selected != 1 {
		t.Fatalf("started on %d", s.selected)
	}
	s.handleKey("\x1b[B")
	if s.selected != 3 {
		t.Fatalf("Down landed on %d", s.selected)
	}
	s.handleKey("\x1b[B") // wraps past the disabled first option
	if s.selected != 1 {
		t.Fatalf("Down from the end landed on %d", s.selected)
	}
	s.handleKey("o") // only "off" matches, and it cannot be picked
	if done, _ := s.handleKey("\r"); done {
		t.Fatal("Enter picked a disabled option")
	}
	if !strings.Contains(s.render(80), "off") {
		t.Fatal("a disabled option should still be listed")
	}
}

func TestKeylessStartFailsOnlyWhereNobodyCanLogIn(t *testing.T) {
	for _, tc := range []struct{ keyless, print, terminal, fails bool }{
		{true, false, true, false}, // an interactive start goes on, so /login can add a key
		{true, true, true, true},
		{true, false, false, true},
		{false, true, false, false},
		{false, false, true, false},
	} {
		if got := keylessStartFails(tc.keyless, tc.print, tc.terminal); got != tc.fails {
			t.Errorf("%+v: got %v", tc, got)
		}
	}
}

func TestAKeylessStartLeavesTheSessionFilesAlone(t *testing.T) {
	chdirForTest(t, t.TempDir())
	setCredentials(t, false, false)
	var writes []string
	previous := sessionSaver
	sessionSaver = func(path string, _ savedSession) error { writes = append(writes, path); return nil }
	t.Cleanup(func() { sessionSaver = previous })

	saved := &savedSession{Model: "anthropic:claude-opus-5-5", Conversation: provider.Conversation{Provider: provider.Anthropic, Messages: []json.RawMessage{json.RawMessage(`{"n":1}`)}}}
	cfg := &config{providers: map[string]provider.Provider{}}
	restoreSavedConversation(cfg, saved)
	if err := commandClear(context.Background(), cfg, nil); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 0 || cfg.pendingSaved != saved {
		t.Fatalf("wrote %v before there was a model", writes)
	}

	// The first model restores the saved session as a start with it would have.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	if err := switchModel(context.Background(), cfg, "anthropic:claude-sonnet-5-5"); err != nil {
		t.Fatal(err)
	}
	if cfg.pendingSaved != nil || len(cfg.conversation.conversation.Messages) != 1 || !slices.Equal(writes, []string{sessionPath}) {
		t.Fatalf("writes %v, conversation %+v", writes, cfg.conversation)
	}
}

func TestModelWithoutAProviderPointsToLogin(t *testing.T) {
	configTest(t)
	out := captureStdout(t, func() {
		if err := commandModel(context.Background(), &config{interactive: true}, nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "/login") {
		t.Fatalf("got %q", out)
	}
}

func TestCollectSecretRefusesAMultiLinePaste(t *testing.T) {
	reader := newLineReader(strings.NewReader("\x1b[200~sk-ONE\rsk-TWO\r\x1b[201~next"))
	if _, err := collectSecret(context.Background(), reader.readKey, func(int) {}); err != errSecretLines {
		t.Fatalf("got %v", err)
	}
	// Nothing of the paste is left for the prompt.
	if line, _ := reader.ReadLine(context.Background()); line != "next" {
		t.Fatalf("left %q", line)
	}

	// Without bracketed paste, the lines behind the first arrive at once and are drained.
	reader = newLineReader(strings.NewReader("sk-ONE\rsk-TWO\r"))
	if secret, err := collectSecret(context.Background(), reader.readKey, func(int) {}); err != nil || secret != "sk-ONE" {
		t.Fatalf("got %q, %v", secret, err)
	}
	if !drainInput(context.Background(), reader.readKey, 50*time.Millisecond) {
		t.Fatal("expected the rest of the paste to be drained")
	}
	if drainInput(context.Background(), reader.readKey, 10*time.Millisecond) {
		t.Fatal("nothing should be left")
	}
}

func TestWriteAuthKeyFollowsALinkToAMissingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	dir := configTest(t)
	target := filepath.Join(t.TempDir(), "dotfiles", "auth.json")
	link := filepath.Join(dir, "auth.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeAuthKey(link, provider.OpenAI, "sk-new"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced by a file")
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("target: %v", err)
	}
}

func TestCheckAPIKey(t *testing.T) {
	if checkAPIKey("sk-abc_123.XYZ") != nil {
		t.Fatal("refused a key")
	}
	for _, bad := range []string{"sk-a b", "sk-a\tb"} {
		if err := checkAPIKey(bad); err == nil || strings.Contains(err.Error(), "sk-a") {
			t.Fatalf("%q: got %v", bad, err)
		}
	}
}

func TestLoginAsksBeforeReplacingAKeyCommandOrToken(t *testing.T) {
	for _, kind := range []string{"key_command", "auth_token"} {
		dir := configTest(t)
		content := `{"anthropic": {"` + kind + `": "op read x"}}`
		writeConfigFile(t, dir, "auth.json", content)
		c := &config{input: newLineReader(strings.NewReader("n\n")), interactive: true}
		out := captureStdout(t, func() {
			if err := commandLogin(context.Background(), c, []string{provider.Anthropic}); err != nil {
				t.Fatal(err)
			}
		})
		data, _ := os.ReadFile(filepath.Join(dir, "auth.json"))
		if string(data) != content || !strings.Contains(out, "replace it with an API key?") || !strings.Contains(out, "Nothing changed.") {
			t.Fatalf("%s: file %q, output:\n%s", kind, data, out)
		}
	}
}

func TestLoginShowsWhereTheKeyGoesFirst(t *testing.T) {
	configTest(t)
	t.Setenv("OPENAI_BASE_URL", "http://10.0.0.5:8080/v1")
	loadAll(t)
	c := &config{input: newLineReader(strings.NewReader("")), interactive: true}
	out := captureStdout(t, func() { commandLogin(context.Background(), c, []string{provider.OpenAI}) })
	if !strings.Contains(out, "unencrypted") || !strings.Contains(out, "http://10.0.0.5:8080") {
		t.Fatalf("got %q", out)
	}
}

// modelsServer answers /models with the given status and body.
func modelsServer(t *testing.T, status int, body string) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestCheckLoginPicksAModelOnlyWhenThereIsNone(t *testing.T) {
	chdirForTest(t, t.TempDir())
	configTest(t)
	server := modelsServer(t, 200, `{"object":"list","data":[{"id":"gpt-6-luna"}]}`)
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	loadAll(t)
	c := &config{providers: map[string]provider.Provider{}}
	applyLogin(c, provider.OpenAI, "sk-test")
	out := captureStdout(t, func() {
		if err := checkLogin(context.Background(), c, provider.OpenAI, "sk-test"); err != nil {
			t.Fatal(err)
		}
	})
	if c.model != (provider.Model{Provider: provider.OpenAI, ID: provider.DefaultOpenAIModel}) || !strings.Contains(out, "lists 1 models") {
		t.Fatalf("model %v, output %q", c.model, out)
	}

	c.model = provider.Model{Provider: provider.OpenAI, ID: "gpt-5.6"}
	captureStdout(t, func() { checkLogin(context.Background(), c, provider.OpenAI, "sk-test") })
	if c.model.ID != "gpt-5.6" {
		t.Fatalf("a chosen model was replaced: %v", c.model)
	}
}

func TestCheckLoginCutsTheKeyOutOfErrors(t *testing.T) {
	configTest(t)
	key := "sk-proj-" + strings.Repeat("A1b2C3d4", 12)
	server := modelsServer(t, 401, `{"error":{"message":"`+strings.Repeat("x", 150)+` Incorrect API key provided: `+key+`","type":"invalid_request_error"}}`)
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	loadAll(t)
	c := &config{model: provider.Model{Provider: provider.OpenAI, ID: "gpt-5.6"}, providers: map[string]provider.Provider{}}
	applyLogin(c, provider.OpenAI, key)
	out := captureStdout(t, func() { checkLogin(context.Background(), c, provider.OpenAI, key) })
	if !strings.Contains(out, "did not accept the key") || strings.Contains(out, key[:16]) {
		t.Fatalf("got %q", out)
	}
}

func TestLoginDropsClientsOfEntriesSharingTheKey(t *testing.T) {
	dir := configTest(t)
	writeConfigFile(t, dir, "config.json", `{"providers": {"corp": {"api": "openai", "base_url": "http://localhost:1/v1", "key_env": "OPENAI_API_KEY", "models": ["m"]}}}`)
	loadAll(t)
	c := &config{providers: map[string]provider.Provider{provider.OpenAI: nil, "corp": nil, provider.DeepSeek: nil}}
	applyLogin(c, provider.OpenAI, "sk-login")
	if _, cached := c.providers["corp"]; cached {
		t.Fatal("an entry sharing OPENAI_API_KEY kept the old client")
	}
	if _, value, _ := provider.Credential("corp"); value != "sk-login" {
		t.Fatalf("corp got %q", value)
	}
}
