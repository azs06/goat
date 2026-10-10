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

// fixedByEnvironment are variables a .env file cannot set: goat finds the global file
// through them before it reads either file.
var fixedByEnvironment = map[string]bool{"HOME": true, "XDG_CONFIG_HOME": true}

// loadEnvFiles fills the environment from the workspace's .env and then the global .env
// in goat's config directory. A variable takes the first non-empty value among the
// environment, the workspace file and the global file, so the shell wins and a project
// can override what is set for every project. A provider's credentials are settled
// together: once one source supplies, say, ANTHROPIC_AUTH_TOKEN, a later file's
// ANTHROPIC_API_KEY is ignored, since either one is a complete credential. Variables
// goat takes from a file are kept out of run_bash commands. It returns warnings to show
// once the terminal is set up, and fails on a file that exists but cannot be read or
// parsed, without quoting the file, which holds keys.
func loadEnvFiles() (warnings []string, err error) {
	files := []string{".env"}
	if global := globalEnvPath(); global != "" {
		files = append(files, global)
	}

	settled := map[string]bool{}
	for _, name := range provider.Names {
		settled[name] = provider.CredentialSource(name) != ""
	}
	credentialOf := map[string]string{}
	for name, vars := range provider.CredentialVars {
		for _, variable := range vars {
			credentialOf[variable] = name
		}
	}

	for i, path := range files {
		values, err := readEnvFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		var parseErr *envParseError
		if err != nil && i > 0 && !errors.As(err, &parseErr) {
			// A global file goat cannot open should not stop it in every folder.
			warnings = append(warnings, fmt.Sprintf("skipping %s: %v", displayPath(path), err))
			continue
		}
		if err != nil {
			return warnings, err
		}
		if i > 0 {
			if warning := sharedFileWarning(path); warning != "" {
				warnings = append(warnings, warning)
			}
		}
		suppliedBy := map[string]bool{}
		var taken []string
		for _, name := range slices.Sorted(maps.Keys(values)) {
			if fixedByEnvironment[name] || strings.TrimSpace(os.Getenv(name)) != "" || strings.TrimSpace(values[name]) == "" {
				continue
			}
			if owner := credentialOf[name]; owner != "" {
				if settled[owner] {
					continue
				}
				suppliedBy[owner] = true
			}
			if err := os.Setenv(name, values[name]); err != nil {
				return warnings, fmt.Errorf("setting %s from %s: %w", name, path, err)
			}
			envSources[name] = displayPath(path)
			taken = append(taken, name)
		}
		for owner := range suppliedBy {
			settled[owner] = true
		}
		workspace.HideFromCommands(taken...)
	}
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
	if file := envSources[source]; file != "" {
		return source + " from " + file
	}
	return source + " from the environment"
}

// noAPIKeyMessage is the error for a start without any credentials. It names the full
// path of the global file, since that is the place that works from every folder.
func noAPIKeyMessage() string {
	message := "no API key found; set OPENAI_API_KEY, ANTHROPIC_API_KEY (or ANTHROPIC_AUTH_TOKEN) or DEEPSEEK_API_KEY in the environment or in a .env file in this folder"
	if global := globalEnvPath(); global != "" {
		message += ", or put it in " + global + " to use it from every folder"
	}
	return message
}
