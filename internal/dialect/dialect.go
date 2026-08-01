// Package dialect defines the driver interfaces separating the
// dialect-agnostic core from database specifics. v0.1 ships only the
// LexerProfile half; Frontend/Oracle land in P2/P4.
package dialect

import "fmt"

type TokenKind int

const (
	KindEOF TokenKind = iota
	KindWhitespace
	KindLineComment
	KindBlockComment
	KindString      // includes dollar-quoted and E'' strings
	KindQuotedIdent // "ident"
	KindIdent
	KindNumber
	KindParamRef        // :name (Text includes the colon)
	KindPositionalParam // $1, $2, …
	KindCast            // ::
	KindOperator
	KindLParen
	KindRParen
	KindComma
	KindSemicolon
	KindOther // ., [, ], and anything else structurally irrelevant
)

type Token struct {
	Kind  TokenKind
	Start int // byte offset into src, inclusive
	End   int // exclusive
	Text  string
}

// LexerProfile lets the shared template scanner walk dialect SQL
// without parsing it. Implementations only need correct token
// *boundaries* (strings, comments, params, operators), not SQL
// understanding.
type LexerProfile interface {
	// NextToken lexes the token starting at src[pos:]. pos is
	// guaranteed to be a token boundary. At end of input it returns
	// a token with Kind == KindEOF.
	NextToken(src []byte, pos int) (Token, error)
}

// PlaceholderStyle is how a dialect binds parameters in prepared SQL.
type PlaceholderStyle int

const (
	// PlaceholderDollar: $1, $2, … numbered in first-occurrence order;
	// repeated references to one bind source reuse the number
	// (PostgreSQL).
	PlaceholderDollar PlaceholderStyle = iota
	// PlaceholderQuestion: '?', one placeholder per occurrence;
	// repeated references repeat the bind (MySQL, SQLite).
	PlaceholderQuestion
)

// Placeholders is implemented by lexer profiles to declare their
// dialect's bind-placeholder style. Profiles that do not implement it
// default to PlaceholderDollar.
type Placeholders interface {
	PlaceholderStyle() PlaceholderStyle
}

// StyleOf resolves a profile's placeholder style.
func StyleOf(profile LexerProfile) PlaceholderStyle {
	if p, ok := profile.(Placeholders); ok {
		return p.PlaceholderStyle()
	}
	return PlaceholderDollar
}

// InEmpty is implemented by expanding-dialect profiles to provide the
// arity-0 @in emission: a fragment completing `expr <here>` so that an
// empty list matches nothing — FALSE even for a NULL operand,
// matching PostgreSQL's `= ANY('{}')`.
type InEmpty interface {
	InEmptySQL() string
}

// InEmptyOf resolves a profile's arity-0 @in emission.
func InEmptyOf(profile LexerProfile) string {
	if p, ok := profile.(InEmpty); ok {
		return p.InEmptySQL()
	}
	// The MySQL form, kept as the default for compatibility.
	return "IN (SELECT NULL FROM DUAL WHERE FALSE)"
}

// LexError is returned for unterminated strings/comments; the scanner
// converts it into a diagnostic at Pos.
type LexError struct {
	Pos int
	Msg string
}

func (e *LexError) Error() string { return fmt.Sprintf("lex error at %d: %s", e.Pos, e.Msg) }
