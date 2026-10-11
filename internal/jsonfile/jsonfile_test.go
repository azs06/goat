package jsonfile

import (
	"strings"
	"testing"
)

func TestObjectKeepsOrderAndLines(t *testing.T) {
	fields, err := Object([]byte("\ufeff{\n  \"b\": 1,\n\n  \"a\": {\"x\": 2}\n}\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 || fields[0].Name != "b" || fields[0].Line != 2 || fields[1].Name != "a" || fields[1].Line != 4 || string(fields[1].Value) != `{"x": 2}` {
		t.Fatalf("got %+v", fields)
	}
}

func TestObjectErrorsNeverQuoteTheFile(t *testing.T) {
	for content, want := range map[string]string{
		`[]`:                               "want an object",
		`{"a": 1, "a": 2}`:                 `a name "a" appears twice`,
		`{"sk-SECRET": 1, "sk-SECRET": 2}`: "a name appears twice",
		"{\n\"a\": SECRET}":                "line 2: not valid JSON",
		`{"a": 1} SECRET`:                  "after the closing }",
		`{"a": `:                           "ends too early",
	} {
		_, err := Object([]byte(content), "want an object")
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: got %v, want %q", content, err, want)
		}
	}
}

func TestShiftMovesLinesIntoTheWholeFile(t *testing.T) {
	if got := Shift(&Error{Line: 2, Message: "m"}, 10).Error(); got != "line 11: m" {
		t.Fatalf("got %q", got)
	}
	if got := Shift(&Error{Message: "m"}, 10).Error(); got != "m" {
		t.Fatalf("got %q", got)
	}
}
