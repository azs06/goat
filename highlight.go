package main

import (
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"

	"github.com/azs06/goat/internal/style"
)

// Code blocks in replies are highlighted with chroma's lexers and goat's code-* theme
// roles. Replies arrive line by line, so each line is highlighted with the block so far
// as context: a string or comment that spans lines keeps its color.

// maxHighlightContext bounds the earlier lines re-read for each new line of a block.
const maxHighlightContext = 200

// codeLexer finds the lexer for a fence's language tag, or nil.
func codeLexer(lang string) chroma.Lexer {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "" {
		return nil
	}
	lexer := lexers.Get(lang)
	if lexer == nil {
		return nil
	}
	return chroma.Coalesce(lexer)
}

// tokenRole is the theme role a token is drawn with.
func tokenRole(t chroma.TokenType) style.Role {
	switch {
	case t.InCategory(chroma.Comment):
		return style.CodeComment
	case t == chroma.KeywordType:
		return style.CodeType
	case t == chroma.KeywordConstant:
		return style.CodeConstant
	case t.InCategory(chroma.Keyword), t == chroma.NameTag, t == chroma.OperatorWord:
		return style.CodeKeyword
	case t.InSubCategory(chroma.LiteralString):
		return style.CodeString
	case t.InSubCategory(chroma.LiteralNumber):
		return style.CodeNumber
	case t == chroma.NameFunction, t == chroma.NameFunctionMagic, t == chroma.NameDecorator, t == chroma.NameAttribute:
		return style.CodeFunction
	case t == chroma.NameClass, t == chroma.NameNamespace, t == chroma.NameBuiltinPseudo:
		return style.CodeType
	case t == chroma.NameBuiltin, t == chroma.NameConstant, t == chroma.NameVariableMagic:
		return style.CodeConstant
	default:
		return style.Code
	}
}

// highlightCodeLine renders the last of lines, highlighted as lang with the earlier
// lines as context. It returns false when lang has no lexer.
func highlightCodeLine(lang string, lines []string) (string, bool) {
	lexer := codeLexer(lang)
	if lexer == nil || len(lines) == 0 {
		return "", false
	}
	if len(lines) > maxHighlightContext {
		lines = lines[len(lines)-maxHighlightContext:]
	}
	iterator, err := lexer.Tokenise(nil, strings.Join(lines, "\n")+"\n")
	if err != nil {
		return "", false
	}

	// Collect the tokens of the last line: everything after the last newline before it.
	var current []chroma.Token
	row := 0
	last := len(lines) - 1
	for token := iterator(); token != chroma.EOF; token = iterator() {
		parts := strings.Split(token.Value, "\n")
		for i, part := range parts {
			if i > 0 {
				row++
			}
			if row == last && part != "" {
				current = append(current, chroma.Token{Type: token.Type, Value: part})
			}
		}
		if row > last {
			break
		}
	}

	var b strings.Builder
	for _, token := range current {
		b.WriteString(tokenRole(token.Type).Render(token.Value))
	}
	return b.String(), true
}
