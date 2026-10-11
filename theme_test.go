package main

import (
	"context"
	"os"
	"path/filepath"
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

func TestThemeCommandSwitchesAndSaves(t *testing.T) {
	t.Cleanup(func() { style.Use(style.Default()); delete(envSources, "GOAT_THEME") })
	t.Setenv("GOAT_THEME", "")
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
	t.Cleanup(func() { style.Use(style.Default()) })
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
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GOAT_THEME", "one-dark")
	options, values := themeOptions()
	if len(options) != 2 || values[1] != "one-dark" || !options[1].current || options[0].current {
		t.Fatalf("got %+v %v", options, values)
	}

	// A theme file of one's own is offered too, and is in use without GOAT_THEME.
	t.Setenv("GOAT_THEME", "")
	path, _ := themePath()
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(`{"styles": {}}`), 0o600)
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
