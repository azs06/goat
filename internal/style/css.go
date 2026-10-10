package style

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// declarations is a set of CSS declarations, property name to value.
type declarations map[string]string

var cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

// parseDeclarations reads a CSS declaration block such as
// "color: #eee; font-weight: bold", with or without braces. Property names are
// case-insensitive, and !important is accepted and ignored.
func parseDeclarations(text string) (declarations, []string) {
	text = cssComment.ReplaceAllString(text, "")
	text = strings.TrimSpace(text)
	text = strings.TrimSuffix(strings.TrimPrefix(text, "{"), "}")

	decls := declarations{}
	var problems []string
	for _, part := range strings.Split(text, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, ":")
		if !ok {
			problems = append(problems, fmt.Sprintf("%q is not a declaration (want property: value)", part))
			continue
		}
		decls.set(name, value)
	}
	return decls, problems
}

func (d declarations) set(name, value string) {
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, "--") {
		// Custom properties are case-sensitive, like in CSS; the rest are not.
		name = strings.ToLower(name)
	}
	value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), "!important"))
	d[name] = value
}

// resolveVars replaces var(--name) and var(--name, fallback) with the custom
// properties in vars. References may nest, up to a fixed depth that also stops cycles.
func resolveVars(value string, vars declarations) (string, error) {
	for depth := 0; ; depth++ {
		start := strings.Index(value, "var(")
		if start < 0 {
			return value, nil
		}
		if depth == 20 {
			return "", fmt.Errorf("var() references nest too deep (a cycle?)")
		}
		end, nesting := -1, 0
		for i := start + len("var("); i < len(value) && end < 0; i++ {
			switch value[i] {
			case '(':
				nesting++
			case ')':
				if nesting == 0 {
					end = i
				}
				nesting--
			}
		}
		if end < 0 {
			return "", fmt.Errorf("unclosed var() in %q", value)
		}
		name, fallback, hasFallback := strings.Cut(value[start+len("var("):end], ",")
		name = strings.TrimSpace(name)
		replacement, ok := vars[name]
		switch {
		case ok:
		case hasFallback:
			replacement = strings.TrimSpace(fallback)
		default:
			return "", fmt.Errorf("undefined variable %s", name)
		}
		value = value[:start] + replacement + value[end+1:]
	}
}

// compile turns a role's declarations into a Style. Declarations that cannot be used
// are reported and skipped; the rest still apply.
func compile(decls declarations, vars declarations) (Style, []string) {
	var s Style
	var problems []string
	names := make([]string, 0, len(decls))
	for name := range decls {
		if !strings.HasPrefix(name, "--") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		raw := decls[name]
		value, err := resolveVars(raw, vars)
		if err == nil {
			err = applyProperty(&s, name, value)
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
		}
	}
	return s, problems
}

// applyProperty sets the attribute one CSS property controls.
func applyProperty(s *Style, name, value string) error {
	v := strings.ToLower(strings.TrimSpace(value))
	switch name {
	case "color":
		c, err := parseColor(value)
		if err != nil {
			return err
		}
		s.Fg = c
	case "background-color", "background":
		c, err := parseColor(value)
		if err != nil {
			return err
		}
		s.Bg = c
	case "font-weight":
		switch v {
		case "bold", "bolder":
			s.Bold, s.Faint = true, false
		case "normal":
			s.Bold, s.Faint = false, false
		case "lighter":
			s.Bold, s.Faint = false, true
		default:
			weight, err := strconv.Atoi(v)
			if err != nil || weight < 1 || weight > 1000 {
				return fmt.Errorf("unknown weight %q (use normal, bold, lighter or 1-1000)", value)
			}
			s.Bold, s.Faint = weight >= 600, weight < 400
		}
	case "font-style":
		switch v {
		case "italic", "oblique":
			s.Italic = true
		case "normal":
			s.Italic = false
		default:
			return fmt.Errorf("unknown font style %q (use normal or italic)", value)
		}
	case "text-decoration", "text-decoration-line":
		s.Underline, s.Strike = false, false
		for _, word := range strings.Fields(v) {
			switch word {
			case "underline":
				s.Underline = true
			case "line-through":
				s.Strike = true
			case "none":
			default:
				return fmt.Errorf("unsupported decoration %q (use underline, line-through or none)", word)
			}
		}
	default:
		return fmt.Errorf("unsupported property (supported: color, background-color, font-weight, font-style, text-decoration)")
	}
	return nil
}
