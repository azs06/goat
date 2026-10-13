package codemode

import (
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
)

func TestDeclarationsDescribeArraysOfObjects(t *testing.T) {
	spec := provider.ToolSpec{Name: "run_tui", Parameters: map[string]any{
		"type":     "object",
		"required": []string{"command"},
		"properties": map[string]any{
			"command": map[string]any{"type": "string"},
			"steps": map[string]any{"type": "array", "items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"keys":     map[string]any{"type": "string"},
					"wait_ms":  map[string]any{"type": "integer"},
					"snapshot": map[string]any{"type": "boolean"},
				},
			}},
			"modes": map[string]any{"type": "array", "items": map[string]any{"enum": []string{"a", "b"}}},
			"names": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}}
	got := Declarations([]provider.ToolSpec{spec}, map[string]Doc{"run_tui": {Returns: "The screen."}})
	want := "run_tui(input: { command: string; modes?: (\"a\" | \"b\")[]; names?: string[]; steps?: ({ keys?: string; snapshot?: boolean; wait_ms?: number })[] }): Promise<string>;"
	if !strings.Contains(got, want) {
		t.Fatalf("expected %s in:\n%s", want, got)
	}
}
