package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/skills"
	"github.com/azs06/goat/internal/workspace"
)

// Skill is the name of the tool that loads a skill.
const Skill = "skill"

func skillSpec() provider.ToolSpec {
	return functionTool(Skill, "Load a skill: instructions for one kind of task, kept in a SKILL.md file with any scripts and references it uses. "+
		"The first message lists the skills there are; when a task matches a skill's description, load it by name and follow it. "+
		"file reads another file in the skill's directory, such as a reference it names, or lists a directory in it. "+
		"When the user asks for a new skill, write .agents/skills/<name>/SKILL.md: frontmatter between --- lines with name (lowercase letters, digits and hyphens, the same as the directory) and description (what it does and when to use it), then the instructions. It can be loaded at once.",
		map[string]any{"name": stringParam, "file": stringParam}, "name")
}

func runSkill(_ context.Context, raw string, _ Options) (string, error) {
	args, err := decodeArgs[struct {
		Name string `json:"name"`
		File string `json:"file"`
	}](raw)
	if err != nil {
		return "", err
	}
	skill, ok := skills.Find(skills.Roots(), strings.TrimSpace(args.Name))
	if !ok {
		return "", fmt.Errorf("there is no skill named %q; %s", args.Name, skillList())
	}
	if args.File != "" {
		content, err := skill.ReadFile(args.File, workspace.IsSecretFileName)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("File %s of skill %s:\n%s", args.File, skill.Name, content), nil
	}
	body, err := skill.Instructions()
	if err != nil {
		return "", err
	}
	header := fmt.Sprintf("Skill %s, from %s. Its directory is %s, and the paths in it are relative to that directory.", skill.Name, skill.Source, skill.DisplayDir())
	if !skill.InWorkspace() {
		header += " It is outside the workspace: read its other files with skill and file, and run its scripts with run_bash by their absolute path."
	}
	return header + "\n\n" + body, nil
}

// skillList names the skills there are, for a call that asked for one that is not.
func skillList() string {
	found, _ := skills.Discover(skills.Roots())
	if len(found) == 0 {
		return "there are no skills"
	}
	names := make([]string, len(found))
	for i, skill := range found {
		names[i] = skill.Name
	}
	return "the skills are: " + strings.Join(names, ", ")
}
