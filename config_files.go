package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/azs06/goat/internal/jsonfile"
	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/style"
)

// goat reads three files from its config directory, never from the workspace:
//
//   - config.json holds settings and providers; it never holds keys, so it is safe to
//     share or keep in a dotfiles repository.
//   - auth.json holds keys, or commands that print them, and is readable only by the user.
//   - .env is the older way to set both, still read below the JSON files.
//
// Settings are looked up in the shell, then config.json, then .env. A provider's key is
// settled as a group in the same order: if the shell sets any of its credential
// variables or their _COMMAND, auth.json and .env are not used for it; otherwise if
// auth.json names it, .env is not used for it.

// configPath and authPath are the JSON files in goat's config directory, or "".
func configPath() string { return configFile("config.json") }
func authPath() string   { return configFile("auth.json") }

func configFile(name string) string {
	if dir := goatConfigDir(); dir != "" {
		return filepath.Join(dir, name)
	}
	return ""
}

// configSetting is a config.json setting that stands in for an environment variable,
// below the shell.
type configSetting struct {
	field, variable string
	// parse checks the JSON value and turns it into the variable's text.
	parse func(raw json.RawMessage) (string, error)
}

var configSettings = []configSetting{
	{"theme", "GOAT_THEME", parseThemeSetting},
	{"auto_approve_bash", "GOAT_AUTO_APPROVE_BASH", parseBoolSetting("true", "false")},
	{"code_mode", "GOAT_CODEMODE", parseBoolSetting("true", "false")},
	{"reduced_motion", "GOAT_REDUCED_MOTION", parseBoolSetting("true", "false")},
	{"anthropic_fallbacks", "GOAT_ANTHROPIC_FALLBACKS", parseBoolSetting("on", "off")},
	{"anthropic_cache_ttl", "GOAT_ANTHROPIC_CACHE_TTL", parseChoiceSetting("5m", "1h")},
	{"auto_compact_tokens", "GOAT_AUTO_COMPACT_TOKENS", parseCountSetting},
	{"verify_timeout", "GOAT_VERIFY_TIMEOUT", parseCountSetting},
}

// configStartup holds config.json's model and effort. They are defaults for a start
// without a saved session, not overrides like GOAT_MODEL, so /model choices still last.
var configStartup struct {
	model  string
	effort string
}

