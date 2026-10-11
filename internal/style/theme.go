package style

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"
)

// Role names what a piece of text is; the theme decides how it looks.
type Role string

const (
	Accent        Role = "accent"         // borders, markers and glyphs that draw the eye
	Muted         Role = "muted"          // secondary text: descriptions, details
	Subtle        Role = "subtle"         // tertiary text: rules, hints, the footer
	Strong        Role = "strong"         // **bold** text and labels
	Emphasis      Role = "emphasis"       // *italic* text
	Highlight     Role = "highlight"      // a value that matters: the current model, effort
	Title         Role = "title"          // the welcome banner's title
	Heading       Role = "heading"        // markdown # and ## headings
	Subheading    Role = "subheading"     // markdown ### headings and smaller
	Quote         Role = "quote"          // markdown > quotes
	Code          Role = "code"           // lines inside a fenced code block
	CodeComment   Role = "code-comment"   // comments inside a fenced code block
	CodeKeyword   Role = "code-keyword"   // keywords in highlighted code
	CodeString    Role = "code-string"    // string literals in highlighted code
	CodeNumber    Role = "code-number"    // number literals in highlighted code
	CodeFunction  Role = "code-function"  // function names in highlighted code
	CodeType      Role = "code-type"      // type and class names in highlighted code
	CodeConstant  Role = "code-constant"  // constants and builtins in highlighted code
	InlineCode    Role = "inline-code"    // `code` spans
	Success       Role = "success"        // success marks and messages
	Error         Role = "error"          // error marks and messages
	Warning       Role = "warning"        // warnings and the approval card frame
	ApprovalTitle Role = "approval-title" // the approval card's "Run command?"
	DiffAdded     Role = "diff-added"     // + lines in a diff
	DiffRemoved   Role = "diff-removed"   // - lines in a diff
	Prompt        Role = "prompt"         // the ❯ in front of the input line
	UserMarker    Role = "user-marker"    // the ❯ in front of a submitted message
	UserMessage   Role = "user-message"   // the text of a submitted message
	Selected      Role = "selected"       // the highlighted entry of a menu
)

// Roles lists every role a theme can style.
var Roles = []Role{
	Accent, Muted, Subtle, Strong, Emphasis, Highlight, Title, Heading, Subheading, Quote,
	Code, CodeComment, CodeKeyword, CodeString, CodeNumber, CodeFunction, CodeType, CodeConstant, InlineCode, Success, Error, Warning, ApprovalTitle, DiffAdded,
	DiffRemoved, Prompt, UserMarker, UserMessage, Selected,
}

// Style is the role's style in the current theme.
func (r Role) Style() Style { return current.styles[r] }

// Render wraps text in the role's style; with styling off it returns text unchanged.
func (r Role) Render(text string) string { return r.Style().Render(text) }

// rootSelector holds custom properties (--name: value) that styles use with var().
const rootSelector = ":root"

// Theme maps roles to styles. It keeps its declarations, so another theme file can be
// layered on top of it property by property, the way CSS cascades.
type Theme struct {
	rules  map[string]declarations
	styles map[Role]Style
}

// themeFiles are the built-in themes, one JSON file each, named after the file.
//
//go:embed themes/*.json
var themeFiles embed.FS

// DefaultThemeName is the theme goat starts with: black and white.
const DefaultThemeName = "default"

var (
	defaultTheme = mustParseBuiltin(DefaultThemeName, nil)
	builtins     = parseBuiltins()
	current      = defaultTheme
)

// mustParseBuiltin parses a built-in theme over base. A broken one is a bug in goat.
func mustParseBuiltin(name string, base *Theme) *Theme {
	data, err := themeFiles.ReadFile("themes/" + name + ".json")
	if err != nil {
		panic(fmt.Sprintf("style: no built-in theme %q: %v", name, err))
	}
	theme, problems, err := ParseTheme(data, base)
	if err != nil || len(problems) > 0 {
		panic(fmt.Sprintf("style: the built-in theme %q is broken: %v %v", name, err, problems))
	}
	return theme
}

// parseBuiltins reads every built-in theme, each layered over the default one.
func parseBuiltins() map[string]*Theme {
	themes := map[string]*Theme{DefaultThemeName: defaultTheme}
	entries, _ := themeFiles.ReadDir("themes")
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".json")
		if name != DefaultThemeName {
			themes[name] = mustParseBuiltin(name, defaultTheme)
		}
	}
	return themes
}

// Default is the built-in default theme.
func Default() *Theme { return defaultTheme }

// Builtin returns the built-in theme with this name, such as "one-dark".
func Builtin(name string) (*Theme, bool) {
	canonical, ok := BuiltinName(name)
	return builtins[canonical], ok
}

// BuiltinName returns the name of the built-in theme name refers to. Case, spaces,
// hyphens and underscores do not matter, so "One Dark" and "onedark" are one-dark.
func BuiltinName(name string) (string, bool) {
	key := themeKey(name)
	for canonical := range builtins {
		if themeKey(canonical) == key {
			return canonical, true
		}
	}
	return "", false
}

// SuggestBuiltin returns the built-in theme a misspelled name most likely meant, such as
// one-dark for "ondedark" or "dark": the one closest to it, if that is at most two edits
// away or contains the name whole.
func SuggestBuiltin(name string) (string, bool) {
	key := themeKey(name)
	best, bestDistance := "", 3
	for _, canonical := range BuiltinNames() {
		distance := editDistance(key, themeKey(canonical))
		if len(key) >= 3 && strings.Contains(themeKey(canonical), key) {
			distance = min(distance, 2)
		}
		if distance < bestDistance {
			best, bestDistance = canonical, distance
		}
	}
	return best, best != ""
}

