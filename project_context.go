package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Project instructions are notes for the agent kept with the code: AGENTS.md (or
// GOAT.md, or CLAUDE.md) at the workspace root, plus the user's own
// ~/.config/goat/AGENTS.md. They go at the start of a conversation's first message
// rather than into the system prompt, so the cached prompt prefix stays the same for
// every project and editing them never invalidates it.

// projectInstructionFiles are tried in order; the first that exists is used.
var projectInstructionFiles = []string{"AGENTS.md", "GOAT.md", "CLAUDE.md"}

// maxInstructionBytes caps each file, so a huge one cannot crowd out the conversation.
const maxInstructionBytes = 32 * 1024

func userInstructionsPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "goat", "AGENTS.md")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "goat", "AGENTS.md")
}

type instructionFile struct {
	label, content string
}

// findProjectInstructions returns the instruction files that apply, the user's first.
func findProjectInstructions() []instructionFile {
	var found []instructionFile
	read := func(path, label string) bool {
		data, err := os.ReadFile(path)
		if err != nil || strings.TrimSpace(string(data)) == "" {
			return false
		}
		content := string(data)
		if len(content) > maxInstructionBytes {
			content = content[:maxInstructionBytes] + "\n[... truncated ...]"
		}
		found = append(found, instructionFile{label: label, content: strings.TrimSpace(content)})
		return true
	}
	if path := userInstructionsPath(); path != "" {
		read(path, "~/.config/goat/AGENTS.md")
	}
	for _, name := range projectInstructionFiles {
		if _, _, err := resolveToolPath(name); err != nil {
			continue
		}
		if read(name, name) {
			break
		}
	}
	return found
}

// projectInstructionsText is the block prepended to a conversation's first message,
// or "" when there are no instruction files.
func projectInstructionsText() string {
	files := findProjectInstructions()
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Instructions for this project, written by the user. Follow them unless the user says otherwise.\n")
	for _, file := range files {
		fmt.Fprintf(&b, "\n<instructions source=%q>\n%s\n</instructions>\n", file.label, file.content)
	}
	return b.String()
}

// projectInstructionSources names the instruction files in use, for /status and the
// welcome notice.
func projectInstructionSources() []string {
	var labels []string
	for _, file := range findProjectInstructions() {
		labels = append(labels, file.label)
	}
	return labels
}
