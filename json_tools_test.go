package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyJSONEditDeleteRemovesPathAndFormatsOutput(t *testing.T) {
	updated, err := applyJSONEdit("{\"name\":\"goat\",\"enabled\":true}\n", "enabled", "delete", "")
	if err != nil {
		t.Fatalf("apply JSON delete: %v", err)
	}

	expected := "{\n  \"name\": \"goat\"\n}\n"
	if updated != expected {
		t.Fatalf("unexpected JSON output:\n%s", updated)
	}
}

func TestPreviewEditJSONFileDoesNotModifyFile(t *testing.T) {
	workspace := t.TempDir()
	chdirForTest(t, workspace)

	original := "{\n  \"name\": \"goat\"\n}\n"
	if err := os.WriteFile(filepath.Join(workspace, "sample.json"), []byte(original), 0644); err != nil {
		t.Fatalf("seed JSON file: %v", err)
	}

	preview, err := previewEditJSONFile("sample.json", "enabled", "set", "true")
	if err != nil {
		t.Fatalf("preview JSON edit: %v", err)
	}
	if !strings.Contains(preview, "Preview for edit_json_file") || !strings.Contains(preview, "+  \"enabled\": true") {
		t.Fatalf("expected JSON diff preview, got %q", preview)
	}

	content, err := os.ReadFile(filepath.Join(workspace, "sample.json"))
	if err != nil {
		t.Fatalf("read JSON file after preview: %v", err)
	}
	if string(content) != original {
		t.Fatalf("expected preview not to modify file, got %q", string(content))
	}
}
