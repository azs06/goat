// Package jsonfile reads goat's JSON settings files: config.json, auth.json and the
// providers in them. Its errors name the line of a mistake but never repeat the file's
// text, since a key may have been pasted anywhere in it.
package jsonfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Error is a mistake in a file, by line (0 when it has none).
type Error struct {
	Line    int
	Message string
}

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Message)
	}
	return e.Message
}

// Shift moves an error found in a part of a file that starts on line start to the
// file's own numbering. Other errors are returned as they are.
func Shift(err error, start int) error {
	var fileErr *Error
	if errors.As(err, &fileErr) && fileErr.Line > 0 {
		return &Error{Line: fileErr.Line + start - 1, Message: fileErr.Message}
	}
	return err
}

// Field is one member of a JSON object: its name, its raw value and the line the name
// is on.
type Field struct {
	Name  string
	Value json.RawMessage
	Line  int
}

// Object reads the JSON object data holds, member by member in file order. A member
// named twice is refused: the decoder would quietly keep the last one. what describes
// the object for the error when data is not one, e.g. "the file must be a JSON object".
func Object(data []byte, what string) ([]Field, error) {
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, syntaxError(data, err, what)
	}
	var fields []Field
	seen := map[string]bool{}
	for decoder.More() {
		line := LineAt(data, decoder.InputOffset())
		token, err := decoder.Token()
		if err != nil {
			return nil, syntaxError(data, err, "")
		}
		name, _ := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, syntaxError(data, err, "")
		}
		if seen[name] {
			return nil, &Error{Line: line, Message: Quote(name, "a name") + " appears twice"}
		}
		seen[name] = true
		fields = append(fields, Field{Name: name, Value: value, Line: line})
	}
	if _, err := decoder.Token(); err != nil {
		return nil, syntaxError(data, err, "")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, &Error{Line: LineAt(data, decoder.InputOffset()), Message: "unexpected text after the closing }"}
	}
	return fields, nil
}

// safeText is text short and plain enough to be a name, never a key.
var safeText = regexp.MustCompile(`^[a-z][a-z_-]{0,19}$`)

// Quote quotes text from a file when it cannot be a pasted key, and otherwise gives
// description alone.
func Quote(text, description string) string {
	if safeText.MatchString(text) {
		return fmt.Sprintf("%s %q", description, text)
	}
	return description
}

// LooksLikeKeyField reports a field name that suggests a key was meant to go there.
func LooksLikeKeyField(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "key") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password")
}

// syntaxError describes invalid JSON by line only: the decoder's own message quotes the
// text it stopped at.
func syntaxError(data []byte, err error, fallback string) error {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		// The streaming decoder's offset leaves out what earlier tokens consumed, so the
		// whole file is checked again for the true position.
		offset := syntax.Offset
		var whole any
		if errors.As(json.Unmarshal(data, &whole), &syntax) {
			offset = syntax.Offset
		}
		return &Error{Line: LineAt(data, offset-1), Message: "not valid JSON: look here or at the end of the line before for a missing comma, quote or brace"}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &Error{Message: "the file ends too early: look for a missing closing brace or quote"}
	}
	if fallback == "" {
		fallback = "not valid JSON"
	}
	return &Error{Message: fallback}
}

// LineAt is the line of the first character at or after offset that is not white space
// or a comma, which is where the decoder's next token starts.
func LineAt(data []byte, offset int64) int {
	offset = min(max(offset, 0), int64(len(data)))
	for offset < int64(len(data)) && strings.ContainsRune(" \t\r\n,:", rune(data[offset])) {
		offset++
	}
	return 1 + bytes.Count(data[:offset], []byte("\n"))
}
