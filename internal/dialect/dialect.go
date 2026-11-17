// Package dialect defines the driver interfaces separating the
// dialect-agnostic core from database specifics. v0.1 ships only the
// LexerProfile half; Frontend/Oracle land in P2/P4.
package dialect

import (
	"fmt"
)

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

// CaseInsensitiveIdents is implemented by lexer profiles whose dialect
// resolves identifier references case-insensitively: an alias written
// `A` is matched by a reference `a`, and a column `Kind` by `kind`
// (MySQL, SQLite). PostgreSQL case-folds unquoted identifiers at parse
// time, so its facade already yields folded names — it leaves this
// unimplemented and folding is then the identity.
type CaseInsensitiveIdents interface {
	CaseInsensitiveIdents() bool
}

// IdentFolds is implemented by case-insensitive profiles to state how
// their ENGINE compares identifiers. Each method maps a name to a
// comparison key; two names compare equal exactly when their keys do.
// Relation names (tables, aliases) get two keys because their case
// sensitivity can depend on server configuration that sqletch cannot see
// offline (MySQL's lower_case_table_names): a "may" key under which two
// names are equal whenever ANY configuration could treat them as the
// same relation, and a "must" key under which they are equal only when
// EVERY configuration does. Guard checks resolve through "may" (a
// reference is checked against every relation it could mean — fail
// closed); decisions that SKIP a check (subquery shadowing, parameter
// nullability) use "must".
type IdentFolds interface {
	FoldColumn(name string) string
	FoldRelationMay(name string) string
	FoldRelationMust(name string) string
}

func identity(s string) string { return s }

// ColumnFold returns the profile's column-name comparison key (column
// names, output aliases). Identity where the profile does not implement
// IdentFolds: PostgreSQL's facade already yields folded names.
func ColumnFold(profile LexerProfile) func(string) string {
	if f, ok := profile.(IdentFolds); ok {
		return f.FoldColumn
	}
	return identity
}

// RelationFolds returns the profile's "may" and "must" relation-name
// comparison keys (see IdentFolds). Identity where the profile does not
// implement IdentFolds.
func RelationFolds(profile LexerProfile) (may, must func(string) string) {
	if f, ok := profile.(IdentFolds); ok {
		return f.FoldRelationMay, f.FoldRelationMust
	}
	return identity, identity
}

// ASCIILower lower-cases ASCII letters and leaves every other byte —
// non-ASCII runes and invalid UTF-8 alike — untouched (SQLite's
// sqlite3StrICmp, and MySQL's handling of the ASCII plane).
func ASCIILower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// PipesOr is implemented by lexer profiles whose dialect parses the `||`
// operator as logical OR — binding LOOSER than AND — under its default
// configuration (MySQL: unless sql_mode has PIPES_AS_CONCAT). Clause
// scanners that decide whether an appended conjunct needs parentheses
// treat such an operator exactly like OR. On PostgreSQL and SQLite `||`
// is string concatenation (tighter than AND), so they leave it
// unimplemented. Over-reporting is safe (an extra wrap); under-reporting
// is a tenant leak in policy weaving.
type PipesOr interface {
	PipesOr() bool
}

// PipesAreOr reports whether profile's dialect parses `||` as logical OR.
func PipesAreOr(profile LexerProfile) bool {
	p, ok := profile.(PipesOr)
	return ok && p.PipesOr()
}

// LexError is returned for unterminated strings/comments; the scanner
// converts it into a diagnostic at Pos.
type LexError struct {
	Pos int
	Msg string
}

func (e *LexError) Error() string { return fmt.Sprintf("lex error at %d: %s", e.Pos, e.Msg) }
