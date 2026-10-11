package provider

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
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

// Names are the providers: the built-in ones, then the entries from providers.json.
var Names = []string{OpenAI, Anthropic, DeepSeek}

// Model is a model together with the provider that serves it.
type Model struct {
	Provider string
	ID       string
}

func (m Model) String() string {
	if m.IsZero() {
		// An entry that lists no models gives goat none to start with.
		return "none (pick one with /model)"
	}
	return m.Provider + ":" + m.ID
}

func (m Model) IsZero() bool {
	return m.Provider == "" && m.ID == ""
}

// CredentialVars are the environment variables that make a provider available.
// A provider's models are only offered when one of them is set. Entries that send no key
// have none.
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

// Unavailable says why a provider cannot be used: it has no credentials, or its base URL
// is invalid, or there is no such provider. It is nil for a usable provider.
func Unavailable(provider string) error {
	if entry, ok := entries[provider]; ok {
		if !entry.Keyless() && CredentialSource(provider) == "" {
			return MissingCredentialsError(provider)
		}
		return nil
	}
	if !isBuiltIn(provider) {
		return fmt.Errorf("there is no provider named %s; providers.json does not define it", provider)
	}
	if CredentialSource(provider) == "" {
		return MissingCredentialsError(provider)
	}
	if _, err := ResolveEndpoint(provider); err != nil {
		return err
	}
	return nil
}

func Available(provider string) bool {
	return Unavailable(provider) == nil
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
	if entry, ok := entries[provider]; ok && entry.KeyCommand != "" {
		return fmt.Errorf("%s models need a key, and its key_command in providers.json gave none", provider)
	}
	return fmt.Errorf("%s models need %s to be set", provider, strings.Join(CredentialVars[provider], " or "))
}

func isKnown(name string) bool {
	return slices.Contains(Names, name)
}

// entryNamePrefix matches what can only be meant as a provider prefix: a name in the
// form entries use, followed by a colon.
var entryNamePrefix = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*:`)

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
	// A model that exactly one entry lists needs no prefix.
	var listedBy []string
	for _, name := range Names {
		if entry, ok := entries[name]; ok && slices.Contains(entry.Models, raw) {
			listedBy = append(listedBy, name)
		}
	}
	if len(listedBy) == 1 {
		return Model{Provider: listedBy[0], ID: raw}, nil
	}
	if len(listedBy) > 1 {
		return Model{}, fmt.Errorf("%s is listed by %s; prefix it with the one you mean, e.g. %s:%s", raw, strings.Join(listedBy, " and "), listedBy[0], raw)
	}
	if prefix := entryNamePrefix.FindString(raw); prefix != "" {
		return Model{}, fmt.Errorf("there is no provider named %s; providers.json does not define it", strings.TrimSuffix(prefix, ":"))
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
		return entries[provider].Models
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
		// The current model stays listed even when the picker would not offer it.
		if current.Provider == provider {
			add(current)
		}
		live := append([]string(nil), extra[provider]...)
		sort.Strings(live)
		for _, id := range pickerModels(provider, append(curatedModels(provider), live...)) {
			add(Model{Provider: provider, ID: id})
		}
	}
	return choices
}

// pickerModels narrows a provider's models to the ones the picker offers: OpenAI's
// GPT-5.6 and GPT-6 series, Claude's 5.5 models and Claude Fable 5.1, and DeepSeek's
// newest pro and flash models. Any model can still be chosen by name.
func pickerModels(provider string, ids []string) []string {
	switch provider {
	case OpenAI:
		return filterModels(ids, func(id string) bool {
			return hasModelPrefix(id, "gpt-5.6") || hasModelPrefix(id, "gpt-6")
		})
	case Anthropic:
		return filterModels(ids, func(id string) bool {
			for _, family := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-1"} {
				if hasModelPrefix(id, family) {
					return true
				}
			}
			return false
		})
	case DeepSeek:
		return newestDeepSeekModels(ids)
	default:
		// An entry that lists models offers just those; one without offers whatever its
		// server lists.
		if listed := entries[provider].Models; len(listed) > 0 {
			return filterModels(ids, func(id string) bool {
				return slices.ContainsFunc(listed, func(model string) bool { return strings.EqualFold(model, id) })
			})
		}
		return ids
	}
}

func filterModels(ids []string, keep func(id string) bool) []string {
	var kept []string
	for _, id := range ids {
		if keep(strings.ToLower(id)) {
			kept = append(kept, id)
		}
	}
	return kept
}

// hasModelPrefix matches prefix itself and its variants ("gpt-6-luna", "gpt-6.1",
// "claude-opus-5-5-20261001"), but not a different version that starts the same way
// ("gpt-60", "gpt-5.60").
func hasModelPrefix(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) {
		return false
	}
	rest := id[len(prefix):]
	return rest == "" || rest[0] == '-' || rest[0] == '.' && !strings.Contains(prefix, ".")
}

var deepSeekVersionPattern = regexp.MustCompile(`-v(\d+(?:\.\d+)*)`)

// newestDeepSeekModels keeps the highest-versioned pro model and flash model, in that
// order. An ID without a version, such as deepseek-flash, counts as the oldest.
func newestDeepSeekModels(ids []string) []string {
	var newest []string
	for _, tier := range []string{"pro", "flash"} {
		best, bestVersion := "", []int(nil)
		for _, id := range ids {
			lower := strings.ToLower(id)
			if !strings.Contains(lower, "-"+tier) {
				continue
			}
			version := deepSeekVersion(lower)
			if best == "" || compareVersions(version, bestVersion) > 0 {
				best, bestVersion = id, version
			}
		}
		if best != "" {
			newest = append(newest, best)
		}
	}
	return newest
}

func deepSeekVersion(id string) []int {
	match := deepSeekVersionPattern.FindStringSubmatch(id)
	if match == nil {
		return nil
	}
	var version []int
	for _, part := range strings.Split(match[1], ".") {
		number, _ := strconv.Atoi(part)
		version = append(version, number)
	}
	return version
}

func compareVersions(a, b []int) int {
	for i := 0; i < max(len(a), len(b)); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x - y
		}
	}
	return 0
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

// DefaultModel picks a starting model from a provider that has credentials, the
// built-in ones first, then the first model of the first entry that lists any.
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
	for _, name := range Names {
		if entry, ok := entries[name]; ok && len(entry.Models) > 0 && Available(name) {
			return Model{Provider: name, ID: entry.Models[0]}
		}
	}
	return Model{}
}
