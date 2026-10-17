// Package skills finds and loads skills: directories holding a SKILL.md file of
// instructions for one kind of task, with any scripts and references it uses. The format
// is the Agent Skills one (https://agentskills.io/specification), so skills written for
// other agents work in goat too.
//
// Skills are found again on every lookup rather than once at startup, so one the agent
// writes during a conversation can be loaded at once.
package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// FileName is the file that makes a directory a skill.
const FileName = "SKILL.md"

const (
	// maxSkillBytes caps a SKILL.md, as goat caps AGENTS.md, so one huge file cannot
	// crowd out the conversation.
	maxSkillBytes = 32 * 1024
	// maxFileBytes caps another file of a skill that is read.
	maxFileBytes = 256 * 1024
	// maxDepth is how far below a root a SKILL.md is looked for.
	maxDepth          = 4
	maxNameLength     = 64
	maxDescriptionLen = 1024
)

var namePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// UserConfigDir returns goat's config directory, whose skills directory holds the user's
// own skills. It is set by the main package.
var UserConfigDir func() string

// HomeDir returns the user's home directory, whose .agents/skills holds skills shared
// with other agents. Tests replace it.
var HomeDir = os.UserHomeDir

// Root is a directory skills are found in.
type Root struct {
	// Dir is the directory, relative to the workspace for a project root.
	Dir string
	// Label names it for people, such as "~/.config/goat/skills".
	Label string
}

// Roots are the directories skills are found in, in order: the user's own first, as
// with AGENTS.md, so a skill in a cloned repository cannot stand in for one of theirs.
func Roots() []Root {
	var roots []Root
	if UserConfigDir != nil {
		if dir := UserConfigDir(); dir != "" {
			roots = append(roots, Root{Dir: filepath.Join(dir, "skills"), Label: "~/.config/goat/skills"})
		}
	}
	if home, err := HomeDir(); err == nil && home != "" {
		roots = append(roots, Root{Dir: filepath.Join(home, ".agents", "skills"), Label: "~/.agents/skills"})
	}
	return append(roots,
		Root{Dir: filepath.Join(".agents", "skills"), Label: ".agents/skills"},
		Root{Dir: filepath.Join(".claude", "skills"), Label: ".claude/skills"},
	)
}

// Skill is one skill that was found.
type Skill struct {
	Name        string
	Description string
	// Dir is the skill's directory, absolute.
	Dir string
	// Source is the label of the root it was found in.
	Source string
	// ModelInvocable is false for a skill with disable-model-invocation: true, which
	// only the user loads, with /skill.
	ModelInvocable bool
}

// Discover finds the skills in the roots, sorted by name. A name found twice keeps the
// first; that and a SKILL.md that cannot be used are reported in problems.
func Discover(roots []Root) (found []Skill, problems []string) {
	seen := map[string]string{}
	for _, root := range roots {
		dir, err := filepath.Abs(root.Dir)
		if err != nil {
			continue
		}
		walk(dir, 0, func(skillDir string) {
			skill, err := read(skillDir)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", display(filepath.Join(skillDir, FileName)), err))
				return
			}
			if first, ok := seen[skill.Name]; ok {
				problems = append(problems, fmt.Sprintf("%s: skill %s is already in %s; this one is not used", display(filepath.Join(skillDir, FileName)), skill.Name, first))
				return
			}
			seen[skill.Name] = root.Label
			skill.Source = root.Label
			found = append(found, skill)
		})
	}
	slices.SortFunc(found, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })
	return found, problems
}

// walk calls visit for each directory under dir, dir included, that holds a SKILL.md,
// and does not look inside those. Symlinked directories are followed; the depth limit
// stops a loop.
func walk(dir string, depth int, visit func(dir string)) {
	if info, err := os.Stat(filepath.Join(dir, FileName)); err == nil && info.Mode().IsRegular() {
		visit(dir)
		return
	}
	if depth >= maxDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			walk(path, depth+1, visit)
		}
	}
}

// read reads the frontmatter of the SKILL.md in dir.
func read(dir string) (Skill, error) {
	_, fields, err := parseFile(filepath.Join(dir, FileName))
	if err != nil {
		return Skill{}, err
	}
	name := fields["name"]
	if name == "" {
		return Skill{}, errors.New("the frontmatter has no name")
	}
	if len(name) > maxNameLength || !namePattern.MatchString(name) {
		return Skill{}, fmt.Errorf("name %q is not lowercase letters, digits and single hyphens of at most %d characters", name, maxNameLength)
	}
	description := strings.Join(strings.Fields(fields["description"]), " ")
	if description == "" {
		return Skill{}, errors.New("the frontmatter has no description")
	}
	if len(description) > maxDescriptionLen {
		description = description[:maxDescriptionLen]
	}
	return Skill{
		Name:           name,
		Description:    description,
		Dir:            dir,
		ModelInvocable: !strings.EqualFold(fields["disable-model-invocation"], "true"),
	}, nil
}

// parseFile reads a SKILL.md and splits it into its body and frontmatter fields.
func parseFile(path string) (body string, fields map[string]string, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, err
	}
	if info.Size() > maxSkillBytes {
		return "", nil, fmt.Errorf("it is %d bytes; a SKILL.md can be at most %d", info.Size(), maxSkillBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	return parse(string(data))
}

// Find returns the skill with the given name among the roots.
func Find(roots []Root, name string) (Skill, bool) {
	found, _ := Discover(roots)
	for _, skill := range found {
		if skill.Name == name {
			return skill, true
		}
	}
	return Skill{}, false
}

// Instructions returns the body of the skill's SKILL.md, the part after the frontmatter.
func (s Skill) Instructions() (string, error) {
	body, _, err := parseFile(filepath.Join(s.Dir, FileName))
	return body, err
}

// ReadFile reads a file in the skill's directory, or lists a directory in it. path is
// relative to the skill's directory and cannot leave it, through ".." or a symlink.
func (s Skill) ReadFile(path string, secret func(name string) bool) (string, error) {
	if path == "" || filepath.IsAbs(path) || filepath.VolumeName(path) != "" {
		return "", fmt.Errorf("file must be a path relative to the skill's directory")
	}
	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the skill's directory", path)
	}
	if secret != nil && secret(filepath.Base(clean)) {
		return "", fmt.Errorf("%s may hold secrets, so it is not read", path)
	}
	root, err := filepath.EvalSymlinks(s.Dir)
	if err != nil {
		return "", err
	}
	target, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", fmt.Errorf("%s does not exist in the skill", path)
	}
	if rel, err := filepath.Rel(root, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the skill's directory", path)
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(target)
		if err != nil {
			return "", err
		}
		var names []string
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() {
				name += "/"
			}
			names = append(names, name)
		}
		if len(names) == 0 {
			return "(empty)", nil
		}
		return strings.Join(names, "\n"), nil
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > maxFileBytes {
		return "", fmt.Errorf("%s is %d bytes; files over %d are not read this way", path, info.Size(), maxFileBytes)
	}
	data, err := os.ReadFile(target)
	return string(data), err
}

// DisplayDir is the skill's directory relative to the workspace when it is inside it,
// and absolute when not.
func (s Skill) DisplayDir() string {
	return display(s.Dir)
}

// InWorkspace reports whether the skill's directory is inside the workspace, where the
// file tools reach it.
func (s Skill) InWorkspace() bool {
	return !filepath.IsAbs(display(s.Dir))
}

func display(path string) string {
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(rel)
		}
	}
	return path
}
