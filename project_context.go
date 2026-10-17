package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/azs06/goat/internal/skills"
	"github.com/azs06/goat/internal/workspace"
)

func init() {
	skills.UserConfigDir = goatConfigDir
}

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
	if dir := goatConfigDir(); dir != "" {
		return filepath.Join(dir, "AGENTS.md")
	}
	return ""
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
		if _, _, err := workspace.ResolvePath(name); err != nil {
			continue
		}
		if read(name, name) {
			break
		}
	}
	return found
}

// projectInstructionsText is the block prepended to a conversation's first message:
// the instruction files and the state of the git repository. It is "" when there is
// neither.
func projectInstructionsText() string {
	var b strings.Builder
	if files := findProjectInstructions(); len(files) > 0 {
		b.WriteString("Instructions for this project, written by the user. Follow them unless the user says otherwise.\n")
		for _, file := range files {
			fmt.Fprintf(&b, "\n<instructions source=%q>\n%s\n</instructions>\n", file.label, file.content)
		}
	}
	if list := skillsListText(); list != "" {
		b.WriteString(list)
	}
	if git := gitContextText(); git != "" {
		fmt.Fprintf(&b, "\n<workspace>\n%s</workspace>\n", git)
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

// skillsListText lists the skills the model can load, for the start of a conversation.
// It goes in the first message rather than the system prompt for the same reason the
// instructions do: adding a skill must not change the cached prompt prefix.
func skillsListText() string {
	found, _ := skills.Discover(skills.Roots())
	var b strings.Builder
	for _, skill := range found {
		if skill.ModelInvocable {
			fmt.Fprintf(&b, "- %s: %s\n", skill.Name, skill.Description)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n<skills>\nSkills you can load with the skill tool when a task matches one:\n" + b.String() + "</skills>\n"
}
