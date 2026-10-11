package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/joho/godotenv"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/workspace"
)

// goatConfigDir is goat's config directory: $XDG_CONFIG_HOME/goat, or ~/.config/goat.
// It is "" when neither can be found.
func goatConfigDir() string {
	// The XDG spec says a relative XDG_CONFIG_HOME is invalid and should be ignored.
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "goat")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "goat")
}

// mentionsConfigDir reports a run_bash command that names goat's config directory, which
// holds the global .env: by config/goat, XDG_CONFIG_HOME or its full path, or by a
// relative path from a workdir above it, or by running inside it. Such commands are
// always confirmed, even when commands run without asking. It is a tripwire, not a
// boundary: it matches text, so quoting, globs, variables or a cd earlier in the
// command get past it.
func mentionsConfigDir(command, workdir string) bool {
	lower := strings.ToLower(command)
	for strings.Contains(lower, "//") || strings.Contains(lower, "/./") {
		lower = strings.ReplaceAll(strings.ReplaceAll(lower, "//", "/"), "/./", "/")
	}
	needles := []string{"config/goat", `config\goat`, "xdg_config_home"}
	if dir := goatConfigDir(); dir != "" {
		needles = append(needles, strings.ToLower(dir))
		if abs, err := filepath.Abs(workdir); err == nil {
			if within(abs, dir) {
				return true
			}
			if within(dir, abs) {
				rel, _ := filepath.Rel(abs, dir)
				needles = append(needles, strings.ToLower(filepath.ToSlash(rel)), strings.ToLower(rel))
			}
		}
	}
	for _, needle := range needles {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// globalEnvPath is the .env file goat reads in every workspace, or "" when there is no
// config directory.
func globalEnvPath() string {
	if dir := goatConfigDir(); dir != "" {
		return filepath.Join(dir, ".env")
	}
	return ""
}

// envSources records the file each variable goat set at startup came from, for /status.
// Variables that were already in the environment are not in it.
var envSources = map[string]string{}

// fixedByEnvironment are variables the global .env cannot set: goat finds the file
// through them before it reads it.
var fixedByEnvironment = map[string]bool{"HOME": true, "XDG_CONFIG_HOME": true}

// loadGlobalEnv fills the environment from the .env in goat's config directory, so the
// same keys work in every folder. A variable the environment already gives a non-empty
// value keeps it, so the shell wins. A provider's credentials are settled together: if
// the shell supplies, say, ANTHROPIC_AUTH_TOKEN, the file's ANTHROPIC_API_KEY is ignored,
// since either one is a complete credential. Variables goat takes from the file are kept
// out of run_bash commands.
//
// goat does not read the .env in the workspace. That file belongs to the project (its
// own keys, database URLs and the like), and a cloned repository could use it to point a
// provider's base URL at a server that would then receive the user's key.
//
// It returns warnings to show once the terminal is set up, and fails on a file that
// cannot be parsed, without quoting it, since it holds keys.
func loadGlobalEnv() (warnings []string, err error) {
	path := globalEnvPath()
	if path == "" {
		return nil, nil
	}
	values, err := readEnvFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var parseErr *envParseError
	if err != nil && !errors.As(err, &parseErr) {
		// A file goat cannot open should not stop it in every folder.
		return []string{fmt.Sprintf("skipping %s: %v", displayPath(path), err)}, nil
	}
	if err != nil {
		return nil, err
	}
	if warning := sharedFileWarning(path); warning != "" {
		warnings = append(warnings, warning)
	}

	// A credential the shell gives, directly or as a key command, settles the provider.
	fromShell := map[string]bool{}
	for _, name := range provider.Names {
		fromShell[name] = provider.CredentialSource(name) != ""
		for _, variable := range provider.CredentialVars[name] {
			if strings.TrimSpace(os.Getenv(variable+"_COMMAND")) != "" {
				fromShell[name] = true
			}
		}
	}
	// A key command belongs to its provider like the key: a shell credential keeps the
	// file's commands out too. A variable can belong to several providers when an
	// entry's key_env names a built-in key; the shell settling any of them keeps it out.
	credentialOf := map[string][]string{}
	for name, vars := range provider.CredentialVars {
		for _, variable := range vars {
			credentialOf[variable] = append(credentialOf[variable], name)
			credentialOf[variable+"_COMMAND"] = append(credentialOf[variable+"_COMMAND"], name)
		}
	}

	var taken []string
	for _, name := range slices.Sorted(maps.Keys(values)) {
		// On Windows anthropic_auth_token is ANTHROPIC_AUTH_TOKEN, so checks go by EnvName.
		canonical := workspace.EnvName(name)
		if fixedByEnvironment[canonical] || strings.TrimSpace(os.Getenv(name)) != "" || strings.TrimSpace(values[name]) == "" {
			continue
		}
		if slices.ContainsFunc(credentialOf[canonical], func(owner string) bool { return fromShell[owner] }) {
			continue
		}
		if err := os.Setenv(name, values[name]); err != nil {
			return warnings, fmt.Errorf("setting %s from %s: %w", name, path, err)
		}
		envSources[canonical] = displayPath(path)
		taken = append(taken, name)
	}
	workspace.HideFromCommands(taken...)
	return warnings, nil
}

// envParseError is a .env file the parser rejects. Its message names the file and,
// where it can be found, the line, but never quotes the file: the parser's own
// messages repeat its contents, keys included.
type envParseError struct {
	path string
	line int
}

func (e *envParseError) Error() string {
	if e.line > 0 {
		return fmt.Sprintf("%s is not a valid .env file: check line %d, which should look like NAME=value", e.path, e.line)
	}
	return fmt.Sprintf("%s is not a valid .env file: look for an unclosed quote or a line that is not NAME=value", e.path)
}

// readEnvFile parses a .env file. An I/O error is returned as it is, with the path; a
// file the parser rejects is an *envParseError.
func readEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Editors on Windows may start the file with a byte order mark.
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	values, err := parseEnv(data)
	if err != nil {
		return nil, &envParseError{path: path, line: badLine(data)}
	}
	return values, nil
}

// parseEnv parses .env text. Text without any NAME, such as a key pasted on its own
// last line, comes back from the parser as a value for the empty name; that is an
// error too.
func parseEnv(data []byte) (map[string]string, error) {
	values, err := godotenv.UnmarshalBytes(data)
	if err != nil {
		return nil, err
	}
	if _, ok := values[""]; ok {
		return nil, errors.New("a line has no name")
	}
	return values, nil
}

// badLine finds the line a parse error starts on: the line after the longest run of
// whole lines that parses. Lines after it all fail too, so a quoted value that spans
// lines does not count as the error. It returns 0 when every prefix parses.
func badLine(data []byte) int {
	lines := strings.SplitAfter(string(data), "\n")
	for n := len(lines); n >= 0; n-- {
		prefix := strings.Join(lines[:n], "")
		if !strings.HasSuffix(prefix, "\n") && prefix != "" {
			prefix += "\n"
		}
		if _, err := parseEnv([]byte(prefix)); err == nil {
			if n == len(lines) {
				return 0
			}
			return n + 1
		}
	}
	return 0
}

// sharedFileWarning warns about a key file other users can read. File modes mean
// nothing on Windows, so it never warns there.
func sharedFileWarning(path string) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o077 == 0 {
		return ""
	}
	return fmt.Sprintf("%s is open to other users, who may read your keys; run chmod 600 %s", displayPath(path), shellQuote(path))
}

