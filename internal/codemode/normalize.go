package codemode

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/dop251/goja"
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
)

// scriptName is the file name goja reports in error positions.
const scriptName = "run_code"

// fencePattern matches code wrapped in one markdown fence, with an optional language.
var fencePattern = regexp.MustCompile("(?s)^```[a-zA-Z]*[ \t]*\n(.*?)\n?```$")

// source is the model's code made ready to run, with what is needed to report error
// positions in the code as the model sent it.
type source struct {
	code string
	// skipped is how many lines were removed before the code (blank lines, a fence).
	skipped int
	// The last expression was wrapped on line mainLine (of code); text inserted before
	// column mainColumn moves later columns on that line right by mainShift.
	mainLine, mainColumn, mainShift int
}

// stripFence removes one surrounding markdown fence and a leading "export default",
// and counts the lines removed before the code.
func stripFence(code string) source {
	var skipped int
	trimLeft := func(text string) string {
		trimmed := strings.TrimLeft(text, " \t\r\n")
		skipped += strings.Count(text[:len(text)-len(trimmed)], "\n")
		return trimmed
	}
	code = strings.TrimRight(trimLeft(code), " \t\r\n")
	if match := fencePattern.FindStringSubmatch(code); match != nil {
		skipped++
		code = strings.TrimRight(trimLeft(match[1]), " \t\r\n")
	}
	code = trimLeft(strings.TrimPrefix(code, "export default "))
	return source{code: strings.TrimRight(code, "; \t\r\n"), skipped: skipped}
}

// The code always runs inside an async function, since goja has no top-level await,
// and starts on line 2 of the program.
const (
	wrapStart = "(async () => {\n"
	wrapEnd   = "\n})()"
	mainStart = "const __goat_main = ("
	mainEnd   = "); return typeof __goat_main === \"function\" ? await __goat_main() : await __goat_main"
)

// compile turns the model's code into a program whose completion value is a promise.
// The code is the body of an async function. When it ends with an expression, that
// expression is its result: a function there (the usual async arrow) is called, and
// whatever comes back is awaited, so its errors are not lost. When it ends with a
// function declaration and never returns, that function is called.
func compile(src *source) (*goja.Program, error) {
	wrapped := wrapStart + src.code + wrapEnd
	program, err := parser.ParseFile(nil, scriptName, wrapped, 0)
	if err != nil {
		return nil, syntaxError(err, src)
	}

	if body := functionBody(program); len(body) > 0 {
		base := program.File.Base()
		switch last := body[len(body)-1].(type) {
		case *ast.ExpressionStatement:
			if start, end, ok := expressionBounds(wrapped, int(last.Idx0())-base, int(last.Idx1())-base); ok {
				before := wrapped[:start]
				src.mainLine = strings.Count(before, "\n")
				src.mainColumn = start - strings.LastIndex(before, "\n")
				src.mainShift = len(mainStart)
				wrapped = before + mainStart + wrapped[start:end] + mainEnd + wrapped[end:]
			}
		case *ast.FunctionDeclaration:
			if !returns(body) && last.Function.Name != nil {
				end := int(last.Idx1()) - base
				if end <= len(wrapped)-len(wrapEnd) {
					wrapped = wrapped[:end] + "; return await " + last.Function.Name.Name.String() + "()" + wrapped[end:]
				}
			}
		}
	}

	compiled, err := goja.Compile(scriptName, wrapped, false)
	if err != nil {
		return nil, syntaxError(err, src)
	}
	return compiled, nil
}

