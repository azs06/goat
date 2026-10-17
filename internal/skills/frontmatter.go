package skills

import (
	"errors"
	"strconv"
	"strings"
)

// parse splits a SKILL.md into its body and the top-level fields of its YAML
// frontmatter. It reads the YAML skills use rather than all of it: plain, quoted and
// block (| and >) scalars, and plain values continued on indented lines. Nested values,
// such as a metadata map, are skipped.
func parse(text string) (body string, fields map[string]string, err error) {
	text = strings.TrimPrefix(text, "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t") != "---" {
		return "", nil, errors.New("it does not start with --- frontmatter")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return "", nil, errors.New("its frontmatter has no closing ---")
	}
	fields = map[string]string{}
	header := lines[1:end]
	for i := 0; i < len(header); i++ {
		line := header[i]
		if line == "" || indented(line) || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		// The indented lines after it: a block scalar's text, a plain value's
		// continuation, or a nested value.
		var more []string
		for i+1 < len(header) && (header[i+1] == "" || indented(header[i+1])) {
			i++
			more = append(more, header[i])
		}
		fields[key] = scalar(value, more)
	}
	return strings.TrimSpace(strings.Join(lines[end+1:], "\n")), fields, nil
}

func indented(line string) bool {
	return strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
}

// scalar is the value of a key whose line holds value, followed by the indented lines
// more.
func scalar(value string, more []string) string {
	switch {
	case strings.HasPrefix(value, "|") || strings.HasPrefix(value, ">"):
		return block(value[0] == '>', more)
	case strings.HasPrefix(value, `"`):
		joined := strings.Join(append([]string{value}, trimAll(more)...), " ")
		if unquoted, err := strconv.Unquote(strings.TrimSpace(stripComment(joined, true))); err == nil {
			return unquoted
		}
		return strings.Trim(joined, `"`)
	case strings.HasPrefix(value, "'"):
		joined := strings.TrimSpace(stripComment(strings.Join(append([]string{value}, trimAll(more)...), " "), true))
		joined = strings.TrimSuffix(strings.TrimPrefix(joined, "'"), "'")
		return strings.ReplaceAll(joined, "''", "'")
	}
	value = stripComment(value, false)
	if value == "" {
		// A nested map or list, not text.
		for _, line := range more {
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "- ") || strings.Contains(trimmed, ": ") || strings.HasSuffix(trimmed, ":") {
				return ""
			}
		}
	}
	return strings.TrimSpace(strings.Join(append([]string{value}, trimAll(more)...), " "))
}

// block is the text of a block scalar: its lines joined with newlines, or with spaces
// when folded, less their common indentation.
func block(folded bool, lines []string) string {
	indent := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if n := len(line) - len(strings.TrimLeft(line, " \t")); indent < 0 || n < indent {
			indent = n
		}
	}
	var out []string
	for _, line := range lines {
		if len(line) >= indent && indent > 0 {
			line = line[indent:]
		} else {
			line = strings.TrimSpace(line)
		}
		out = append(out, strings.TrimRight(line, " \t"))
	}
	sep := "\n"
	if folded {
		sep = " "
	}
	return strings.TrimSpace(strings.Join(out, sep))
}

func trimAll(lines []string) []string {
	var out []string
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// stripComment drops a trailing " #" comment. In a quoted value only one after the
// closing quote counts.
func stripComment(value string, quoted bool) string {
	start := 0
	if quoted {
		if closing := strings.LastIndexAny(value, `"'`); closing > 0 {
			start = closing
		}
	}
	if i := strings.Index(value[start:], " #"); i >= 0 {
		return strings.TrimSpace(value[:start+i])
	}
	return value
}
