// Package postgres implements the PostgreSQL dialect driver. This file
// is the lexer profile: token boundaries only, no SQL understanding.
package postgres

import (
	"github.com/moznion/go-sqletch/internal/dialect"
)

type Profile struct{}

var (
	_ dialect.LexerProfile = Profile{}
	_ dialect.Placeholders = Profile{}
)

// PlaceholderStyle declares PostgreSQL's numbered-with-reuse binding.
func (Profile) PlaceholderStyle() dialect.PlaceholderStyle { return dialect.PlaceholderDollar }

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}
func isIdentCont(c byte) bool { return isIdentStart(c) || isDigit(c) || c == '$' }

// PostgreSQL operator characters (lexer rules), minus paren/comma
// handled separately. '@' is included: constructs are matched by the
// scanner *before* this profile ever sees the '@'.
func isOpChar(c byte) bool {
	switch c {
	case '+', '-', '*', '/', '<', '>', '=', '~', '!', '@', '#', '%', '^', '&', '|', '`', '?':
		return true
	}
	return false
}

func (Profile) NextToken(src []byte, pos int) (dialect.Token, error) {
	if pos >= len(src) {
		return dialect.Token{Kind: dialect.KindEOF, Start: pos, End: pos}, nil
	}
	c := src[pos]
	mk := func(kind dialect.TokenKind, end int) (dialect.Token, error) {
		return dialect.Token{Kind: kind, Start: pos, End: end, Text: string(src[pos:end])}, nil
	}

	switch {
	case isSpace(c):
		i := pos
		for i < len(src) && isSpace(src[i]) {
			i++
		}
		return mk(dialect.KindWhitespace, i)

	case c == '-' && pos+1 < len(src) && src[pos+1] == '-':
		i := pos + 2
		for i < len(src) && src[i] != '\n' && src[i] != '\r' {
			i++
		}
		return mk(dialect.KindLineComment, i)

	case c == '/' && pos+1 < len(src) && src[pos+1] == '*':
		depth := 1
		i := pos + 2
		for i < len(src) && depth > 0 {
			if i+1 < len(src) && src[i] == '/' && src[i+1] == '*' {
				depth++
				i += 2
			} else if i+1 < len(src) && src[i] == '*' && src[i+1] == '/' {
				depth--
				i += 2
			} else {
				i++
			}
		}
		if depth > 0 {
			return dialect.Token{}, &dialect.LexError{Pos: pos, Msg: "unterminated block comment"}
		}
		return mk(dialect.KindBlockComment, i)

	case c == '\'':
		end, err := lexQuoted(src, pos+1, '\'', false)
		if err != nil {
			return dialect.Token{}, err
		}
		return mk(dialect.KindString, end)

	case (c == 'e' || c == 'E') && pos+1 < len(src) && src[pos+1] == '\'':
		end, err := lexQuoted(src, pos+2, '\'', true)
		if err != nil {
			return dialect.Token{}, err
		}
		return mk(dialect.KindString, end)

	case c == '"':
		end, err := lexQuoted(src, pos+1, '"', false)
		if err != nil {
			return dialect.Token{}, err
		}
		return mk(dialect.KindQuotedIdent, end)

	case c == '$':
		// $tag$…$tag$ or $$…$$ dollar-quoted string; $1 positional.
		if end, ok := lexDollarQuote(src, pos); ok {
			if end < 0 {
				return dialect.Token{}, &dialect.LexError{Pos: pos, Msg: "unterminated dollar-quoted string"}
			}
			return mk(dialect.KindString, end)
		}
		if pos+1 < len(src) && isDigit(src[pos+1]) {
			i := pos + 1
			for i < len(src) && isDigit(src[i]) {
				i++
			}
			return mk(dialect.KindPositionalParam, i)
		}
		return mk(dialect.KindOther, pos+1)

	case c == ':':
		if pos+1 < len(src) && src[pos+1] == ':' {
			return mk(dialect.KindCast, pos+2)
		}
		if pos+1 < len(src) && isIdentStart(src[pos+1]) {
			i := pos + 1
			for i < len(src) && isIdentCont(src[i]) && src[i] != '$' {
				i++
			}
			return mk(dialect.KindParamRef, i)
		}
		return mk(dialect.KindOther, pos+1)

	case c == '(':
		return mk(dialect.KindLParen, pos+1)
	case c == ')':
		return mk(dialect.KindRParen, pos+1)
	case c == ',':
		return mk(dialect.KindComma, pos+1)
	case c == ';':
		return mk(dialect.KindSemicolon, pos+1)

	case isIdentStart(c):
		i := pos
		for i < len(src) && isIdentCont(src[i]) {
			i++
		}
		return mk(dialect.KindIdent, i)

	case isDigit(c) || (c == '.' && pos+1 < len(src) && isDigit(src[pos+1])):
		i := pos
		for i < len(src) && (isDigit(src[i]) || src[i] == '.') {
			i++
		}
		// exponent
		if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
			j := i + 1
			if j < len(src) && (src[j] == '+' || src[j] == '-') {
				j++
			}
			if j < len(src) && isDigit(src[j]) {
				i = j
				for i < len(src) && isDigit(src[i]) {
					i++
				}
			}
		}
		return mk(dialect.KindNumber, i)

	case isOpChar(c):
		i := pos
		for i < len(src) && isOpChar(src[i]) {
			// operators never absorb a comment start
			if i+1 < len(src) && ((src[i] == '-' && src[i+1] == '-') || (src[i] == '/' && src[i+1] == '*')) {
				break
			}
			i++
		}
		if i == pos {
			i = pos + 1
		}
		return mk(dialect.KindOperator, i)

	default:
		return mk(dialect.KindOther, pos+1)
	}
}

// lexQuoted lexes from just after the opening quote to just after the
// closing quote. Doubled quotes escape; backslash escapes only when
// backslashEsc (E'…' strings).
func lexQuoted(src []byte, i int, q byte, backslashEsc bool) (int, error) {
	start := i
	for i < len(src) {
		switch {
		case backslashEsc && src[i] == '\\':
			i += 2
		case src[i] == q:
			if i+1 < len(src) && src[i+1] == q { // doubled quote
				i += 2
				continue
			}
			return i + 1, nil
		default:
			i++
		}
	}
	return 0, &dialect.LexError{Pos: start - 1, Msg: "unterminated quoted token"}
}

// lexDollarQuote reports whether src[pos:] starts a dollar-quoted
// string ($$ or $tag$). Returns (end, true) on a dollar quote; end is
// -1 if unterminated. Returns (0, false) if not a dollar quote.
func lexDollarQuote(src []byte, pos int) (int, bool) {
	i := pos + 1
	for i < len(src) && (src[i] == '_' || isDigit(src[i]) ||
		(src[i] >= 'a' && src[i] <= 'z') || (src[i] >= 'A' && src[i] <= 'Z')) {
		// tag may not start with a digit per PG, but for boundary
		// purposes accepting it is harmless: $1$ is not matched below
		// because we require the closing '$' here AND digits alone are
		// handled by the caller before… so exclude digit-leading tags:
		if i == pos+1 && isDigit(src[i]) {
			return 0, false
		}
		i++
	}
	if i >= len(src) || src[i] != '$' {
		return 0, false
	}
	tag := string(src[pos : i+1]) // "$tag$" or "$$"
	body := i + 1
	for j := body; j+len(tag) <= len(src); j++ {
		if string(src[j:j+len(tag)]) == tag {
			return j + len(tag), true
		}
	}
	return -1, true
}
