package provider

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

const (
	OpenAI    = "openai"
	Anthropic = "anthropic"
	DeepSeek  = "deepseek"

	DefaultOpenAIModel    = "gpt-6-luna"
	DefaultAnthropicModel = "claude-opus-5-5"
	DefaultDeepSeekModel  = "deepseek-v4-pro"
)

var Names = []string{OpenAI, Anthropic, DeepSeek}

// Model is a model together with the provider that serves it.
type Model struct {
	Provider string
	ID       string
}

func (m Model) String() string {
	return m.Provider + ":" + m.ID
}

func (m Model) IsZero() bool {
	return m.Provider == "" && m.ID == ""
}

// CredentialVars are the environment variables that make a provider available.
// A provider's models are only offered when one of them is set.
var CredentialVars = map[string][]string{
	OpenAI:    {"OPENAI_API_KEY"},
	Anthropic: {"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"},
	DeepSeek:  {"DEEPSEEK_API_KEY"},
}

// CredentialSource returns the variable that holds the provider's credentials,
// or "" when none is set.
func CredentialSource(provider string) string {
	for _, name := range CredentialVars[provider] {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return name
		}
	}
	return ""
}

func Available(provider string) bool {
	return CredentialSource(provider) != ""
}

func AvailableNames() []string {
	var available []string
	for _, provider := range Names {
		if Available(provider) {
			available = append(available, provider)
		}
	}
	return available
}

func MissingCredentialsError(provider string) error {
	return fmt.Errorf("%s models need %s to be set", provider, strings.Join(CredentialVars[provider], " or "))
}

func isKnown(name string) bool {
	for _, provider := range Names {
		if provider == name {
			return true
		}
	}
	return false
}

// forModelID guesses the provider from a bare model ID.
func forModelID(id string) string {
	lower := strings.ToLower(id)
	if strings.HasPrefix(lower, "claude-") {
		return Anthropic
	}
	if strings.HasPrefix(lower, "deepseek-") {
		return DeepSeek
	}
	for _, prefix := range []string{"gpt-", "o1", "o3", "o4", "codex", "chatgpt-", "ft:"} {
		if strings.HasPrefix(lower, prefix) {
			return OpenAI
		}
	}
	return ""
}

// ResolveModel turns user input into a model. A provider prefix such as "openai:"
// chooses the provider explicitly; otherwise it is inferred from the ID. IDs that do not
// reveal their provider need the prefix, so a typo is never taken for a model.
func ResolveModel(raw string) (Model, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Model{}, fmt.Errorf("model name is empty")
	}

	if provider, id, ok := strings.Cut(raw, ":"); ok && isKnown(strings.ToLower(provider)) {
		id = strings.TrimSpace(id)
		if id == "" {
			return Model{}, fmt.Errorf("model name is empty")
		}
		return Model{Provider: strings.ToLower(provider), ID: id}, nil
	}

	if provider := forModelID(raw); provider != "" {
		return Model{Provider: provider, ID: raw}, nil
	}
	return Model{}, fmt.Errorf("cannot tell which provider serves %q; prefix it with the provider, e.g. openai:%s", raw, raw)
}

// curatedModels are offered by the /model picker before any live listing.
func curatedModels(provider string) []string {
	switch provider {
	case OpenAI:
		models := []string{DefaultOpenAIModel}
		if configured := strings.TrimSpace(os.Getenv("OPENAI_MODEL")); configured != "" && configured != DefaultOpenAIModel {
			models = append(models, configured)
		}
		return models
	case Anthropic:
		return []string{DefaultAnthropicModel, "claude-fable-5-1", "claude-sonnet-5-5", "claude-haiku-5-5"}
	case DeepSeek:
		return []string{DefaultDeepSeekModel, "deepseek-flash"}
	default:
		return nil
	}
}

// ModelChoices lists the models the picker offers: only providers with credentials,
// the current model first within its provider, then the given extra models.
func ModelChoices(current Model, extra map[string][]string) []Model {
	var choices []Model
	seen := map[Model]bool{}
	add := func(ref Model) {
		if !seen[ref] {
			seen[ref] = true
			choices = append(choices, ref)
		}
	}

	for _, provider := range AvailableNames() {
		if current.Provider == provider {
			add(current)
		}
		for _, id := range curatedModels(provider) {
			add(Model{Provider: provider, ID: id})
		}
		live := append([]string(nil), extra[provider]...)
		sort.Strings(live)
		for _, id := range live {
			add(Model{Provider: provider, ID: id})
		}
	}
	return choices
}

// isOpenAIChatModel filters OpenAI's model list down to models the agent can chat with.
func isOpenAIChatModel(id string) bool {
	lower := strings.ToLower(id)
	if forModelID(lower) != OpenAI || strings.HasPrefix(lower, "ft:") {
		return false
	}
	for _, excluded := range []string{"embedding", "tts", "whisper", "dall-e", "image", "audio", "realtime", "transcribe", "search", "moderation", "instruct"} {
		if strings.Contains(lower, excluded) {
			return false
		}
	}
	return true
}

// DefaultModel picks a starting model from a provider that has credentials.
func DefaultModel() Model {
	if Available(OpenAI) {
		return Model{Provider: OpenAI, ID: DefaultOpenAIModel}
	}
	if Available(Anthropic) {
		return Model{Provider: Anthropic, ID: DefaultAnthropicModel}
	}
	if Available(DeepSeek) {
		return Model{Provider: DeepSeek, ID: DefaultDeepSeekModel}
	}
	return Model{}
}
