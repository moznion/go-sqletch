// Package sqlite implements the SQLite dialect driver (Tier 2). This
// file is the lexer profile: token boundaries only, no SQL
// understanding.
package sqlite

import (
	"github.com/moznion/go-sqletch/internal/dialect"
)

type Profile struct{}

var (
	_ dialect.LexerProfile          = Profile{}
	_ dialect.Placeholders          = Profile{}
	_ dialect.InEmpty               = Profile{}
	_ dialect.CaseInsensitiveIdents = Profile{}
	_ dialect.ASCIIFoldIdents       = Profile{}
)

// PlaceholderStyle declares SQLite's '?' per-occurrence binding.
func (Profile) PlaceholderStyle() dialect.PlaceholderStyle { return dialect.PlaceholderQuestion }

// CaseInsensitiveIdents reports that SQLite resolves aliases and column
// references case-insensitively, so R3/R2 name matching must fold.
func (Profile) CaseInsensitiveIdents() bool { return true }

// ASCIIFoldIdents reports that SQLite folds identifier case for ASCII
// letters only (sqlite3StrICmp): `É` and `é` are different names.
func (Profile) ASCIIFoldIdents() bool { return true }

// InEmptySQL is the arity-0 @in emission (FALSE even for NULL
// operands; SQLite allows a FROM-less SELECT with WHERE).
func (Profile) InEmptySQL() string { return "IN (SELECT NULL WHERE 0)" }

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isHexDigit(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

// isIdentCont is SQLite's IdChar: alphanumerics, `_`, bytes >= 0x80,
// and `$` (tokenize.c; the engine runs `SELECT a$b FROM t`). A leading
// `$` is not an identifier start: it opens an engine variable.
func isIdentCont(c byte) bool { return isIdentStart(c) || isDigit(c) || c == '$' }

// SQLite operator characters, minus paren/comma handled separately.
// '@' is included: template constructs are matched by the scanner
// *before* this profile ever sees the '@'; native @name parameters lex
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

	// SQLite line comments need no whitespace after the dashes, and end
	// ONLY at '\n' — SQLite's tokenizer (`c!='\n'`) and rqlite's scanner
	// (`case -1, '\n'`) alike. A lone '\r' does NOT end one (security
	// audit F12): ending it there let sqletch weave a predicate into
	// what the engine reads as comment text. The scanner rejects lone
	// CRs anyway (SQLETCH024); the profile still has to agree with the
	// engine.
	case c == '-' && pos+1 < len(src) && src[pos+1] == '-':
		i := pos + 2
		for i < len(src) && src[i] != '\n' {
			i++
		}
		return mk(dialect.KindLineComment, i)

	case c == '/' && pos+1 < len(src) && src[pos+1] == '*':
		// SQLite block comments do NOT nest: the first */ closes.
		i := pos + 2
		for i+1 < len(src) {
			if src[i] == '*' && src[i+1] == '/' {
				return mk(dialect.KindBlockComment, i+2)
			}
			i++
		}
		return dialect.Token{}, &dialect.LexError{Pos: pos, Msg: "unterminated block comment"}

	// Blob literal x'...' / X'...'.
	case (c == 'x' || c == 'X') && pos+1 < len(src) && src[pos+1] == '\'':
		i := pos + 2
		for i < len(src) && isHexDigit(src[i]) {
			i++
		}
		if i >= len(src) || src[i] != '\'' {
			return dialect.Token{}, &dialect.LexError{Pos: pos, Msg: "unterminated blob literal"}
		}
		return mk(dialect.KindString, i+1)

	case c == '\'':
		// Doubled quotes escape; backslash is literal in SQLite.
		end, err := lexQuoted(src, pos+1, '\'')
		if err != nil {
			return dialect.Token{}, err
		}
		return mk(dialect.KindString, end)

	case c == '"':
		end, err := lexQuoted(src, pos+1, '"')
		if err != nil {
			return dialect.Token{}, err
		}
		return mk(dialect.KindQuotedIdent, end)

	case c == '`':
		end, err := lexQuoted(src, pos+1, '`')
		if err != nil {
			return dialect.Token{}, err
		}
		return mk(dialect.KindQuotedIdent, end)

	case c == '[':
		// Bracket identifier: terminated by ']', no escaping.
		i := pos + 1
		for i < len(src) {
			if src[i] == ']' {
				return mk(dialect.KindQuotedIdent, i+1)
			}
			i++
		}
		return dialect.Token{}, &dialect.LexError{Pos: pos, Msg: "unterminated bracket identifier"}

	case c == '?':
		i := pos + 1
		for i < len(src) && isDigit(src[i]) {
			i++
		}
		return mk(dialect.KindPositionalParam, i)

	case c == ':':
		if pos+1 < len(src) && isIdentStart(src[pos+1]) {
			// A sqletch `:name` ends before `$` on every dialect: the
			// template parameter grammar is sqletch's, not the engine's.
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
		return mk(dialect.KindNumber, lexNumber(src, pos))

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

// lexNumber returns the end of the numeric literal starting at pos,
// following SQLite's tokenizer (tokenize.c CC_DIGIT/CC_DOT, 3.46+
// digit separators): a hex literal is 0x/0X plus a hex digit run, a
// decimal is a digit run, at most ONE `.` and its digit run, then an
// exponent only when [Ee][+-]? is followed by a digit; `_` is accepted
// only between two digits, the one place the engine accepts it. The
// old loop absorbed every `.` (`1.2.3`), which SQLite reads as `1.2`
// then `.3`. Trailing identifier characters (`1x`) make the engine's
// token illegal, so that form never runs and is not modeled.
func lexNumber(src []byte, pos int) int {
	if src[pos] == '0' && pos+2 < len(src) && (src[pos+1] == 'x' || src[pos+1] == 'X') && isHexDigit(src[pos+2]) {
		return lexDigits(src, pos+2, isHexDigit)
	}
	i := pos
	if src[i] != '.' {
		i = lexDigits(src, i, isDigit)
	}
	if i < len(src) && src[i] == '.' {
		i++
		if i < len(src) && isDigit(src[i]) {
			i = lexDigits(src, i, isDigit)
		}
	}
	if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
		j := i + 1
		if j < len(src) && (src[j] == '+' || src[j] == '-') {
			j++
		}
		if j < len(src) && isDigit(src[j]) {
			i = lexDigits(src, j, isDigit)
		}
	}
	return i
}

// lexDigits returns the end of the digit run starting at i (which must
// be a digit), with single `_` separators between digits.
func lexDigits(src []byte, i int, digit func(byte) bool) int {
	for i < len(src) {
		switch {
		case digit(src[i]):
			i++
		case src[i] == '_' && i+1 < len(src) && digit(src[i+1]):
			i += 2
		default:
			return i
		}
	}
	return i
}

// lexQuoted lexes from just after the opening quote to just after the
// closing quote. Doubled quotes escape; backslash is literal.
func lexQuoted(src []byte, i int, q byte) (int, error) {
	start := i
	for i < len(src) {
		if src[i] == q {
			if i+1 < len(src) && src[i+1] == q { // doubled quote
				i += 2
				continue
			}
			return i + 1, nil
		}
		i++
	}
	return 0, &dialect.LexError{Pos: start - 1, Msg: "unterminated quoted token"}
}
