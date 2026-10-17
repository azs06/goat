package main

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/azs06/goat/internal/skills"
	"github.com/azs06/goat/internal/style"
)

// commandSkills lists the skills, and any SKILL.md that could not be used.
func commandSkills(ctx context.Context, c *config, args []string) error {
	found, problems := skills.Discover(skills.Roots())
	if len(found) == 0 {
		fmt.Println(formatNotice("No skills. Put one in .agents/skills/<name>/SKILL.md, or ~/.config/goat/skills/<name>/SKILL.md for every project."))
	} else {
		fmt.Println(style.Strong.Render(fmt.Sprintf("Skills (%d)", len(found))))
		fmt.Println()
		rows := make([][2]string, 0, len(found))
		for _, skill := range found {
			detail := skill.Description + " " + style.Muted.Render("("+skill.Source)
			if !skill.ModelInvocable {
				detail += style.Muted.Render(", only with /skill")
			}
			rows = append(rows, [2]string{skill.Name, detail + style.Muted.Render(")")})
		}
		fmt.Println(renderKeyValues(rows))
	}
	for _, problem := range problems {
		fmt.Println(style.Muted.Render("Not loaded: " + problem))
	}
	return nil
}

// commandSkill sends the skill's instructions with the rest of the line as the request,
// for a skill the model would not pick on its own or one only the user loads.
func commandSkill(ctx context.Context, c *config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("use /skill <name> [request]; /skills lists them")
	}
	skill, ok := skills.Find(skills.Roots(), args[0])
	if !ok {
		return fmt.Errorf("there is no skill named %q; /skills lists them", args[0])
	}
	body, err := skill.Instructions()
	if err != nil {
		return err
	}
	text := c.commandText
	if text == "" {
		text = "/skill " + strings.Join(args, " ")
	}
	return runPrompt(ctx, c, skillPrompt(skill, body, skillRequest(text)))
}

// skillRequest is what follows "/skill <name>" in the typed text, line breaks kept.
func skillRequest(text string) string {
	rest := strings.TrimSpace(text)
	for range 2 {
		if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
			rest = strings.TrimSpace(rest[i:])
		} else {
			rest = ""
		}
	}
	return rest
}

// skillPrompt is the prompt /skill sends: the skill, then the request.
func skillPrompt(skill skills.Skill, body, request string) string {
	if request == "" {
		request = "Follow this skill."
	}
	return fmt.Sprintf("Use the skill %s for this request. Its directory is %s, and the paths in it are relative to that directory.\n\n<skill name=%q>\n%s\n</skill>\n\n%s",
		skill.Name, skill.DisplayDir(), skill.Name, body, request)
}
