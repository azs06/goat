package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/workspace"
)

// writeProviderEntries writes providers.json into the config directory runGoat gives
// the process.
func writeProviderEntries(t *testing.T, workDir, json string) {
	t.Helper()
	config := filepath.Join(workDir, "config", "goat")
	os.MkdirAll(config, 0o700)
	if err := os.WriteFile(filepath.Join(config, "providers.json"), []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeChatServer answers every chat request with reply and counts the requests that
// carry an Authorization header.
func fakeChatServer(t *testing.T, reply string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var withKey atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			withKey.Add(1)
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", reply)
	}))
	t.Cleanup(server.Close)
	return server, &withKey
}

func TestProcessUsesAKeylessEntry(t *testing.T) {
	server, withKey := fakeChatServer(t, "pong from local")
	workDir := t.TempDir()
	writeProviderEntries(t, workDir, `{"local": {"api": "openai", "base_url": "`+server.URL+`/v1", "models": ["qwen3-coder"]}}`)

	// No built-in key at all: the entry alone is enough to start.
	out, errOut, code := runGoat(t, workDir, "ping", []string{"OPENAI_API_KEY=sk-openai-SECRET"}, "-p", "-m", "local:qwen3-coder")
	if code != 0 || strings.TrimSpace(out) != "pong from local" {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if withKey.Load() != 0 {
		t.Fatal("a keyless entry must not send any key, least of all OpenAI's")
	}

	out, errOut, code = runGoat(t, workDir, "/status\n/exit\n", nil)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	origin := strings.TrimPrefix(server.URL, "http://")
	if !strings.Contains(out, "local:qwen3-coder") || !strings.Contains(out, "available (no key; requests go to http://"+origin+")") {
		t.Fatalf("expected the entry's model and row in /status:\n%s", out)
	}
	if strings.Contains(errOut, "local") {
		t.Errorf("a loopback entry needs no startup notice:\n%s", errOut)
	}
}

func TestProcessRefusesABrokenProvidersFile(t *testing.T) {
	workDir := t.TempDir()
	writeProviderEntries(t, workDir, "{\n  \"p\": {\"api\": \"openai\",\n    \"base_url\": sk-SECRET}\n}\n")
	_, errOut, code := runGoat(t, workDir, "", []string{"OPENAI_API_KEY=test"})
	want := filepath.Join("~", "config", "goat", "providers.json") + ": line 3: not valid JSON"
	if code == 0 || !strings.Contains(errOut, want) || strings.Contains(errOut, "SECRET") {
		t.Fatalf("expected %q, got %q (exit %d)", want, errOut, code)
	}
}

func TestProcessWarnsWhenAnEntrySendsItsKeyUnencrypted(t *testing.T) {
	workDir := t.TempDir()
	writeProviderEntries(t, workDir, `{"lan": {"api": "openai", "base_url": "http://10.0.0.7:8000/t/sk-in-path/v1", "key_env": "LAN_KEY"}}`)
	out, errOut, code := runGoat(t, workDir, "/status\n/exit\n", []string{"LAN_KEY=lan-key"})
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "lan sends its key to http://10.0.0.7:8000, unencrypted") {
		t.Errorf("expected an unencrypted notice:\n%s", errOut)
	}
	if !strings.Contains(out, "available (LAN_KEY from the environment; requests go to http://10.0.0.7:8000, unencrypted: ") {
		t.Errorf("expected the entry in /status:\n%s", out)
	}
	if strings.Contains(out+errOut, "sk-in-path") {
		t.Error("the entry's path must not be shown")
	}
}

// entryTest registers entries in this process for one test.
func entryTest(t *testing.T, json string) {
	t.Helper()
	list, err := provider.ParseEntries([]byte(json))
	if err != nil {
		t.Fatal(err)
	}
	provider.ResetEntries()
	provider.RegisterEntries(list)
	t.Cleanup(provider.ResetEntries)
}

func TestAnEntrysKeyCommandFillsItsKey(t *testing.T) {
	keyCommandsTest(t)
	entryTest(t, `{"router": {"api": "openai", "base_url": "https://router.example/v1", "key_command": "printf 'rk-from-command\\n'"}}`)
	t.Setenv("GOAT_PROVIDER_ROUTER_KEY", "")
	if warnings := runKeyCommands(); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	if os.Getenv("GOAT_PROVIDER_ROUTER_KEY") != "rk-from-command" || !provider.Available("router") {
		t.Fatal("the key command should make the entry available")
	}
	if origin := credentialOrigin("router"); !strings.Contains(origin, "key_command in ") {
		t.Fatalf("got origin %q", origin)
	}
}

func TestAnEntrysFailingKeyCommandIsNamed(t *testing.T) {
	keyCommandsTest(t)
	entryTest(t, `{"router": {"api": "openai", "base_url": "https://router.example/v1", "key_command": "echo sk-SECRET >&2; exit 3"}}`)
	t.Setenv("GOAT_PROVIDER_ROUTER_KEY", "")
	warnings := runKeyCommands()
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "router's key_command failed") || strings.Contains(warnings[0], "SECRET") {
		t.Fatalf("got %q", warnings)
	}
}

func TestEntryKeysAreHiddenFromCommands(t *testing.T) {
	entryTest(t, `{"proxy": {"api": "anthropic", "base_url": "https://p.example", "key_env": "WORK_LLM"},
		"router": {"api": "openai", "base_url": "https://router.example/v1", "key_command": "x"}}`)
	env := workspace.CommandEnvironment([]string{"WORK_LLM=secret", "WORK_LLM_COMMAND=op read x", "GOAT_PROVIDER_ROUTER_KEY=secret", "PATH=/bin"})
	if !slices.Equal(env, []string{"PATH=/bin"}) {
		t.Fatalf("got %q", env)
	}
}
