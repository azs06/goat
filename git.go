package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/azs06/goat/internal/style"
)

// gitState is what goat shows about the workspace's repository.
type gitState struct {
	branch  string
	changed []string // porcelain status lines, e.g. " M main.go"
}

// runGit runs a read-only git command in the workspace, without the user's credentials
// in its environment, and gives up after a few seconds.
func runGit(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(commandEnvironment(os.Environ()), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	return string(out), err
}

// gitStateCache keeps the last status briefly, since the footer, banner and /status may
// all ask for it within moments and git status is slow in large repositories.
var gitStateCache struct {
	sync.Mutex
	dir   string
	at    time.Time
	state gitState
	ok    bool
}

const gitStateTTL = 2 * time.Second

// readGitState returns the repository's branch and changed files, or false when the
// workspace is not in a git repository.
func readGitState() (gitState, bool) {
	dir, _ := os.Getwd()
	gitStateCache.Lock()
	defer gitStateCache.Unlock()
	if gitStateCache.dir == dir && time.Since(gitStateCache.at) < gitStateTTL {
		return gitStateCache.state, gitStateCache.ok
	}
	state, ok := readGitStateNow()
	gitStateCache.dir, gitStateCache.at, gitStateCache.state, gitStateCache.ok = dir, time.Now(), state, ok
	return state, ok
}

func readGitStateNow() (gitState, bool) {
	out, err := runGit("status", "--porcelain=v1", "--branch", "--untracked-files=normal")
	if err != nil {
		return gitState{}, false
	}
	var state gitState
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if header, ok := strings.CutPrefix(line, "## "); ok {
			branch, _, _ := strings.Cut(header, "...")
			branch = strings.TrimPrefix(branch, "No commits yet on ")
			if strings.HasPrefix(branch, "HEAD (no branch)") {
				branch = "detached HEAD"
			}
			state.branch = branch
			continue
		}
		if line != "" {
			state.changed = append(state.changed, line)
		}
	}
	return state, true
}

// footerText is the branch with a count of changed files, for the prompt footer.
func (s gitState) footerText() string {
	if len(s.changed) == 0 {
		return s.branch
	}
	return fmt.Sprintf("%s %s %d changed", s.branch, style.Glyphs().Dot, len(s.changed))
}

// maxGitContextFiles bounds the changed files listed for the model.
const maxGitContextFiles = 30

// gitContextText describes the repository for the start of a new conversation, or ""
// outside git.
func gitContextText() string {
	state, ok := readGitState()
	if !ok {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The workspace is a git repository on branch %s", state.branch)
	if len(state.changed) == 0 {
		b.WriteString(" with no uncommitted changes.\n")
		return b.String()
	}
	fmt.Fprintf(&b, " with %d uncommitted change(s) (git status --porcelain):\n", len(state.changed))
	for i, line := range state.changed {
		if i == maxGitContextFiles {
			fmt.Fprintf(&b, "... and %d more\n", len(state.changed)-maxGitContextFiles)
			break
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// commandDiff shows the uncommitted changes: a summary, then the diff, through the
// pager when it is long. /diff staged shows what is staged instead.
func commandDiff(ctx context.Context, c *config, args []string) error {
	state, ok := readGitState()
	if !ok {
		return errors.New("the workspace is not a git repository")
	}
	if len(state.changed) == 0 {
		fmt.Println(formatNotice("No uncommitted changes on " + state.branch + "."))
		return nil
	}
	diffArgs := []string{"--no-pager", "diff", "--no-color", "--no-ext-diff"}
	if len(args) > 0 && (args[0] == "staged" || args[0] == "--staged" || args[0] == "cached") {
		diffArgs = append(diffArgs, "--staged")
	} else {
		diffArgs = append(diffArgs, "HEAD")
	}
	diff, err := runGit(diffArgs...)
	if err != nil {
		// A repository without commits has no HEAD to compare with.
		diff, _ = runGit("--no-pager", "diff", "--no-color", "--no-ext-diff")
	}

	var b strings.Builder
	b.WriteString(style.Strong.Render(fmt.Sprintf("%s · %s", state.branch, plural(len(state.changed), "changed file"))) + "\n")
	for _, line := range state.changed {
		b.WriteString("  " + style.Muted.Render(escapeControlChars(line)) + "\n")
	}
	if strings.TrimSpace(diff) != "" {
		b.WriteString("\n")
		for _, line := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
			b.WriteString(renderGitDiffLine(escapeControlChars(line)) + "\n")
		}
	}
	return showLong(ctx, c, strings.TrimRight(b.String(), "\n"))
}

// renderGitDiffLine styles one line of git diff output with the diff roles.
func renderGitDiffLine(line string) string {
	switch {
	case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"), strings.HasPrefix(line, "diff --git"), strings.HasPrefix(line, "index "):
		return style.Strong.Render(line)
	case strings.HasPrefix(line, "@@"):
		return style.Subtle.Render(line)
	case strings.HasPrefix(line, "+"):
		return style.DiffAdded.Render(line)
	case strings.HasPrefix(line, "-"):
		return style.DiffRemoved.Render(line)
	default:
		return style.Muted.Render(line)
	}
}
