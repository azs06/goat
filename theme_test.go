package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/style"
)

func TestApplyThemeLoadsGoatTheme(t *testing.T) {
	t.Cleanup(func() { style.Use(style.Default()) })
	path := filepath.Join(t.TempDir(), "theme.json")
	if err := os.WriteFile(path, []byte(`{"styles": {"accent": "color: red; font-weight: bold"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOAT_THEME", path)
	if got := applyTheme(); got != path {
		t.Fatalf("applyTheme() = %q, want %q", got, path)
	}
	if !style.Accent.Style().Bold || style.Muted.Style() != style.Default().Style(style.Muted) {
		t.Fatalf("theme not layered over the default: accent=%+v", style.Accent.Style())
	}

	// A broken file falls back to the built-in theme.
	if err := os.WriteFile(path, []byte(`{"styles":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := applyTheme(); got != "" || style.Accent.Style() != style.Default().Style(style.Accent) {
		t.Fatalf("broken theme: applyTheme() = %q, accent=%+v", got, style.Accent.Style())
	}
}

func TestThemePathUsesXDGConfigHome(t *testing.T) {
	t.Setenv("GOAT_THEME", "")
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	if path, explicit := themePath(); path != "/cfg/goat/theme.json" || explicit {
		t.Fatalf("themePath() = %q, %v", path, explicit)
	}
}

func TestGoatThemeNamesABuiltinTheme(t *testing.T) {
	t.Cleanup(func() { style.Use(style.Default()) })
	t.Setenv("GOAT_THEME", "one-dark")
	oneDark, _ := style.Builtin("one-dark")
	if got := applyTheme(); got != "the built-in one-dark theme" || style.Accent.Style() != oneDark.Style(style.Accent) {
		t.Fatalf("applyTheme() = %q", got)
	}
	t.Setenv("GOAT_THEME", "One Dark")
	if got := applyTheme(); got != "the built-in one-dark theme" {
		t.Fatalf("applyTheme() = %q", got)
	}
}

// isolateThemeSettings keeps a test's theme switches and config.json reads to itself.
func isolateThemeSettings(t *testing.T) {
	t.Helper()
	previous, previousInUse, previousFiles := envSources, themeInUse, themeFiles
	envSources, themeInUse, themeFiles = map[string]string{}, "", nil
	t.Cleanup(func() {
		envSources, themeInUse, themeFiles = previous, previousInUse, previousFiles
		style.Use(style.Default())
	})
	t.Setenv("GOAT_THEME", "")
	t.Setenv("GOAT_CODEMODE", "")
}

func TestThemeCommandSwitchesAndSaves(t *testing.T) {
	isolateThemeSettings(t)
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	configFile := filepath.Join(configDir, "goat", "config.json")
	os.MkdirAll(filepath.Dir(configFile), 0o700)
	os.WriteFile(configFile, []byte(`{"code_mode": true}`), 0o600)

	out := captureStdout(t, func() {
		if err := commandTheme(context.Background(), &config{}, []string{"one-dark"}); err != nil {
			t.Fatal(err)
		}
	})
	oneDark, _ := style.Builtin("one-dark")
	if style.Accent.Style() != oneDark.Style(style.Accent) || !strings.Contains(out, "Saved in") {
		t.Fatalf("expected the one-dark theme, saved:\n%s", out)
	}
	data, _ := os.ReadFile(configFile)
	if !strings.Contains(string(data), `"code_mode": true`) || !strings.Contains(string(data), `"theme": "one-dark"`) {
		t.Fatalf("expected config.json to keep its settings and gain the theme:\n%s", data)
	}
	if info, _ := os.Stat(configFile); info.Mode().Perm() != 0o600 {
		t.Fatalf("expected config.json to keep its permissions, got %v", info.Mode().Perm())
	}
	if _, err := applyConfig(data, "config.json"); err != nil {
		t.Fatalf("the saved config.json does not load: %v", err)
	}

	// A near miss is not applied, but named.
	style.Use(style.Default())
	err := commandTheme(context.Background(), &config{}, []string{"ondedark"})
	if err == nil || !strings.Contains(err.Error(), "did you mean one-dark?") || style.Accent.Style() != style.Default().Style(style.Accent) {
		t.Fatalf("got %v", err)
	}
	// Spelled without the hyphen, or as two words, it is one-dark.
	for _, args := range [][]string{{"onedark"}, {"One", "Dark"}} {
		style.Use(style.Default())
		captureStdout(t, func() {
			if err := commandTheme(context.Background(), &config{}, args); err != nil || style.Accent.Style() != oneDark.Style(style.Accent) {
				t.Fatalf("%v: got %v", args, err)
			}
		})
	}

	// Without a terminal to pick on, /theme reloads the configured theme: the saved one.
	style.Use(style.Default())
	out = captureStdout(t, func() {
		if err := commandTheme(context.Background(), &config{}, nil); err != nil || style.Accent.Style() != oneDark.Style(style.Accent) {
			t.Fatalf("got %v", err)
		}
	})
	if !strings.Contains(out, "Using the built-in one-dark theme") {
		t.Fatalf("got %q", out)
	}
}

func TestThemeFromTheShellIsNotSaved(t *testing.T) {
	isolateThemeSettings(t)
	t.Setenv("GOAT_THEME", "default")
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	out := captureStdout(t, func() {
		if err := commandTheme(context.Background(), &config{}, []string{"one-dark"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "set in your shell") || os.Getenv("GOAT_THEME") != "default" {
		t.Fatalf("got %q, GOAT_THEME=%q", out, os.Getenv("GOAT_THEME"))
	}
	if _, err := os.Stat(filepath.Join(configDir, "goat", "config.json")); err == nil {
		t.Fatal("expected nothing saved while the shell sets the theme")
	}
}

func TestThemeOptionsMarkTheThemeInUse(t *testing.T) {
	isolateThemeSettings(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GOAT_THEME", "one-dark")
	applyTheme()
	options, values := themeOptions()
	if len(options) != 2 || values[1] != "one-dark" || !options[1].current || options[0].current {
		t.Fatalf("got %+v %v", options, values)
	}

	// A theme file of one's own is offered too, and is in use without GOAT_THEME.
	t.Setenv("GOAT_THEME", "")
	path, _ := themePath()
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(`{"styles": {}}`), 0o600)
	applyTheme()
	options, values = themeOptions()
	if len(options) != 3 || values[2] != path || !options[2].current || options[0].current {
		t.Fatalf("got %+v %v", options, values)
	}
}

func TestProcessStaysPlainWithAColorTheme(t *testing.T) {
	out, _, code := runGoat(t, t.TempDir(), "/theme one-dark\n/status\n/exit\n", []string{"OPENAI_API_KEY=test", "GOAT_THEME=one-dark"})
	if code != 0 || strings.Contains(out, "\x1b") {
		t.Fatalf("piped output must stay plain (exit %d):\n%q", code, out)
	}
}

func TestThemeFileStaysOfferedAndBrokenOnesAreNotSaved(t *testing.T) {
	isolateThemeSettings(t)
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	file := filepath.Join(configDir, "goat", "theme.json")
	os.MkdirAll(filepath.Dir(file), 0o700)
	os.WriteFile(file, []byte(`{"styles": {"accent": "color: red"}}`), 0o600)

	captureStdout(t, func() {
		if err := commandTheme(context.Background(), &config{}, []string{"one-dark"}); err != nil {
			t.Fatal(err)
		}
	})
	// With a built-in theme chosen, the file is still there to go back to.
	if _, values := themeOptions(); len(values) != 3 || values[2] != file {
		t.Fatalf("expected the theme file offered, got %v", values)
	}
	captureStdout(t, func() {
		if err := switchTheme(file); err != nil {
			t.Fatal(err)
		}
	})
	if currentThemeSetting() != file {
		t.Fatalf("expected the file in use, got %q", currentThemeSetting())
	}

	// A file that does not load is not switched to or saved.
	os.WriteFile(file, []byte(`{"styles":`), 0o600)
	before, _ := os.ReadFile(filepath.Join(configDir, "goat", "config.json"))
	if err := switchTheme(file); err == nil {
		t.Fatal("expected a broken theme file to be refused")
	}
	after, _ := os.ReadFile(filepath.Join(configDir, "goat", "config.json"))
	if string(before) != string(after) {
		t.Fatalf("config.json changed:\n%s", after)
	}
}

func TestThemeSavedAfterAFailedSave(t *testing.T) {
	isolateThemeSettings(t)
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	configFile := filepath.Join(configDir, "goat", "config.json")
	os.MkdirAll(filepath.Dir(configFile), 0o700)
	os.WriteFile(configFile, []byte(`{"theme":`), 0o600)
	if err := switchTheme("one-dark"); err == nil || !strings.Contains(err.Error(), "saving it failed") {
		t.Fatalf("got %v", err)
	}
	// Once config.json is fixed, the next switch saves: goat's own GOAT_THEME is not
	// mistaken for the shell's.
	os.WriteFile(configFile, []byte(`{}`), 0o600)
	out := captureStdout(t, func() {
		if err := switchTheme("default"); err != nil {
			t.Fatal(err)
		}
	})
	if data, _ := os.ReadFile(configFile); !strings.Contains(out, "Saved in") || !strings.Contains(string(data), `"theme": "default"`) {
		t.Fatalf("got %q, %s", out, data)
	}
}

func TestThemePickerFollowsWhatIsOnScreen(t *testing.T) {
	isolateThemeSettings(t)
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)

	// A theme file config.json named at startup stays offered after a built-in theme
	// is chosen.
	mine := filepath.Join(t.TempDir(), "mine.json")
	os.WriteFile(mine, []byte(`{"styles": {"accent": "color: red"}}`), 0o600)
	t.Setenv("GOAT_THEME", mine)
	envSources["GOAT_THEME"] = "config.json"
	applyTheme()
	if currentThemeSetting() != mine {
		t.Fatalf("got %q", currentThemeSetting())
	}
	captureStdout(t, func() {
		if err := switchTheme("one-dark"); err != nil {
			t.Fatal(err)
		}
	})
	if _, values := themeOptions(); !slices.Contains(values, mine) || currentThemeSetting() != "one-dark" {
		t.Fatalf("got %v, current %q", values, currentThemeSetting())
	}

	// With GOAT_THEME from the shell, what /theme switched to is still what is in use.
	t.Setenv("GOAT_THEME", "default")
	delete(envSources, "GOAT_THEME")
	applyTheme()
	captureStdout(t, func() {
		if err := switchTheme("one-dark"); err != nil {
			t.Fatal(err)
		}
	})
	if options, _ := themeOptions(); !options[1].current || options[0].current {
		t.Fatalf("expected one-dark marked, got %+v", options)
	}

	// A theme file that failed to load is not marked as in use.
	t.Setenv("GOAT_THEME", "")
	broken := filepath.Join(configDir, "goat", "theme.json")
	os.MkdirAll(filepath.Dir(broken), 0o700)
	os.WriteFile(broken, []byte(`{"styles":`), 0o600)
	captureStdout(t, func() { applyTheme() })
	if currentThemeSetting() != "default" {
		t.Fatalf("expected the default theme in use, got %q", currentThemeSetting())
	}
}

func TestThemeFileUnderHomeIsSavedWithTilde(t *testing.T) {
	isolateThemeSettings(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	file := filepath.Join(home, "dotfiles", "goat-theme.json")
	os.MkdirAll(filepath.Dir(file), 0o700)
	os.WriteFile(file, []byte(`{"styles": {}}`), 0o600)
	captureStdout(t, func() {
		if err := switchTheme(file); err != nil {
			t.Fatal(err)
		}
	})
	data, _ := os.ReadFile(filepath.Join(home, ".config", "goat", "config.json"))
	if !strings.Contains(string(data), `"theme": "~/dotfiles/goat-theme.json"`) {
		t.Fatalf("got %s", data)
	}
}
