package sqlite

import (
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	rsql "github.com/rqlite/sql"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// rqlite/sql's scanner accepts only [A-Za-z0-9_] in an unquoted
// identifier: it ends one at `$` (lexing the rest as a `$name` bind)
// and reads any non-ASCII byte as ILLEGAL, so it refuses
// `SELECT a$b FROM café`. SQLite's IdChar admits `$` after the first
// character and EVERY byte >= 0x80 (accented letters, CJK, U+3000, even
// invalid UTF-8), and the engine runs those statements. The frontend
// bridges the gap without touching rqlite:
//
//  1. rewriteIdents finds every unquoted identifier the PROFILE lexes
//     (the profile matches the engine) that contains `$` or a non-ASCII
//     rune, and replaces each such rune with ONE `_`. The byte length
//     may change, but the RUNE count does not, and rqlite reports rune
//     offsets — so every position maps, through the ORIGINAL's rune
//     table (runeToByteTable), to the original bytes. Runes are decoded
//     exactly as rqlite's bufio.Reader.ReadRune and runeToByteTable's
//     range loop do (an invalid byte is one RuneError rune). A word
//     whose rewrite is an rqlite keyword (`current$date` ->
//     CURRENT_DATE, a value function, not a column) is refused: the
//     rewrite would change the statement's meaning.
//  2. restoreIdents walks the WHOLE parsed AST by reflection (no
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

// rewrittenWord is one rewritten identifier: its byte span in the
// original SQL and its length in runes (= its byte length in the
// rewritten, all-ASCII form).
type rewrittenWord struct{ start, end, runes int }

// rewriteIdents returns sqlText with `$` and every non-ASCII rune
// replaced by `_` inside unquoted identifiers, plus the rewritten words.
// words is empty when nothing was rewritten (or the profile cannot lex
// the text — the parse then reports the error on the original).
func rewriteIdents(sqlText string) (string, []rewrittenWord, error) {
	src := []byte(sqlText)
	var words []rewrittenWord
	var out strings.Builder
	copied := 0 // bytes of sqlText already written to out
	pos := 0
	for {
		tok, err := Profile{}.NextToken(src, pos)
		if err != nil || tok.Kind == dialect.KindEOF {
			break
		}
		pos = tok.End
		if tok.Kind != dialect.KindIdent || !needsRewrite(tok.Text) {
			continue
		}
		var word strings.Builder
		runes := 0
		for i := 0; i < len(tok.Text); runes++ {
			r, size := utf8.DecodeRuneInString(tok.Text[i:])
			if r == '$' || size > 1 || r >= utf8.RuneSelf {
				word.WriteByte('_')
			} else {
				word.WriteByte(tok.Text[i])
			}
			i += size
		}
		w := word.String()
		if rsql.Lookup(w) != rsql.IDENT {
			return "", nil, &dialect.ParseError{Pos: tok.Start, Msg: fmt.Sprintf(
				"identifier %q contains `$` or non-ASCII characters: the SQLite frontend cannot parse those in an unquoted identifier, and its workaround would read this one as the keyword %s; quote it (\"%s\")",
				tok.Text, w, tok.Text)}
		}
		out.WriteString(sqlText[copied:tok.Start])
		out.WriteString(w)
		copied = tok.End
		words = append(words, rewrittenWord{start: tok.Start, end: tok.End, runes: runes})
	}
	if len(words) == 0 {
		return sqlText, nil, nil
	}
	out.WriteString(sqlText[copied:])
	return out.String(), words, nil
}

// needsRewrite reports whether an identifier holds a byte rqlite's
// scanner cannot take: `$` or anything >= 0x80.
func needsRewrite(ident string) bool {
	for i := 0; i < len(ident); i++ {
		if ident[i] == '$' || ident[i] >= utf8.RuneSelf {
			return true
		}
	}
	return false
}

var identPtrType = reflect.TypeFor[*rsql.Ident]()

// restoreIdents resets every unquoted identifier spanning a rewritten
// word to the original text, and refuses when a word is not covered by
// at least one such identifier.
func restoreIdents(sqlText string, stmts []rsql.Statement, r2b []int, words []rewrittenWord) error {
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
				// id.Name is the rewritten, all-ASCII text: its byte
				// length is the word's rune count.
				if i, ok := byStart[start]; ok && len(id.Name) == words[i].runes {
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
				"identifier %q contains `$` or non-ASCII characters, which the SQLite frontend cannot parse in this position; quote it (\"%s\")",
				sqlText[w.start:w.end], sqlText[w.start:w.end])}
		}
	}
	return nil
}
