package codemode

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/dop251/goja"
)

// scriptName is the file name goja reports in error positions.
const scriptName = "run_code"

// fencePattern matches code wrapped in one markdown fence, with an optional language.
var fencePattern = regexp.MustCompile("(?s)^```[a-zA-Z]*[ \t]*\n(.*?)\n?```$")

// stripFence removes one surrounding markdown fence and a leading "export default".
func stripFence(code string) string {
	code = strings.TrimSpace(code)
	if match := fencePattern.FindStringSubmatch(code); match != nil {
		code = strings.TrimSpace(match[1])
	}
	code = strings.TrimSpace(strings.TrimPrefix(code, "export default "))
	return strings.TrimRight(code, "; \t\r\n")
}

// compile turns the model's code into a program whose completion value is a promise.
// goja has no top-level await, so the code always runs inside an async function. It is
// tried first as one expression (an arrow function, a function, or a call), which is
// called if it is a function and awaited either way; otherwise it is the body of an
// async function and must return its result. In both forms the code starts on line 2,
// which errorPosition corrects for.
func compile(code string) (*goja.Program, error) {
	asExpression := "(async () => { const __goat_main = (\n" + code + "\n); return typeof __goat_main === \"function\" ? await __goat_main() : await __goat_main; })()"
	expressionProgram, expressionErr := goja.Compile(scriptName, asExpression, false)
	if expressionErr == nil {
		return expressionProgram, nil
	}
	asBody := "(async () => {\n" + code + "\n})()"
	bodyProgram, bodyErr := goja.Compile(scriptName, asBody, false)
	if bodyErr == nil {
		return bodyProgram, nil
	}

	// Both failed. Code that starts like a function is reported as one.
	err := bodyErr
	if looksLikeFunction(code) {
		err = expressionErr
	}
	return nil, fmt.Errorf("%s; run_code takes plain JavaScript: TypeScript annotations, import and require are not supported", fixSyntaxErrorLine(err.Error()))
}

func looksLikeFunction(code string) bool {
	for _, prefix := range []string{"async ", "async(", "function", "("} {
		if strings.HasPrefix(code, prefix) {
			return true
		}
	}
	return false
}

// syntaxLinePattern matches goja's "run_code: Line 3:8" in a SyntaxError.
var syntaxLinePattern = regexp.MustCompile(scriptName + `: Line (\d+):(\d+)`)

func fixSyntaxErrorLine(message string) string {
	return syntaxLinePattern.ReplaceAllStringFunc(message, func(match string) string {
		parts := syntaxLinePattern.FindStringSubmatch(match)
		return "line " + shiftLine(parts[1]) + ", column " + parts[2]
	})
}

// framePattern matches a stack frame position such as "run_code:4:12(3)".
var framePattern = regexp.MustCompile(scriptName + `:(\d+):(\d+)`)

// errorPosition returns "line N, column M" for the first frame in a stack trace that
// points into the model's code, or "".
func errorPosition(stack string) string {
	parts := framePattern.FindStringSubmatch(stack)
	if parts == nil || shiftLine(parts[1]) == "0" {
		return ""
	}
	return "line " + shiftLine(parts[1]) + ", column " + parts[2]
}

// shiftLine moves a line number in the wrapped program to the model's code, which
// starts on line 2.
func shiftLine(line string) string {
	var n int
	fmt.Sscanf(line, "%d", &n)
	if n > 1 {
		n--
	}
	return fmt.Sprint(n)
}

// summaryOpener matches a line that only opens the function, which says nothing about
// what the code does.
var summaryOpener = regexp.MustCompile(`^(async\s*)?(function\s*[\w$]*\s*\(\s*\)|\(\s*\))\s*(=>)?\s*\{?$`)

// Summary describes code in one line for display: the first line that does more than
// open the function, and the number of lines.
func Summary(code string) (string, int) {
	code = stripFence(code)
	if code == "" {
		return "", 0
	}
	lines := strings.Split(code, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !summaryOpener.MatchString(line) {
			return line, len(lines)
		}
	}
	return strings.TrimSpace(lines[0]), len(lines)
}