// configFields lists config.json's fields, for errors.
func configFields() string {
	names := []string{"model", "effort", "base_urls", "providers", "verify"}
	for _, setting := range configSettings {
		names = append(names, setting.field)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// loadConfigFile reads config.json: its settings go into the environment where the shell
// has not set them, and its providers are returned to be registered. A file goat cannot
// open is skipped with a warning; one it cannot parse stops goat, naming the file and
// line without quoting it.
func loadConfigFile() (entries []provider.Entry, warnings []string, err error) {
	path := configPath()
	if path == "" {
		return nil, nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, []string{fmt.Sprintf("skipping %s: %v", displayPath(path), err)}, nil
	}
	entries, err = applyConfig(data, displayPath(path))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", displayPath(path), err)
	}
	return entries, nil, nil
}

// applyConfig parses config.json and applies it. source names the file for /status.
func applyConfig(data []byte, source string) ([]provider.Entry, error) {
	fields, err := jsonfile.Object(data, "config.json must be a JSON object of settings, such as {\"theme\": \"one-dark\"}")
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	var entries []provider.Entry
	for _, field := range fields {
		fail := func(message string) error { return &jsonfile.Error{Line: field.Line, Message: message} }
		switch field.Name {
		case "model":
			var model string
			if json.Unmarshal(field.Value, &model) != nil || strings.TrimSpace(model) == "" {
				return nil, fail("model must be a model name such as \"anthropic:claude-opus-5-5\"")
			}
			configStartup.model = strings.TrimSpace(model)
		case "effort":
			var effort string
			if json.Unmarshal(field.Value, &effort) != nil {
				return nil, fail("effort must be a string")
			}
			if _, err := provider.ParseEffort(effort); err != nil {
				return nil, fail("effort must be one of " + provider.JoinEfforts(provider.EffortLevels))
			}
			configStartup.effort = strings.TrimSpace(effort)
		case "base_urls":
			urls, err := jsonfile.Object(field.Value, `base_urls must be an object such as {"anthropic": "https://proxy.example"}`)
			if err != nil {
				return nil, jsonfile.Shift(err, field.ValueLine)
			}
			for _, url := range urls {
				// Lines inside base_urls count from its own first line.
				line := url.Line + field.ValueLine - 1
				variable := provider.BaseURLVars[url.Name]
				if variable == "" {
					return nil, &jsonfile.Error{Line: line, Message: fmt.Sprintf("base_urls has %s; it takes openai, anthropic and deepseek (other providers go in providers)", jsonfile.Quote(url.Name, "a provider"))}
				}
				var value string
				if json.Unmarshal(url.Value, &value) != nil {
					return nil, &jsonfile.Error{Line: line, Message: "base_urls." + url.Name + " must be a URL string"}
				}
				if err := provider.CheckBaseURL("base_urls."+url.Name, value); err != nil {
					return nil, &jsonfile.Error{Line: line, Message: err.Error()}
				}
				values[variable] = strings.TrimSpace(value)
			}
		case "verify":
			parsed, err := parseVerifySetting(field.Value, field.ValueLine)
			if err != nil {
				return nil, err
			}
			setVerifyCommands(parsed)
		case "providers":
			parsed, err := provider.ParseEntries(field.Value)
			if err != nil {
				return nil, jsonfile.Shift(err, field.ValueLine)
			}
			for i := range parsed {
				parsed[i].Source = source
			}
			entries = parsed
		default:
			index := slices.IndexFunc(configSettings, func(s configSetting) bool { return s.field == field.Name })
			if index < 0 {
				if jsonfile.LooksLikeKeyField(field.Name) {
					return nil, fail(fmt.Sprintf("config.json has %s; keys go in auth.json, which only you can read", jsonfile.Quote(field.Name, "a field for a key")))
				}
				if strings.HasPrefix(field.Name, "$") {
					continue
				}
				return nil, fail(fmt.Sprintf("config.json has %s; the fields are %s", jsonfile.Quote(field.Name, "an unknown field"), configFields()))
			}
			setting := configSettings[index]
			value, err := setting.parse(field.Value)
			if err != nil {
				return nil, fail(setting.field + " " + err.Error())
			}
			values[setting.variable] = value
		}
	}
	// The shell wins; what config.json sets keeps the global .env's value out.
	for _, variable := range slices.Sorted(maps.Keys(values)) {
		if strings.TrimSpace(os.Getenv(variable)) != "" {
			continue
		}
		if err := os.Setenv(variable, values[variable]); err != nil {
			return nil, err
		}
		envSources[variable] = source
	}
	return entries, nil
}

func parseBoolSetting(on, off string) func(json.RawMessage) (string, error) {
	return func(raw json.RawMessage) (string, error) {
		var value *bool
		if json.Unmarshal(raw, &value) != nil || value == nil {
			return "", errors.New("must be true or false")
		}
		if *value {
			return on, nil
		}
		return off, nil
	}
}

func parseChoiceSetting(choices ...string) func(json.RawMessage) (string, error) {
	return func(raw json.RawMessage) (string, error) {
		var value string
		if json.Unmarshal(raw, &value) != nil || !slices.Contains(choices, value) {
			return "", fmt.Errorf("must be %q", strings.Join(choices, `" or "`))
		}
		return value, nil
	}
}

func parseCountSetting(raw json.RawMessage) (string, error) {
	var value int64
	if json.Unmarshal(raw, &value) != nil || value <= 0 {
		return "", errors.New("must be a whole number above 0")
	}
	return strconv.FormatInt(value, 10), nil
}

// parseThemeSetting takes a built-in theme's name or the path of a theme file, where ~
// stands for the home directory.
func parseThemeSetting(raw json.RawMessage) (string, error) {
	var value string
	if json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) == "" {
		return "", errors.New("must be a built-in theme's name or a theme file's path")
	}
	value = strings.TrimSpace(value)
	if _, ok := style.Builtin(value); ok {
		return value, nil
	}
	if rest, ok := strings.CutPrefix(value, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			value = filepath.Join(home, rest)
		}
	}
	if !filepath.IsAbs(value) {
		hint := ""
		if suggestion, ok := style.SuggestBuiltin(value); ok {
			hint = "; did you mean " + suggestion + "?"
		}
		return "", fmt.Errorf("must be a built-in theme (%s) or the full path of a theme file%s", strings.Join(style.BuiltinNames(), ", "), hint)
	}
	return value, nil
}

