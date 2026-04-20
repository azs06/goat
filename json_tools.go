package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func formatJSONContent(content string) (string, error) {
	var decoded any
	if err := json.Unmarshal([]byte(content), &decoded); err != nil {
		return "", err
	}

	formatted, err := json.MarshalIndent(decoded, "", "  ")
	if err != nil {
		return "", err
	}

	return string(append(formatted, '\n')), nil
}

func applyJSONEdit(content, jsonPath, operation, valueJSON string) (string, error) {
	jsonPath = strings.TrimSpace(jsonPath)
	if jsonPath == "" {
		return "", fmt.Errorf("json_path is required")
	}
	if !gjson.Valid(content) {
		return "", fmt.Errorf("file does not contain valid JSON")
	}

	switch operation {
	case "set":
		valueJSON = strings.TrimSpace(valueJSON)
		if valueJSON == "" {
			return "", fmt.Errorf("value_json is required for set operations")
		}
		var decoded any
		if err := json.Unmarshal([]byte(valueJSON), &decoded); err != nil {
			return "", fmt.Errorf("value_json must be valid JSON: %w", err)
		}

		updatedContent, err := sjson.SetRaw(content, jsonPath, valueJSON)
		if err != nil {
			return "", err
		}
		return formatJSONContent(updatedContent)
	case "delete":
		if !gjson.Get(content, jsonPath).Exists() {
			return "", fmt.Errorf("json_path %q not found", jsonPath)
		}
		updatedContent, err := sjson.Delete(content, jsonPath)
		if err != nil {
			return "", err
		}
		formattedContent, err := formatJSONContent(updatedContent)
		if err != nil {
			return "", err
		}
		return formattedContent, nil
	default:
		return "", fmt.Errorf("unsupported operation %q; expected set or delete", operation)
	}
}

func editJSONFile(filePath, jsonPath, operation, valueJSON string) error {
	content, err := readFile(filePath)
	if err != nil {
		return err
	}

	updatedContent, err := applyJSONEdit(content, jsonPath, operation, valueJSON)
	if err != nil {
		return err
	}

	return withRecordedMutation("edit_json_file", []string{filePath}, func() error {
		return rawWriteFile(filePath, updatedContent)
	})
}

func previewEditJSONFile(filePath, jsonPath, operation, valueJSON string) (string, error) {
	content, err := readFile(filePath)
	if err != nil {
		return "", err
	}

	updatedContent, err := applyJSONEdit(content, jsonPath, operation, valueJSON)
	if err != nil {
		return "", err
	}
	if bytes.Equal([]byte(content), []byte(updatedContent)) {
		return fmt.Sprintf("Preview for edit_json_file on %s:\n(no changes)", filepath.ToSlash(filepath.Clean(filePath))), nil
	}

	cleanPath := filepath.ToSlash(filepath.Clean(filePath))
	preview := buildTextDiff(cleanPath, cleanPath, content, updatedContent)
	return fmt.Sprintf("Preview for edit_json_file on %s:\n%s", cleanPath, preview), nil
}
