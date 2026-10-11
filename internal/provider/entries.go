package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
)

// Entry is a provider the user adds in providers.json: an Anthropic- or
// OpenAI-compatible API at its own base URL, with its own key. It never uses a built-in
// provider's key unless key_env names that key outright.
type Entry struct {
	Name string
	// API is the wire format the server speaks: APIAnthropic (the Messages API) or
	// APIOpenAI (Chat Completions).
	API     string
	BaseURL string
	// KeyEnv names the variable that holds the key. KeyCommand is a command that prints
	// it instead. An entry with neither sends no key, as a local server needs.
	KeyEnv     string
	KeyCommand string
	// Models are offered in the picker; with none, the picker offers what the server
	// lists.
	Models []string

	endpoint Endpoint
}

const (
	APIAnthropic = "anthropic"
	APIOpenAI    = "openai"
)

// Keyless reports an entry that sends no key.
func (e Entry) Keyless() bool {
	return e.KeyEnv == "" && e.KeyCommand == ""
}

// builtIns are the providers goat has without providers.json; their names are reserved.
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
		return nil, fmt.Errorf("there is no provider named %s; providers.json does not define it", name)
	}
	key := ""
	if source := CredentialSource(name); source != "" {
		key = strings.TrimSpace(os.Getenv(source))
	}
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

// ResetEntries removes every registered entry, for tests.
func ResetEntries() {
	for name := range entries {
		delete(CredentialVars, name)
	}
	entries = map[string]Entry{}
	KeyCommands = map[string]string{}
	Names = slices.Clone(builtIns)
}

// EntriesError is a providers.json the parser rejects. It names the line where it can,
// and never quotes the file.
type EntriesError struct {
	Line    int
	Message string
}

func (e *EntriesError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Message)
	}
	return e.Message
}

var (
	entryNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	envNamePattern   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	// safeText is text short and plain enough to be a name, never a key.
	safeText = regexp.MustCompile(`^[a-z][a-z_-]{0,19}$`)
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
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, syntaxError(data, err, "the file must be a JSON object that maps each provider's name to its settings")
	}

	var list []Entry
	seen := map[string]bool{}
	for decoder.More() {
		line := lineAt(data, decoder.InputOffset())
		token, err := decoder.Token()
		if err != nil {
			return nil, syntaxError(data, err, "")
		}
		name, _ := token.(string)
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, syntaxError(data, err, "")
		}
		if seen[name] {
			return nil, &EntriesError{Line: line, Message: quoteIfSafe(name, "this entry's name") + " is defined twice"}
		}
		seen[name] = true
		entry, err := parseEntry(name, raw)
		if err != nil {
			return nil, &EntriesError{Line: line, Message: err.Error()}
		}
		list = append(list, entry)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, syntaxError(data, err, "")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, &EntriesError{Line: lineAt(data, decoder.InputOffset()), Message: "unexpected text after the closing }"}
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
	fields, err := objectFields(raw)
	if err != nil {
		return Entry{}, fmt.Errorf("%s %v", name, err)
	}
	for _, field := range slices.Sorted(maps.Keys(fields)) {
		if slices.Contains(entryFields, field) {
			continue
		}
		lower := strings.ToLower(field)
		if strings.Contains(lower, "key") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") {
			return Entry{}, fmt.Errorf("%s has %s; providers.json does not hold keys: name the variable that does with key_env, or a command that prints the key with key_command", name, quoteIfSafe(field, "a field for a key"))
		}
		return Entry{}, fmt.Errorf("%s has %s; the fields are %s", name, quoteIfSafe(field, "an unknown field"), strings.Join(entryFields, ", "))
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

// quoteIfSafe quotes text from the file when it cannot be a pasted key, and otherwise
// describes it.
func quoteIfSafe(text, description string) string {
	if safeText.MatchString(text) {
		return fmt.Sprintf("%s %q", description, text)
	}
	return description
}

// objectFields reads a JSON object's fields, refusing one that appears twice: the
// decoder would quietly keep the last, which could be a different base_url.
func objectFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New(`must be an object such as {"api": "openai", "base_url": "..."}`)
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, errors.New("is not valid JSON")
		}
		field, _ := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("is not valid JSON")
		}
		if _, ok := fields[field]; ok {
			return nil, fmt.Errorf("has %s twice", quoteIfSafe(field, "a field"))
		}
		fields[field] = value
	}
	return fields, nil
}

// syntaxError describes invalid JSON by line only: the decoder's own message quotes the
// text it stopped at.
func syntaxError(data []byte, err error, fallback string) error {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		// The streaming decoder's offset leaves out what earlier tokens consumed, so the
		// whole file is checked again for the true position.
		offset := syntax.Offset
		var whole any
		if errors.As(json.Unmarshal(data, &whole), &syntax) {
			offset = syntax.Offset
		}
		return &EntriesError{Line: lineAt(data, offset-1), Message: "not valid JSON: look here or at the end of the line before for a missing comma, quote or brace"}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &EntriesError{Message: "the file ends too early: look for a missing closing brace or quote"}
	}
	if fallback == "" {
		fallback = "not valid JSON"
	}
	return &EntriesError{Message: fallback}
}

// lineAt is the line of the first character at or after offset that is not white space
// or a comma, which is where the decoder's next token starts.
func lineAt(data []byte, offset int64) int {
	offset = min(max(offset, 0), int64(len(data)))
	for offset < int64(len(data)) && strings.ContainsRune(" \t\r\n,:", rune(data[offset])) {
		offset++
	}
	return 1 + bytes.Count(data[:offset], []byte("\n"))
}
