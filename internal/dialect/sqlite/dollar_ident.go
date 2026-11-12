package sqlite

import (
	"fmt"
	"reflect"

	rsql "github.com/rqlite/sql"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// rqlite/sql's scanner ends an unquoted identifier at `$`
// (isUnquotedIdent is [A-Za-z0-9_]) and lexes the rest as a `$name`
// bind, so it refuses `SELECT a$b FROM t`. SQLite's IdChar admits `$`
// after the first character, and the engine runs it. The frontend
// bridges the gap without touching rqlite:
//
//  1. rewriteDollarIdents finds every unquoted identifier the PROFILE
//     lexes (the profile matches the engine) that contains `$`, and
//     replaces each `$` with `_`. Same byte and rune length, so every
//     offset rqlite reports is still an offset into the original SQL.
//     A word whose rewrite is an rqlite keyword (`current$date` ->
//     CURRENT_DATE, a value function, not a column) is refused: the
//     rewrite would change the statement's meaning.
//  2. restoreDollarIdents walks the WHOLE parsed AST by reflection (no
//     hand-written walker, so no node kind can be missed) and resets
//     the Name of every unquoted *rsql.Ident that spans exactly a
//     rewritten word to the original bytes. Every rewritten word must
//     be covered by such a node; one that is not (dropped by the
//     parser, or carried by some other node kind) is refused, so a
//     rewritten name — which could collide with a real `a_b` — never
//     reaches a facade.
//
// The engine itself always receives the original SQL; only the parse
// sees the rewrite.

// dollarWord is the byte span of one rewritten identifier.
type dollarWord struct{ start, end int }

// rewriteDollarIdents returns sqlText with `$` replaced by `_` inside
// unquoted identifiers, plus the rewritten spans. words is empty when
// nothing was rewritten (or the profile cannot lex the text — the
// parse then reports the error on the original).
func rewriteDollarIdents(sqlText string) (string, []dollarWord, error) {
	src := []byte(sqlText)
	var words []dollarWord
	var out []byte
	pos := 0
	for {
		tok, err := Profile{}.NextToken(src, pos)
		if err != nil || tok.Kind == dialect.KindEOF {
			break
		}
		pos = tok.End
		if tok.Kind != dialect.KindIdent {
			continue
		}
		dollar := false
		for i := tok.Start; i < tok.End; i++ {
			if src[i] == '$' {
				dollar = true
				break
			}
		}
		if !dollar {
			continue
		}
		if out == nil {
			out = append([]byte(nil), src...)
		}
		for i := tok.Start; i < tok.End; i++ {
			if out[i] == '$' {
				out[i] = '_'
			}
		}
		if rsql.Lookup(string(out[tok.Start:tok.End])) != rsql.IDENT {
			return "", nil, &dialect.ParseError{Pos: tok.Start, Msg: fmt.Sprintf(
				"identifier %q contains `$`: the SQLite frontend cannot parse `$` in an unquoted identifier, and its workaround would read this one as the keyword %s; quote it (\"%s\")",
				tok.Text, string(out[tok.Start:tok.End]), tok.Text)}
		}
		words = append(words, dollarWord{start: tok.Start, end: tok.End})
	}
	if len(words) == 0 {
		return sqlText, nil, nil
	}
	return string(out), words, nil
}

var identPtrType = reflect.TypeFor[*rsql.Ident]()

// restoreDollarIdents resets every unquoted identifier spanning a
// rewritten word to the original text, and refuses when a word is not
// covered by at least one such identifier.
func restoreDollarIdents(sqlText string, stmts []rsql.Statement, r2b []int, words []dollarWord) error {
	if len(words) == 0 {
		return nil
	}
	byStart := make(map[int]int, len(words)) // start byte -> index in words
	for i, w := range words {
		byStart[w.start] = i
	}
	covered := make([]bool, len(words))
	seen := map[uintptr]bool{}
	var visit func(v reflect.Value)
	visit = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Pointer:
			if v.IsNil() || seen[v.Pointer()] {
				return
			}
			seen[v.Pointer()] = true
			if v.Type() == identPtrType && v.CanInterface() {
				id := v.Interface().(*rsql.Ident)
				if id.Quoted {
					return
				}
				start := runeToByte(r2b, id.NamePos.Offset)
				if i, ok := byStart[start]; ok && start+len(id.Name) == words[i].end {
					id.Name = sqlText[words[i].start:words[i].end]
					covered[i] = true
				}
				return
			}
			visit(v.Elem())
		case reflect.Interface:
			if !v.IsNil() {
				visit(v.Elem())
			}
		case reflect.Struct:
			for i := range v.NumField() {
				visit(v.Field(i))
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				visit(v.Index(i))
			}
		}
	}
	for _, s := range stmts {
		visit(reflect.ValueOf(s))
	}
	for i, ok := range covered {
		if !ok {
			w := words[i]
			return &dialect.ParseError{Pos: w.start, Msg: fmt.Sprintf(
				"identifier %q contains `$`, which the SQLite frontend cannot parse in this position; quote it (\"%s\")",
				sqlText[w.start:w.end], sqlText[w.start:w.end])}
		}
	}
	return nil
}
