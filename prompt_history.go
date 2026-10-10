package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/azs06/goat/internal/workspace"
)

// The prompts typed at the interactive prompt are kept in .goat/prompts.jsonl, one JSON
// string per line, so Up can bring them back in later sessions too.

const (
	promptHistoryPath = workspace.StateDirName + "/prompts.jsonl"
	// maxPromptHistory is how many past prompts Up can reach.
	maxPromptHistory = 1000
)

// loadPromptHistory returns the saved prompts, oldest first. A missing or unreadable
// file is an empty history.
func loadPromptHistory() []string {
	resolved, _, err := workspace.ResolveStatePath(promptHistoryPath)
	if err != nil {
		return nil
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil
	}
	defer file.Close()

	var prompts []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		var prompt string
		if json.Unmarshal(scanner.Bytes(), &prompt) == nil && prompt != "" {
			prompts = append(prompts, prompt)
		}
	}
	if len(prompts) > maxPromptHistory {
		prompts = prompts[len(prompts)-maxPromptHistory:]
	}
	return prompts
}

// rememberPrompt adds prompt to history and the file, unless it repeats the last one.
func rememberPrompt(history []string, prompt string) []string {
	if strings.TrimSpace(prompt) == "" || (len(history) > 0 && history[len(history)-1] == prompt) {
		return history
	}
	history = append(history, prompt)
	if len(history) > maxPromptHistory {
		history = history[len(history)-maxPromptHistory:]
	}

	resolved, _, err := workspace.ResolveStatePath(promptHistoryPath)
	if err != nil {
		return history
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return history
	}
	encoded, err := json.Marshal(prompt)
	if err != nil {
		return history
	}
	file, err := os.OpenFile(resolved, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return history
	}
	defer file.Close()
	_, _ = file.Write(append(encoded, '\n'))
	return history
}

// maxPathCompletions bounds the menu of path matches.
const maxPathCompletions = 50

// pathCompletions completes the word that ends at the cursor as a workspace path. It
// returns the matching entries, directories with a trailing slash, and the start of
// the word they replace. goat's state directory is never offered.
func pathCompletions(text []rune, pos int) (matches []string, wordStart int) {
	wordStart = pos
	for wordStart > 0 && !strings.ContainsRune(" \t\n\"'`(", text[wordStart-1]) {
		wordStart--
	}
	word := string(text[wordStart:pos])
	// "@path" mentions complete the same way.
	word = strings.TrimPrefix(word, "@")
	if strings.HasPrefix(word, "/") || strings.HasPrefix(word, "~") || strings.Contains(word, "..") {
		return nil, wordStart
	}

	dir, base := filepath.Split(word)
	listDir := dir
	if listDir == "" {
		listDir = "."
	}
	if _, _, err := workspace.ResolvePath(listDir); err != nil && listDir != "." {
		return nil, wordStart
	}
	entries, err := os.ReadDir(listDir)
	if err != nil {
		return nil, wordStart
	}
	showHidden := strings.HasPrefix(base, ".")
	for _, entry := range entries {
		name := entry.Name()
		if workspace.IsStateDirName(name) || !strings.HasPrefix(strings.ToLower(name), strings.ToLower(base)) {
			continue
		}
		if strings.HasPrefix(name, ".") && !showHidden {
			continue
		}
		candidate := dir + name
		if entry.IsDir() {
			candidate += "/"
		}
		matches = append(matches, candidate)
	}
	sort.Strings(matches)
	if len(matches) > maxPathCompletions {
		matches = matches[:maxPathCompletions]
	}
	if strings.HasPrefix(string(text[wordStart:pos]), "@") {
		for i := range matches {
			matches[i] = "@" + matches[i]
		}
	}
	return matches, wordStart
}

// commonPrefix is the longest prefix every match shares, compared case-sensitively.
func commonPrefix(matches []string) string {
	if len(matches) == 0 {
		return ""
	}
	prefix := matches[0]
	for _, match := range matches[1:] {
		for !strings.HasPrefix(match, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	return prefix
}