// authCommands are auth.json's key_command values, by provider.
var authCommands = map[string]string{}

// authSettled are the providers auth.json supplies, so the global .env is not used for
// them.
var authSettled = map[string]bool{}

// authOrigins say where each stored key came from, for /status.
var authOrigins = map[string]string{}

// authFields are what a provider's entry in auth.json may hold: exactly one of them.
var authFields = []string{"key", "auth_token", "key_command"}

// loadAuthFile reads auth.json: each provider's key goes into goat's memory, never the
// environment, unless the shell already supplies that provider. A key_command is run by
// runKeyCommands. Errors name the file and line and never quote it.
func loadAuthFile() (warnings []string, err error) {
	path := authPath()
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return []string{fmt.Sprintf("skipping %s: %v", displayPath(path), err)}, nil
	}
	if warning := sharedFileWarning(path); warning != "" {
		warnings = append(warnings, warning)
	}
	more, err := applyAuth(data, displayPath(path))
	if err != nil {
		return warnings, fmt.Errorf("%s: %w", displayPath(path), err)
	}
	return append(warnings, more...), nil
}

// applyAuth parses auth.json and stores its keys. source names the file for /status.
func applyAuth(data []byte, source string) (warnings []string, err error) {
	fields, err := jsonfile.Object(data, `auth.json must be a JSON object such as {"openai": {"key": "sk-..."}}`)
	if err != nil {
		return nil, err
	}
	for _, field := range fields {
		fail := func(message string) error { return &jsonfile.Error{Line: field.Line, Message: message} }
		name := field.Name
		if strings.HasPrefix(name, "$") {
			continue
		}
		if !slices.Contains(provider.Names, name) {
			// An entry may have been removed from config.json; its key does no harm.
			warnings = append(warnings, fmt.Sprintf("%s line %d: %s is not a provider; it is ignored", source, field.Line, jsonfile.Quote(name, "the name")))
			continue
		}
		credential := map[string]string{}
		var key string
		if json.Unmarshal(field.Value, &key) == nil {
			// "openai": "sk-..." is short for "openai": {"key": "sk-..."}.
			credential["key"] = key
		} else {
			members, err := jsonfile.Object(field.Value, name+` must be an object such as {"key": "..."} or {"key_command": "..."}`)
			if err != nil {
				var fileErr *jsonfile.Error
				if errors.As(err, &fileErr) {
					return nil, fail(fileErr.Message)
				}
				return nil, fail(err.Error())
			}
			for _, member := range members {
				if !slices.Contains(authFields, member.Name) {
					return nil, fail(fmt.Sprintf("%s has %s; it takes one of %s", name, jsonfile.Quote(member.Name, "an unknown field"), strings.Join(authFields, ", ")))
				}
				var value string
				if json.Unmarshal(member.Value, &value) != nil {
					return nil, fail(name + "." + member.Name + " must be a string")
				}
				credential[member.Name] = value
			}
		}
		if len(credential) != 1 {
			return nil, fail(name + " needs exactly one of " + strings.Join(authFields, ", "))
		}
		for kind, value := range credential {
			value = strings.TrimSpace(value)
			if value == "" {
				return nil, fail(name + "." + kind + " is empty")
			}
			if kind == "auth_token" && name != provider.Anthropic {
				return nil, fail("auth_token is only for anthropic; use key")
			}
			if shellSettles(name) {
				continue
			}
			authSettled[name] = true
			provider.NameInAuth(name)
			switch kind {
			case "key_command":
				authCommands[name] = value
			default:
				provider.StoreKey(name, value, kind == "auth_token")
				authOrigins[name] = source
			}
		}
	}
	return warnings, nil
}

// shellSettles reports a provider whose credential, or a command for it, the shell sets.
func shellSettles(name string) bool {
	for _, variable := range provider.CredentialVars[name] {
		if strings.TrimSpace(os.Getenv(variable)) != "" || strings.TrimSpace(os.Getenv(variable+"_COMMAND")) != "" {
			return true
		}
	}
	return false
}
