package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/azs06/goat/internal/jsonfile"
)

// Entry is a provider the user adds in config.json or providers.json: an Anthropic- or
// OpenAI-compatible API at its own base URL, with its own key. It never uses a built-in
// provider's key unless key_env names that key outright.
type Entry struct {
	Name string
	// API is the wire format the server speaks: APIAnthropic (the Messages API) or
	// APIOpenAI (Chat Completions).
	API     string
	BaseURL string
	// KeyEnv names the variable that holds the key. KeyCommand is a command that prints
	// it instead. An entry with neither sends the key auth.json has under its name, or
	// none, as a local server needs.
	KeyEnv     string
	KeyCommand string
	// Models are offered in the picker; with none, the picker offers what the server
	// lists.
	Models []string
	// Source is the file the entry came from, for messages, such as
	// "~/.config/goat/config.json".
	Source string

	endpoint Endpoint
}

const (
	APIAnthropic = "anthropic"
	APIOpenAI    = "openai"
)

// Keyless reports an entry that names no key variable or command. It still sends a key
// auth.json has for it.
func (e Entry) Keyless() bool {
	return e.KeyEnv == "" && e.KeyCommand == ""
}

// builtIns are the providers goat has without config.json; their names are reserved.
var builtIns = []string{OpenAI, Anthropic, DeepSeek}

// entries are the registered entries, by name.
var entries = map[string]Entry{}

// KeyCommands maps the variable that holds an entry's key to the key_command that fills
// it.
var KeyCommands = map[string]string{}

// LookupEntry returns the entry with this name.
func LookupEntry(name string) (Entry, bool) {
	entry, ok := entries[name]
	return entry, ok
}

func isBuiltIn(name string) bool {
	return slices.Contains(builtIns, name)
}

// keyCommandVar is the variable an entry's key_command fills, such as
// GOAT_PROVIDER_MY_PROXY_KEY for my-proxy.
func keyCommandVar(name string) string {
	return "GOAT_PROVIDER_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_KEY"
}

// RegisterEntries adds entries to the providers, after the built-in ones, in the order
// given. Each entry's key variable becomes a credential like the built-in ones, so
// run_bash commands never see it.
func RegisterEntries(list []Entry) {
	for _, entry := range list {
		entries[entry.Name] = entry
		Names = append(Names, entry.Name)
		switch {
		case entry.KeyEnv != "":
			CredentialVars[entry.Name] = []string{entry.KeyEnv}
		case entry.KeyCommand != "":
			variable := keyCommandVar(entry.Name)
			CredentialVars[entry.Name] = []string{variable}
			KeyCommands[variable] = entry.KeyCommand
		}
	}
}

// NewEntry creates the provider for an entry, with the key from its variable.
func NewEntry(name string) (Provider, error) {
	entry, ok := entries[name]
	if !ok {
		return nil, unknownProviderError(name)
	}
	_, key, _ := Credential(name)
	if entry.API == APIOpenAI {
		return newChatProvider(name, entry.BaseURL, key), nil
	}
	return newAnthropicEntry(name, entry.BaseURL, key), nil
}

// SpeaksAnthropic reports a provider reached through the Anthropic Messages API:
// Anthropic, DeepSeek and Anthropic-compatible entries.
func SpeaksAnthropic(name string) bool {
	if entry, ok := entries[name]; ok {
		return entry.API == APIAnthropic
	}
	return name == Anthropic || name == DeepSeek
}

// unknownProviderError is the error for a provider goat has never heard of.
func unknownProviderError(name string) error {
	return fmt.Errorf("there is no provider named %s; add it under providers in config.json", name)
}

// sourceName is the file an entry came from, by name alone.
func (e Entry) sourceName() string {
	if e.Source == "" {
		return "config.json"
	}
	return filepath.Base(e.Source)
}

// ResetEntries removes every registered entry, for tests.
func ResetEntries() {
	for name := range entries {
		delete(CredentialVars, name)
	}
	entries = map[string]Entry{}
	KeyCommands = map[string]string{}
	Names = slices.Clone(builtIns)
}

