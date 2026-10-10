package codemode

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/azs06/goat/internal/provider"
)

// Doc is what a script needs to know about a tool beyond its schema: the text it
// returns, and when it throws.
type Doc struct {
	Returns, Throws string
}

var identifierPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Declarations describes the goat object as a TypeScript-style declaration: one async
// function per tool, taking the tool's arguments as one object. The tools' descriptions
// are already in their specs, so only the return format from docs is added. The output
// is the same for the same input, since it is part of the cached prompt.
func Declarations(specs []provider.ToolSpec, docs map[string]Doc) string {
	var b strings.Builder
	b.WriteString("declare const goat: {\n")
	for _, spec := range specs {
		if !identifierPattern.MatchString(spec.Name) {
			panic(fmt.Sprintf("codemode: tool name %q is not a JavaScript identifier", spec.Name))
		}
		doc := docs[spec.Name]
		var notes []string
		if doc.Returns != "" {
			notes = append(notes, "@returns "+doc.Returns)
		}
		if doc.Throws != "" {
			notes = append(notes, "@throws "+doc.Throws)
		}
		switch len(notes) {
		case 0:
		case 1:
			fmt.Fprintf(&b, "  /** %s */\n", escapeComment(notes[0]))
		default:
			b.WriteString("  /**\n")
			for _, note := range notes {
				fmt.Fprintf(&b, "   * %s\n", escapeComment(note))
			}
			b.WriteString("   */\n")
		}

		input := "input: " + inputType(spec)
		if len(requiredNames(spec)) == 0 {
			input = "input?: " + inputType(spec)
		}
		fmt.Fprintf(&b, "  %s(%s): Promise<string>;\n", spec.Name, input)
	}
	b.WriteString("};")
	return b.String()
}

// ArgumentNames lists a tool's arguments: the required ones in schema order, then the
// rest sorted by name.
func ArgumentNames(spec provider.ToolSpec) []string {
	required := requiredNames(spec)
	properties, _ := spec.Parameters["properties"].(map[string]any)
	var optional []string
	for name := range properties {
		if !slices.Contains(required, name) {
			optional = append(optional, name)
		}
	}
	slices.Sort(optional)
	return append(slices.Clone(required), optional...)
}

func requiredNames(spec provider.ToolSpec) []string {
	switch required := spec.Parameters["required"].(type) {
	case []string:
		return required
	case []any:
		names := make([]string, 0, len(required))
		for _, name := range required {
			if s, ok := name.(string); ok {
				names = append(names, s)
			}
		}
		return names
	}
	return nil
}

func inputType(spec provider.ToolSpec) string {
	properties, _ := spec.Parameters["properties"].(map[string]any)
	required := requiredNames(spec)
	var fields []string
	for _, name := range ArgumentNames(spec) {
		schema, _ := properties[name].(map[string]any)
		optional := "?"
		if slices.Contains(required, name) {
			optional = ""
		}
		fields = append(fields, name+optional+": "+typeOf(schema))
	}
	if len(fields) == 0 {
		return "{}"
	}
	return "{ " + strings.Join(fields, "; ") + " }"
}

// typeOf maps a JSON schema to a TypeScript type.
func typeOf(schema map[string]any) string {
	var values []any
	switch enum := schema["enum"].(type) {
	case []string:
		for _, value := range enum {
			values = append(values, value)
		}
	case []any:
		values = enum
	}
	if len(values) > 0 {
		literals := make([]string, 0, len(values))
		for _, value := range values {
			literal, err := json.Marshal(value)
			if err != nil {
				continue
			}
			literals = append(literals, string(literal))
		}
		return strings.Join(literals, " | ")
	}
	switch schema["type"] {
	case "string":
		return "string"
	case "integer", "number":
		return "number"
	case "boolean":
		return "boolean"
	}
	return "unknown"
}

func escapeComment(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	return strings.ReplaceAll(text, "*/", `*\/`)
}
