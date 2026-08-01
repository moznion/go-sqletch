// Package mysql implements the MySQL dialect driver (Tier 2). This
// file is the lexer profile: token boundaries only, no SQL
// understanding. Lexing assumes the default sql_mode — double quotes
// delimit strings (not ANSI_QUOTES) and backslash escapes are active
// (not NO_BACKSLASH_ESCAPES).
package mysql

import (
	"github.com/moznion/sqletch/internal/dialect"
)

type Profile struct{}

var (
	_ dialect.LexerProfile = Profile{}
	_ dialect.Placeholders = Profile{}
	_ dialect.InEmpty      = Profile{}
)

// PlaceholderStyle declares MySQL's '?' per-occurrence binding.
func (Profile) PlaceholderStyle() dialect.PlaceholderStyle { return dialect.PlaceholderQuestion }

// InEmptySQL is the arity-0 @in emission (FALSE even for NULL
// operands; MySQL needs FROM DUAL to attach a WHERE).
func (Profile) InEmptySQL() string { return "IN (SELECT NULL FROM DUAL WHERE FALSE)" }

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// MySQL unquoted identifiers allow '$' anywhere (and leading digits,
// which the number branch claims first — a boundary-level compromise).
func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}
func isIdentCont(c byte) bool { return isIdentStart(c) || isDigit(c) }

// MySQL operator characters, minus paren/comma handled separately.
// '@' is included: template constructs are matched by the scanner
// *before* this profile ever sees the '@'; user/system variables lex
// as operator + ident, which is fine for boundary purposes.
func isOpChar(c byte) bool {
	switch c {
	case '+', '-', '*', '/', '<', '>', '=', '~', '!', '@', '%', '^', '&', '|':
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

	case c == '#':
		i := pos + 1
		for i < len(src) && src[i] != '\n' {
			i++
		}
		return mk(dialect.KindLineComment, i)

	// MySQL requires whitespace (or end of line/input) after `--` for
	// it to start a comment; `1--2` is double negation.
	case c == '-' && pos+1 < len(src) && src[pos+1] == '-' &&
		(pos+2 >= len(src) || isSpace(src[pos+2])):
		i := pos + 2
		for i < len(src) && src[i] != '\n' {
			i++
		}
		return mk(dialect.KindLineComment, i)

	case c == '/' && pos+1 < len(src) && src[pos+1] == '*':
		// MySQL block comments do NOT nest: the first */ closes.
		i := pos + 2
		for i+1 < len(src) {
			if src[i] == '*' && src[i+1] == '/' {
				return mk(dialect.KindBlockComment, i+2)
			}
			i++
		}
		return dialect.Token{}, &dialect.LexError{Pos: pos, Msg: "unterminated block comment"}

	case c == '\'' || c == '"':
		end, err := lexQuoted(src, pos+1, c)
		if err != nil {
			return dialect.Token{}, err
		}
		return mk(dialect.KindString, end)

	case c == '`':
		// Backtick identifier: doubled backticks escape; backslash is
		// literal inside backticks.
		i := pos + 1
		for i < len(src) {
			if src[i] == '`' {
				if i+1 < len(src) && src[i+1] == '`' {
					i += 2
					continue
				}
				return mk(dialect.KindQuotedIdent, i+1)
			}
			i++
		}
		return dialect.Token{}, &dialect.LexError{Pos: pos, Msg: "unterminated quoted identifier"}

	case c == '?':
		return mk(dialect.KindPositionalParam, pos+1)

	case c == ':':
		if pos+1 < len(src) && src[pos+1] == '=' {
			return mk(dialect.KindOperator, pos+2)
		}
		if pos+1 < len(src) && isIdentStart(src[pos+1]) && src[pos+1] != '$' {
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

	case isIdentStart(c):
		i := pos
		for i < len(src) && isIdentCont(src[i]) {
			i++
		}
		return mk(dialect.KindIdent, i)

	case isOpChar(c):
		i := pos
		for i < len(src) && isOpChar(src[i]) {
			// operators never absorb a comment start
			if i+1 < len(src) && src[i] == '/' && src[i+1] == '*' {
				break
			}
			if i+2 < len(src) && src[i] == '-' && src[i+1] == '-' && isSpace(src[i+2]) {
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

// lexQuoted lexes a MySQL string from just after the opening quote to
// just after the closing quote. Both doubled quotes and backslash
// escapes are active (default sql_mode).
func lexQuoted(src []byte, i int, q byte) (int, error) {
	start := i
	for i < len(src) {
		switch src[i] {
		case '\\':
			i += 2
		case q:
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