// expressionBounds finds the source of the last expression statement. The positions
// the parser gives leave out parentheses around the expression, so up to a few are
// taken in on each side, as many as keep the result valid JavaScript.
func expressionBounds(wrapped string, start, end int) (int, int, bool) {
	codeStart, codeEnd := len(wrapStart), len(wrapped)-len(wrapEnd)
	if start < codeStart || end > codeEnd || start > end {
		return 0, 0, false
	}
	starts := []int{start}
	for i := start; len(starts) < 4; {
		j := i
		for j > codeStart && strings.ContainsRune(" \t\r\n", rune(wrapped[j-1])) {
			j--
		}
		if j == codeStart || wrapped[j-1] != '(' {
			break
		}
		i = j - 1
		starts = append(starts, i)
	}
	ends := []int{end}
	for i := end; len(ends) < 4; {
		j := i
		for j < codeEnd && strings.ContainsRune(" \t\r\n", rune(wrapped[j])) {
			j++
		}
		if j == codeEnd || wrapped[j] != ')' {
			break
		}
		i = j + 1
		ends = append(ends, i)
	}
	for _, s := range starts {
		for _, e := range ends {
			candidate := wrapped[:s] + mainStart + wrapped[s:e] + mainEnd + wrapped[e:]
			if _, err := parser.ParseFile(nil, scriptName, candidate, 0); err == nil {
				return s, e, true
			}
		}
	}
	return 0, 0, false
}

// functionBody returns the statements of the async function the code was wrapped in.
func functionBody(program *ast.Program) []ast.Statement {
	if len(program.Body) != 1 {
		return nil
	}
	statement, ok := program.Body[0].(*ast.ExpressionStatement)
	if !ok {
		return nil
	}
	call, ok := statement.Expression.(*ast.CallExpression)
	if !ok {
		return nil
	}
	arrow, ok := call.Callee.(*ast.ArrowFunctionLiteral)
	if !ok {
		return nil
	}
	block, ok := arrow.Body.(*ast.BlockStatement)
	if !ok {
		return nil
	}
	return block.List
}

func returns(body []ast.Statement) bool {
	for _, statement := range body {
		if _, ok := statement.(*ast.ReturnStatement); ok {
			return true
		}
	}
	return false
}

func syntaxError(err error, src *source) error {
	message := strings.TrimPrefix(err.Error(), "SyntaxError: ")
	message = syntaxLinePattern.ReplaceAllStringFunc(message, func(match string) string {
		parts := syntaxLinePattern.FindStringSubmatch(match)
		return src.position(parts[1], parts[2])
	})
	return fmt.Errorf("SyntaxError: %s; run_code takes plain JavaScript: TypeScript annotations, import and require are not supported", message)
}

// syntaxLinePattern matches goja's "run_code: Line 3:8" in a SyntaxError.
var syntaxLinePattern = regexp.MustCompile(scriptName + `: Line (\d+):(\d+)`)

// framePattern matches a stack frame position such as "run_code:4:12(3)".
var framePattern = regexp.MustCompile(scriptName + `:(\d+):(\d+)`)

// errorPosition returns "line N, column M" for the first frame in a stack trace that
// points into the model's code, or "".
func (src *source) errorPosition(stack string) string {
	parts := framePattern.FindStringSubmatch(stack)
	if parts == nil {
		return ""
	}
	return src.position(parts[1], parts[2])
}

// position turns a line and column of the wrapped program into "line N, column M" of
// the code the model sent.
func (src *source) position(lineText, columnText string) string {
	line, _ := strconv.Atoi(lineText)
	column, _ := strconv.Atoi(columnText)
	if line == src.mainLine+1 && column >= src.mainColumn+src.mainShift {
		column -= src.mainShift
	}
	// Line 1 is the wrapper's own first line.
	line = max(line-1, 1) + src.skipped
	return fmt.Sprintf("line %d, column %d", line, column)
}

// summaryOpener matches a line that only opens the function, which says nothing about
// what the code does.
var summaryOpener = regexp.MustCompile(`^(async\s*)?(function\s*[\w$]*\s*\(\s*\)|\(\s*\))\s*(=>)?\s*\{?$`)

// Summary describes code in one line for display: the first line that does more than
// open the function, and the number of lines.
func Summary(code string) (string, int) {
	code = stripFence(code).code
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
