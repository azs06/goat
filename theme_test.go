package main

import (
	"os"
	"path/filepath"
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
