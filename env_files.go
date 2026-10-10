package main

import (
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
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
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

// loadEnvFiles fills the environment from the workspace's .env and then the global .env
// in goat's config directory. A variable takes the first non-empty value among the
// environment, the workspace file and the global file, so the shell wins and a project
// can override what is set for every project. Every variable either file defines is
// kept out of run_bash commands. It returns warnings to show once the terminal is set
// up, and fails on a file that exists but cannot be read or parsed.
func loadEnvFiles() (warnings []string, err error) {
	files := []string{".env"}
	if global := globalEnvPath(); global != "" {
		files = append(files, global)
	}
	for i, path := range files {
		values, err := godotenv.Read(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return warnings, fmt.Errorf("reading %s: %w", path, err)
		}
		if i > 0 {
			if warning := sharedFileWarning(path); warning != "" {
				warnings = append(warnings, warning)
			}
		}
		names := slices.Sorted(maps.Keys(values))
		workspace.HideFromCommands(names...)
		for _, name := range names {
			if strings.TrimSpace(os.Getenv(name)) != "" || strings.TrimSpace(values[name]) == "" {
				continue
			}
			if err := os.Setenv(name, values[name]); err != nil {
				return warnings, fmt.Errorf("setting %s from %s: %w", name, path, err)
			}
			envSources[name] = displayPath(path)
		}
	}
	return warnings, nil
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
	return fmt.Sprintf("%s can be read by other users; run chmod 600 %s", displayPath(path), path)
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