// themeKey is what is left of a theme's name to compare: lowercase, without spaces,
// hyphens or underscores.
func themeKey(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, name)
}

// editDistance counts the single-rune insertions, deletions and substitutions that turn a
// into b.
func editDistance(a, b string) int {
	x, y := []rune(a), []rune(b)
	previous := make([]int, len(y)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(x); i++ {
		current := make([]int, len(y)+1)
		current[0] = i
		for j := 1; j <= len(y); j++ {
			cost := 1
			if x[i-1] == y[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous = current
	}
	return previous[len(y)]
}

// BuiltinNames lists the built-in themes, the default first.
func BuiltinNames() []string {
	names := []string{DefaultThemeName}
	for _, name := range sortedKeys(builtins) {
		if name != DefaultThemeName {
			names = append(names, name)
		}
	}
	return names
}

// Use makes theme the current one. Call it while nothing else is rendering: at
// startup or between turns, never while the spinner runs.
func Use(theme *Theme) { current = theme }

// Style is a role's style in this theme.
func (t *Theme) Style(role Role) Style { return t.styles[role] }

// themeFile is the JSON layout of a theme:
//
//	{
//	  "styles": {
//	    ":root":   "--fg: #eeeeee",
//	    "accent":  "color: var(--fg)",
//	    "heading": {"color": "var(--fg)", "font-weight": "bold"}
//	  }
//	}
//
// A style is a CSS declaration block, written as a string or as an object of
// properties. "extends" names a built-in theme to layer the file over instead of the
// default one. Keys starting with "$" (such as "$schema") are ignored.
type themeFile struct {
	Extends string                     `json:"extends"`
	Styles  map[string]json.RawMessage `json:"styles"`
}

// ParseTheme reads a theme from JSON and layers it over base (nil for no base, which
// leaves roles the theme does not mention unstyled). It returns an error only when
// the JSON cannot be read as a theme; declarations that cannot be used are returned
// as problems and skipped, and the rest of the theme still applies.
func ParseTheme(data []byte, base *Theme) (*Theme, []string, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, nil, fmt.Errorf("invalid theme JSON: %w", err)
	}
	var problems []string
	for key := range top {
		if key != "styles" && key != "extends" && !strings.HasPrefix(key, "$") {
			problems = append(problems, fmt.Sprintf("unknown top-level key %q (styles go under \"styles\")", key))
		}
	}
	var file themeFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, nil, fmt.Errorf("invalid theme: %w", err)
	}

	theme := &Theme{rules: map[string]declarations{}}
	if base != nil {
		for selector, decls := range base.rules {
			theme.rules[selector] = cloneDeclarations(decls)
		}
	}

	known := map[string]bool{rootSelector: true}
	for _, role := range Roles {
		known[string(role)] = true
	}
	for _, key := range sortedKeys(file.Styles) {
		if strings.HasPrefix(key, "$") {
			continue
		}
		// ".accent" is accepted too, so a role can be written like a CSS class.
		selector := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(key)), ".")
		if !known[selector] {
			problems = append(problems, fmt.Sprintf("unknown style %q (known: %s)", key, roleNames()))
			continue
		}
		decls, declProblems := decodeDeclarations(file.Styles[key])
		for _, p := range declProblems {
			problems = append(problems, selector+": "+p)
		}
		if theme.rules[selector] == nil {
			theme.rules[selector] = declarations{}
		}
		for name, value := range decls {
			theme.rules[selector][name] = value
		}
	}

	theme.styles = map[Role]Style{}
	vars := theme.rules[rootSelector]
	for _, role := range Roles {
		style, styleProblems := compile(theme.rules[string(role)], vars)
		theme.styles[role] = style
		for _, p := range styleProblems {
			problems = append(problems, string(role)+": "+p)
		}
	}
	return theme, problems, nil
}

// LoadTheme reads the theme file at path and layers it over the default theme, or over
// the built-in theme its "extends" names.
func LoadTheme(path string) (*Theme, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var file themeFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, nil, fmt.Errorf("invalid theme JSON: %w", err)
	}
	base, problem := defaultTheme, ""
	if name := strings.TrimSpace(file.Extends); name != "" {
		if builtin, ok := Builtin(name); ok {
			base = builtin
		} else {
			problem = fmt.Sprintf("extends: there is no built-in theme %q (built in: %s)", name, strings.Join(BuiltinNames(), ", "))
		}
	}
	theme, problems, err := ParseTheme(data, base)
	if problem != "" {
		problems = append([]string{problem}, problems...)
	}
	return theme, problems, err
}

// decodeDeclarations reads one style: a CSS declaration string or an object whose
// values are strings or numbers (for font-weight: 700).
func decodeDeclarations(raw json.RawMessage) (declarations, []string) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return parseDeclarations(text)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, []string{"a style must be a CSS declaration string or an object of properties"}
	}
	decls := declarations{}
	var problems []string
	for _, name := range sortedKeys(object) {
		switch value := object[name].(type) {
		case string:
			decls.set(name, value)
		case float64:
			decls.set(name, fmt.Sprint(value))
		default:
			problems = append(problems, fmt.Sprintf("%s: the value must be a string or a number", name))
		}
	}
	return decls, problems
}

func cloneDeclarations(d declarations) declarations {
	out := make(declarations, len(d))
	for k, v := range d {
		out[k] = v
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func roleNames() string {
	names := make([]string, len(Roles))
	for i, role := range Roles {
		names[i] = string(role)
	}
	return strings.Join(names, ", ")
}
