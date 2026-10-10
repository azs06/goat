package provider

import (
	"strings"
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

func TestPickerModels(t *testing.T) {
	cases := []struct {
		provider string
		ids      []string
		want     string
	}{
		{OpenAI, []string{"gpt-6-luna", "gpt-6", "gpt-6.1-mini", "gpt-5.6", "gpt-5.6-codex", "gpt-5.5", "gpt-5.60", "gpt-60", "gpt-4o", "o3"}, "gpt-6-luna,gpt-6,gpt-6.1-mini,gpt-5.6,gpt-5.6-codex"},
		{Anthropic, []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-1", "claude-opus-5-5-20261001", "claude-fable-5", "claude-opus-5", "claude-opus-4-8", "claude-sonnet-5"}, "claude-opus-5-5,claude-sonnet-5-5,claude-haiku-5-5,claude-fable-5-1,claude-opus-5-5-20261001"},
		{DeepSeek, []string{"deepseek-v4-pro", "deepseek-flash"}, "deepseek-v4-pro,deepseek-flash"},
		{DeepSeek, []string{"deepseek-v4-pro", "deepseek-flash", "deepseek-v4.1-pro", "deepseek-v4-flash", "deepseek-v3-pro"}, "deepseek-v4.1-pro,deepseek-v4-flash"},
	}
	for _, c := range cases {
		if got := strings.Join(pickerModels(c.provider, c.ids), ","); got != c.want {
			t.Errorf("%s %v:\n got %s\nwant %s", c.provider, c.ids, got, c.want)
		}
	}
}

func TestModelChoicesKeepTheCurrentModel(t *testing.T) {
	setCredentials(t, true, false)
	current := Model{OpenAI, "gpt-4.1"}
	choices := ModelChoices(current, map[string][]string{OpenAI: {"gpt-5.5", "gpt-6.1"}})
	var ids []string
	for _, choice := range choices {
		ids = append(ids, choice.ID)
	}
	if got := strings.Join(ids, ","); got != "gpt-4.1,gpt-6-luna,gpt-6.1" {
		t.Fatalf("expected the current model plus the 5.6/6 series, got %s", got)
	}
}