// shellQuote quotes path for pasting into a shell when it needs it.
func shellQuote(path string) string {
	if path != "" && strings.IndexFunc(path, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+,@%", r))
	}) < 0 {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// displayPath shortens a path under the home directory to ~/...
func displayPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return filepath.Join("~", rest)
	}
	return path
}

// credentialOrigin says where a provider's credential came from, e.g.
// "OPENAI_API_KEY from ~/.config/goat/.env", or "" when it has none.
func credentialOrigin(name string) string {
	source := provider.CredentialSource(name)
	if source == "" {
		return ""
	}
	if provider.KeyCommands[source] != "" && envSources[source] != "" {
		return "key from " + envSources[source] + " in " + displayPath(providerEntriesPath())
	}
	return variableOrigin(source)
}

// variableOrigin says where a variable came from: a file goat loaded, or the environment.
func variableOrigin(name string) string {
	if file := envSources[name]; file != "" {
		return name + " from " + file
	}
	return name + " from the environment"
}

// endpointNotices are the startup lines about providers whose base URL moves their
// requests: the base URLs goat refuses, and every host that is not the provider's own
// API, so a key never goes to a proxy without the user seeing where. An entry from
// providers.json is only mentioned when its key would go unencrypted.
func endpointNotices() []string {
	var notices []string
	for _, name := range provider.Names {
		if provider.CredentialSource(name) == "" {
			continue
		}
		endpoint, err := provider.ResolveEndpoint(name)
		if err != nil {
			notices = append(notices, fmt.Sprintf("%s is unavailable: %v", name, err))
			continue
		}
		if _, isEntry := provider.LookupEntry(name); isEntry {
			// The user wrote the entry's URL into their own config, so only plain http
			// to another machine is worth a word at every start.
			if endpoint.Unencrypted {
				notices = append(notices, fmt.Sprintf("%s sends its key to %s%s", name, endpoint.Origin(), unencryptedNote(endpoint)))
			}
			continue
		}
		if endpoint.Custom {
			notices = append(notices, fmt.Sprintf("%s requests go to %s (%s)%s", name, endpoint.Origin(), variableOrigin(provider.BaseURLVars[name]), unencryptedNote(endpoint)))
		}
	}
	return notices
}

func unencryptedNote(endpoint provider.Endpoint) string {
	if endpoint.Unencrypted {
		return ", unencrypted: the key crosses the network in plain text"
	}
	return ""
}

// noAPIKeyMessage is the error for a start without a usable provider. Usually there are
// no credentials, but a key whose base URL goat refuses is named instead. It names the full
// path of the global file, and says the workspace's .env is not read, since keys left
// there from before are the likely mistake.
func noAPIKeyMessage() string {
	var refused []string
	for _, name := range provider.Names {
		if provider.CredentialSource(name) != "" {
			if err := provider.Unavailable(name); err != nil {
				refused = append(refused, err.Error())
			}
		}
	}
	if len(refused) > 0 {
		return "no provider can be used; " + strings.Join(refused, "; ")
	}
	// Entries from providers.json say what each one is missing.
	var entries []string
	for _, name := range provider.Names {
		if _, ok := provider.LookupEntry(name); ok {
			if err := provider.Unavailable(name); err != nil {
				entries = append(entries, err.Error())
			}
		}
	}
	if len(entries) > 0 {
		return "no provider can be used; " + strings.Join(entries, "; ")
	}
	message := "no API key found; set OPENAI_API_KEY, ANTHROPIC_API_KEY (or ANTHROPIC_AUTH_TOKEN) or DEEPSEEK_API_KEY in the environment"
	if global := globalEnvPath(); global != "" {
		message += " or in " + global
	}
	if info, err := os.Stat(".env"); err == nil && !info.IsDir() {
		message += " (goat does not read the .env in this folder: it belongs to the project)"
	}
	return message
}