var (
	entryNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	envNamePattern   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

// entryFields are the fields an entry may have.
var entryFields = []string{"api", "base_url", "key_env", "key_command", "models"}

// ParseEntries reads providers.json: an object that maps each entry's name to its
// settings, e.g.
//
//	{"ollama": {"api": "openai", "base_url": "http://localhost:11434/v1", "models": ["qwen3-coder"]}}
//
// Errors never repeat the file's text, in case a key was pasted into it.
func ParseEntries(data []byte) ([]Entry, error) {
	fields, err := jsonfile.Object(data, "the providers must be a JSON object that maps each provider's name to its settings")
	if err != nil {
		return nil, err
	}
	var list []Entry
	for _, field := range fields {
		entry, err := parseEntry(field.Name, field.Value)
		if err != nil {
			return nil, &jsonfile.Error{Line: field.Line, Message: err.Error()}
		}
		list = append(list, entry)
	}
	return list, nil
}

func parseEntry(name string, raw json.RawMessage) (Entry, error) {
	if !entryNamePattern.MatchString(name) {
		return Entry{}, errors.New("an entry's name may only use lowercase letters, digits and dashes")
	}
	if isBuiltIn(name) {
		return Entry{}, fmt.Errorf("%s is a built-in provider; give the entry another name", name)
	}
	members, err := jsonfile.Object(raw, `must be an object such as {"api": "openai", "base_url": "..."}`)
	if err != nil {
		var fileErr *jsonfile.Error
		if errors.As(err, &fileErr) {
			return Entry{}, fmt.Errorf("%s: %s", name, fileErr.Message)
		}
		return Entry{}, fmt.Errorf("%s: %v", name, err)
	}
	fields := map[string]json.RawMessage{}
	for _, member := range members {
		field := member.Name
		fields[field] = member.Value
		if slices.Contains(entryFields, field) {
			continue
		}
		if jsonfile.LooksLikeKeyField(field) {
			return Entry{}, fmt.Errorf("%s has %s; keys go in auth.json, or name the variable that holds one with key_env, or a command that prints it with key_command", name, jsonfile.Quote(field, "a field for a key"))
		}
		return Entry{}, fmt.Errorf("%s has %s; the fields are %s", name, jsonfile.Quote(field, "an unknown field"), strings.Join(entryFields, ", "))
	}

	entry := Entry{Name: name}
	for field, target := range map[string]*string{"api": &entry.API, "base_url": &entry.BaseURL, "key_env": &entry.KeyEnv, "key_command": &entry.KeyCommand} {
		if value, ok := fields[field]; ok {
			if json.Unmarshal(value, target) != nil {
				return Entry{}, fmt.Errorf("%s.%s must be a string", name, field)
			}
			*target = strings.TrimSpace(*target)
		}
	}
	if value, ok := fields["models"]; ok {
		if json.Unmarshal(value, &entry.Models) != nil {
			return Entry{}, fmt.Errorf("%s.models must be a list of model names", name)
		}
		for i, model := range entry.Models {
			entry.Models[i] = strings.TrimSpace(model)
			if entry.Models[i] == "" {
				return Entry{}, fmt.Errorf("%s.models has an empty name", name)
			}
		}
	}

	switch entry.API {
	case APIAnthropic, APIOpenAI:
	case "":
		return Entry{}, fmt.Errorf("%s needs \"api\": \"openai\" (Chat Completions) or \"anthropic\" (Messages)", name)
	default:
		return Entry{}, fmt.Errorf("%s.api must be \"openai\" or \"anthropic\"", name)
	}
	if entry.BaseURL == "" {
		return Entry{}, fmt.Errorf("%s needs a base_url, such as http://localhost:11434/v1", name)
	}
	endpoint, err := checkBaseURL(name+".base_url", entry.BaseURL, "")
	if err != nil {
		return Entry{}, err
	}
	entry.endpoint = endpoint
	entry.BaseURL = endpoint.URL
	if entry.KeyEnv != "" && entry.KeyCommand != "" {
		return Entry{}, fmt.Errorf("%s has both key_env and key_command; use one", name)
	}
	if entry.KeyEnv != "" && !envNamePattern.MatchString(entry.KeyEnv) {
		// Upper case only: a key pasted in by mistake (gsk_..., hf_...) is not taken for
		// a name and then shown in "needs ... to be set" messages.
		return Entry{}, fmt.Errorf("%s.key_env must be the name of a variable, in capitals, such as MY_PROXY_KEY; the key itself goes in that variable", name)
	}
	return entry, nil
}
