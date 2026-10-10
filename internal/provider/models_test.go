package provider

import (
	"testing"
)

func setCredentials(t *testing.T, openAI, anthropic bool) {
	t.Helper()
	for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "DEEPSEEK_API_KEY", "GOAT_MODEL", "OPENAI_MODEL", "GOAT_EFFORT", "OPENAI_REASONING_EFFORT"} {
		t.Setenv(name, "")
	}
	if openAI {
		t.Setenv("OPENAI_API_KEY", "sk-test")
	}
	if anthropic {
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	}
}

func TestResolveModel(t *testing.T) {
	cases := []struct {
		raw     string
		want    Model
		wantErr bool
	}{
		{raw: "claude-opus-5-5", want: Model{Anthropic, "claude-opus-5-5"}},
		{raw: "gpt-6-luna", want: Model{OpenAI, "gpt-6-luna"}},
		{raw: "o3-mini", want: Model{OpenAI, "o3-mini"}},
		{raw: "ft:gpt-5:org::abc", want: Model{OpenAI, "ft:gpt-5:org::abc"}},
		{raw: "anthropic:claude-custom", want: Model{Anthropic, "claude-custom"}},
		{raw: "OpenAI:my-proxy-model", want: Model{OpenAI, "my-proxy-model"}},
		{raw: "deepseek-v4-pro", want: Model{DeepSeek, "deepseek-v4-pro"}},
		{raw: "anthropic:mystery-model", want: Model{Anthropic, "mystery-model"}},
		{raw: "mystery-model", wantErr: true},
		{raw: "cancel", wantErr: true},
		{raw: "anthropic:", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ResolveModel(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: expected an error, got %v", tc.raw, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q: got %v (%v), want %v", tc.raw, got, err, tc.want)
		}
	}
}

func TestModelChoicesOnlyListProvidersWithAPIKeys(t *testing.T) {
	current := Model{OpenAI, DefaultOpenAIModel}

	setCredentials(t, true, false)
	for _, choice := range ModelChoices(current, map[string][]string{Anthropic: {"claude-x"}}) {
		if choice.Provider != OpenAI {
			t.Fatalf("expected only OpenAI models without an Anthropic key, got %v", choice)
		}
	}

	setCredentials(t, false, true)
	choices := ModelChoices(current, nil)
	if len(choices) == 0 {
		t.Fatal("expected Anthropic models")
	}
	for _, choice := range choices {
		if choice.Provider != Anthropic {
			t.Fatalf("expected only Anthropic models without an OpenAI key, got %v", choice)
		}
	}

	setCredentials(t, false, false)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "token")
	if choices := ModelChoices(current, nil); len(choices) == 0 || choices[0].Provider != Anthropic {
		t.Fatalf("expected ANTHROPIC_AUTH_TOKEN to count as an Anthropic key, got %v", choices)
	}

	setCredentials(t, true, true)
	choices = ModelChoices(current, map[string][]string{OpenAI: {"gpt-other"}})
	if choices[0] != current {
		t.Fatalf("expected the current model first, got %v", choices[0])
	}
	seen := map[string]bool{}
	for _, choice := range choices {
		seen[choice.Provider] = true
	}
	if !seen[OpenAI] || !seen[Anthropic] {
		t.Fatalf("expected both providers, got %v", choices)
	}
}

func TestOpenAIModelListFilter(t *testing.T) {
	for id, want := range map[string]bool{"gpt-6-luna": true, "o3-mini": true, "text-embedding-3-large": false, "gpt-4o-realtime-preview": false, "dall-e-3": false, "whisper-1": false, "gpt-image-1": false, "babbage-002": false} {
		if got := isOpenAIChatModel(id); got != want {
			t.Errorf("%s: got %v, want %v", id, got, want)
		}
	}
}
