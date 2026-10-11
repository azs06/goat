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
}

func TestThemeCommandSwitchesForTheSession(t *testing.T) {
	t.Cleanup(func() { style.Use(style.Default()) })
	t.Setenv("GOAT_THEME", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := commandTheme(context.Background(), &config{}, []string{"one-dark"}); err != nil {
		t.Fatal(err)
	}
	oneDark, _ := style.Builtin("one-dark")
	if style.Accent.Style() != oneDark.Style(style.Accent) {
		t.Fatal("expected the one-dark theme")
	}
	if err := commandTheme(context.Background(), &config{}, []string{"nope"}); err == nil || !strings.Contains(err.Error(), "default, one-dark") {
		t.Fatalf("got %v", err)
	}
	// With no argument the configured theme comes back.
	if err := commandTheme(context.Background(), &config{}, nil); err != nil || style.Accent.Style() != style.Default().Style(style.Accent) {
		t.Fatalf("got %v", err)
	}
}

func TestProcessStaysPlainWithAColorTheme(t *testing.T) {
	out, _, code := runGoat(t, t.TempDir(), "/theme one-dark\n/status\n/exit\n", []string{"OPENAI_API_KEY=test", "GOAT_THEME=one-dark"})
	if code != 0 || strings.Contains(out, "\x1b") {
		t.Fatalf("piped output must stay plain (exit %d):\n%q", code, out)
	}
}
