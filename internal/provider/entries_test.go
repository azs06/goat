package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// entriesTest registers entries for one test and removes them after.
func entriesTest(t *testing.T, json string) []Entry {
	t.Helper()
	list, err := ParseEntries([]byte(json))
	if err != nil {
		t.Fatal(err)
	}
	ResetEntries()
	RegisterEntries(list)
	t.Cleanup(ResetEntries)
	return list
}

func TestParseEntries(t *testing.T) {
	list, err := ParseEntries([]byte(`{
  "ollama": {"api": "openai", "base_url": "http://localhost:11434/v1/", "models": ["qwen3-coder", "llama3.2:latest"]},
  "work-proxy": {"api": "anthropic", "base_url": "https://llm.corp.example", "key_env": "WORK_PROXY_KEY"},
  "router": {"api": "openai", "base_url": "https://router.example/api/v1", "key_command": "pass show router"}
}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Name != "ollama" || list[1].Name != "work-proxy" || list[2].Name != "router" {
		t.Fatalf("expected the entries in file order, got %+v", list)
	}
	ollama := list[0]
	if ollama.BaseURL != "http://localhost:11434/v1" || !ollama.Keyless() || !ollama.endpoint.Custom || ollama.endpoint.Unencrypted {
		t.Fatalf("got %+v", ollama)
	}
	if list[1].KeyEnv != "WORK_PROXY_KEY" || list[1].Keyless() || list[2].KeyCommand != "pass show router" {
		t.Fatalf("got %+v", list[1:])
	}
}

func TestParseEntriesRefusesMistakes(t *testing.T) {
	for _, test := range []struct{ json, want string }{
		{`[]`, "must be a JSON object"},
		{`{"openai": {"api": "openai", "base_url": "https://x.example"}}`, "built-in provider"},
		{`{"My_Proxy": {"api": "openai", "base_url": "https://x.example"}}`, "lowercase letters, digits and dashes"},
		{`{"p": {"api": "openai", "base_url": "https://x.example", "api_key": "sk-SECRET"}}`, "keys go in auth.json"},
		{`{"p": {"api": "openai", "base_url": "https://x.example", "url": "x"}}`, `unknown field "url"`},
		{`{"p": {"api": "gemini", "base_url": "https://x.example"}}`, `must be "openai" or "anthropic"`},
		{`{"p": {"base_url": "https://x.example"}}`, `needs "api"`},
		{`{"p": {"api": "openai"}}`, "needs a base_url"},
		{`{"p": {"api": "openai", "base_url": "https://me:SECRET@x.example"}}`, "p.base_url must not contain a user name or password"},
		{`{"p": {"api": "openai", "base_url": "ftp://SECRET.example"}}`, "p.base_url must start with https://"},
		{`{"p": {"api": "openai", "base_url": "https://x.example", "key_env": "K", "key_command": "c"}}`, "both key_env and key_command"},
		{`{"p": {"api": "openai", "base_url": "https://x.example", "key_env": "sk-SECRET"}}`, "must be the name of a variable"},
		{`{"p": {"api": "openai", "base_url": "https://x.example", "key_env": "gsk_SECRETabc123"}}`, "must be the name of a variable"},
		{`{"p": {"api": "openai", "base_url": "https://x.example", "key_env": "my_proxy_key"}}`, "in capitals"},
		{`{"sk-ant-api03-SECRET": {"api": "openai", "base_url": "https://x.example"}}`, "lowercase letters, digits and dashes"},
		{`{"p": {"api": "openai", "base_url": "https://x.example", "sk-proj-SECRET": ""}}`, "p has a field for a key; keys go in auth.json"},
		{`{"p": {"api": "openai", "base_url": "https://x.example", "sk_SECRETVALUE": ""}}`, "p has a field for a key"},
		{`{"p": {"api": "openai", "base_url": "https://x.example", "Xyzabc123": ""}}`, "p has an unknown field;"},
		{`{"p": {"api": "openai", "base_url": "https://x.example", "base_url": "http://SECRET.example"}}`, `p: a name "base_url" appears twice`},
		{`{"p": {"api": "openai", "base_url": "https://x.example", "models": "m"}}`, "list of model names"},
		{`{"p": {"api": "openai", "base_url": "https://x.example", "models": [" "]}}`, "empty name"},
		{`{"p": {"api": "openai", "base_url": "https://x.example"}, "p": {"api": "openai", "base_url": "https://y.example"}}`, `a name "p" appears twice`},
		{`{"p": "SECRET"}`, "must be an object"},
		{`{"p": {"api": "openai", "base_url": "https://x.example"}} SECRET`, "after the closing }"},
		{"{\n  \"p\": {\"api\": \"openai\",\n  \"base_url\": sk-SECRET}\n}", "line 3: not valid JSON"},
		{`{"p": {"api": "openai"`, "ends too early"},
	} {
		_, err := ParseEntries([]byte(test.json))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: expected an error containing %q, got %v", test.json, test.want, err)
		} else if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: the error repeats the file: %v", test.json, err)
		}
	}
}

func TestParseEntriesNamesTheEntrysLine(t *testing.T) {
	_, err := ParseEntries([]byte("{\n  \"good\": {\"api\": \"openai\", \"base_url\": \"https://x.example\"},\n\n  \"bad\": {\"api\": \"openai\"}\n}"))
	if err == nil || !strings.HasPrefix(err.Error(), "line 4: ") {
		t.Fatalf("expected the error on line 4, got %v", err)
	}
}

func TestEntriesAreProviders(t *testing.T) {
	t.Setenv("WORK_PROXY_KEY", "")
	entriesTest(t, `{
  "ollama": {"api": "openai", "base_url": "http://localhost:11434/v1", "models": ["qwen3-coder", "llama3.2:latest"]},
  "work-proxy": {"api": "anthropic", "base_url": "https://llm.corp.example", "key_env": "WORK_PROXY_KEY", "models": ["qwen3-coder"]},
  "router": {"api": "openai", "base_url": "https://router.example/v1", "key_command": "pass show router"}
}`)
	if err := Unavailable("ollama"); err != nil {
		t.Fatalf("a keyless entry should be available: %v", err)
	}
	if err := Unavailable("work-proxy"); err == nil || !strings.Contains(err.Error(), "WORK_PROXY_KEY") {
		t.Fatalf("expected the missing key variable, got %v", err)
	}
	if err := Unavailable("router"); err == nil || !strings.Contains(err.Error(), "key_command") {
		t.Fatalf("expected the key command to be named, got %v", err)
	}
	if KeyCommands[keyCommandVar("router")] != "pass show router" || CredentialVars["router"][0] != "GOAT_PROVIDER_ROUTER_KEY" {
		t.Fatalf("got %v, %v", KeyCommands, CredentialVars["router"])
	}
	if err := Unavailable("gone"); err == nil || !strings.Contains(err.Error(), "no provider named gone") {
		t.Fatalf("got %v", err)
	}
	t.Setenv("WORK_PROXY_KEY", "wp-key")
	if !Available("work-proxy") {
		t.Fatal("the entry should be available once its key is set")
	}

	for raw, want := range map[string]Model{
		"ollama:llama3.2:latest": {Provider: "ollama", ID: "llama3.2:latest"},
		"llama3.2:latest":        {Provider: "ollama", ID: "llama3.2:latest"},
		"router:some/model":      {Provider: "router", ID: "some/model"},
		"claude-opus-5-5":        {Provider: Anthropic, ID: "claude-opus-5-5"},
	} {
		if got, err := ResolveModel(raw); err != nil || got != want {
			t.Errorf("%s: got %v, %v", raw, got, err)
		}
	}
	if _, err := ResolveModel("qwen3-coder"); err == nil || !strings.Contains(err.Error(), "ollama and work-proxy") {
		t.Errorf("expected an ambiguous model to need a prefix, got %v", err)
	}
	if _, err := ResolveModel("gone:model"); err == nil || !strings.Contains(err.Error(), "no provider named gone") {
		t.Errorf("got %v", err)
	}

	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("DEEPSEEK_API_KEY", "")
	if got := DefaultModel(); got != (Model{Provider: "ollama", ID: "qwen3-coder"}) {
		t.Fatalf("expected the first entry's first model, got %v", got)
	}
	choices := ModelChoices(Model{}, map[string][]string{"ollama": {"other-model"}, "router": {"a", "b"}})
	var got []string
	for _, choice := range choices {
		got = append(got, choice.String())
	}
	if strings.Join(got, " ") != "ollama:qwen3-coder ollama:llama3.2:latest work-proxy:qwen3-coder" {
		t.Fatalf("expected each entry's listed models, got %v", got)
	}
}

func TestEntriesHideTheirURLPaths(t *testing.T) {
	entriesTest(t, `{"p": {"api": "openai", "base_url": "https://gw.example/t/SECRET-TOKEN/v1"}}`)
	got := HideEndpointPaths(`Post "https://gw.example/t/SECRET-TOKEN/v1/chat/completions": 500`)
	if strings.Contains(got, "SECRET") || !strings.Contains(got, "https://gw.example/…") {
		t.Fatalf("got %q", got)
	}
}

func TestEntriesDescribeEffort(t *testing.T) {
	entriesTest(t, `{"local": {"api": "openai", "base_url": "http://localhost:1/v1"}, "proxy": {"api": "anthropic", "base_url": "https://p.example"}}`)
	if got := DescribeEffort(nil, Model{Provider: "local", ID: "m"}, EffortHigh); !strings.HasPrefix(got, "nothing") {
		t.Errorf("got %q", got)
	}
	if got := DescribeEffort(nil, Model{Provider: "proxy", ID: "glm-5"}, EffortHigh); !strings.HasPrefix(got, "nothing") {
		t.Errorf("got %q", got)
	}
	if got := DescribeEffort(nil, Model{Provider: "proxy", ID: "claude-opus-5-5"}, EffortHigh); got != "high" {
		t.Errorf("got %q", got)
	}
	if !SpeaksAnthropic("proxy") || SpeaksAnthropic("local") || !SpeaksAnthropic(DeepSeek) || SpeaksAnthropic(OpenAI) {
		t.Error("SpeaksAnthropic")
	}
}

func TestParseEntriesFindsTheLineOfASyntaxErrorInALongFile(t *testing.T) {
	var file strings.Builder
	file.WriteString("{\n")
	for i := range 5 {
		fmt.Fprintf(&file, "  \"e%d\": {\n    \"api\": \"openai\",\n    \"base_url\": \"https://x.example\"\n  },\n\n", i)
	}
	// Line 28 is missing its comma, which the parser finds at the start of line 29.
	file.WriteString("  \"last\": {\n    \"api\": \"openai\"\n    \"base_url\": \"https://x.example\"\n  }\n}\n")
	_, err := ParseEntries([]byte(file.String()))
	if err == nil || !strings.HasPrefix(err.Error(), "line 29: ") {
		t.Fatalf("expected line 29, got %v\n%s", err, file.String())
	}
}

func TestEntryModelsWinOverBuiltInLookingNames(t *testing.T) {
	entriesTest(t, `{"ollama": {"api": "openai", "base_url": "http://localhost:11434/v1", "models": ["gpt-oss:20b", "deepseek-r1:8b"]}}`)
	for _, raw := range []string{"gpt-oss:20b", "deepseek-r1:8b"} {
		if got, err := ResolveModel(raw); err != nil || got.Provider != "ollama" {
			t.Errorf("%s: got %v, %v", raw, got, err)
		}
	}
	if got, _ := ResolveModel("gpt-6-luna"); got.Provider != OpenAI {
		t.Errorf("an unlisted model should still go by its name, got %v", got)
	}
	if _, err := ResolveModel("qwen3:8b"); err == nil || !strings.Contains(err.Error(), "e.g. ollama:qwen3:8b") {
		t.Errorf("expected a hint to prefix the model, got %v", err)
	}
}

func TestMissingEntryKeyNamesTheVariable(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	entriesTest(t, `{"openrouter": {"api": "openai", "base_url": "https://openrouter.ai/api/v1", "key_env": "OPENROUTER_API_KEY"}}`)
	if err := Unavailable("openrouter"); err == nil || err.Error() != "openrouter models need OPENROUTER_API_KEY, the variable its key_env names, to be set" {
		t.Fatalf("got %v", err)
	}
}

func TestConversationFitsTheEntrysCurrentAPI(t *testing.T) {
	entriesTest(t, `{"local": {"api": "anthropic", "base_url": "http://localhost:1"}}`)
	chat := Conversation{Provider: "local", Format: FormatChat, Messages: []json.RawMessage{json.RawMessage(`{}`)}}
	if chat.Fits("local") {
		t.Fatal("a Chat Completions transcript must not continue on an Anthropic-format entry")
	}
	messages := Conversation{Provider: "local", Messages: chat.Messages}
	if !messages.Fits("local") || messages.Fits("other") || !(Conversation{}).Fits("local") {
		t.Fatal("Fits")
	}
	if !(Conversation{Provider: Anthropic, Messages: chat.Messages}).Fits(Anthropic) {
		t.Fatal("built-in transcripts fit their provider")
	}
}
