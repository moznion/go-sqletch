package sqlite

import (
	"strings"
	"testing"

	rsql "github.com/rqlite/sql"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// rqlite/sql's scanner stops an unquoted identifier at `$`, while the
// engine (and the profile) continue through it. The frontend rewrites
// such identifiers for the parse and restores every name from the
// original bytes, so the facades report the engine's names.
func TestParse_DollarIdentifiers(t *testing.T) {
	tr := parse(t, "SELECT x$.a$b, c AS out$ FROM t$1 AS x$ JOIN u ON u.id = x$.a$b WHERE x$.a$b = ? AND a_b = 1")
	rels := tr.Relations()
	if len(rels) != 2 || rels[0].Table != "t$1" || rels[0].Alias != "x$" || rels[0].Loc != strings.Index("SELECT x$.a$b, c AS out$ FROM t$1", "t$1") {
		t.Fatalf("relations = %+v", rels)
	}
	var refs []string
	for _, c := range tr.ColumnRefs() {
		refs = append(refs, strings.Join(c.Fields, "."))
	}
	got := strings.Join(refs, ",")
	for _, want := range []string{"x$.a$b", "u.id", "a_b", "c"} {
		if !strings.Contains(","+got+",", ","+want+",") {
			t.Errorf("column refs %q lack %q", got, want)
		}
	}
	if strings.Contains(got, "x_") || strings.Contains(got, "a_b,a_b") {
		t.Errorf("a rewritten name leaked: %q", got)
	}
	items := tr.TargetItems()
	if len(items) != 2 || items[1].Name != "out$" {
		t.Errorf("target items = %+v", items)
	}
	tables := tr.DeepTables()
	if len(tables) != 2 || tables[0].Name != "t$1" {
		t.Errorf("deep tables = %+v", tables)
	}
}

// `a_b` and `a$b` are distinct names to the engine; the rewrite must
// never merge them.
func TestParse_DollarIdentifierDoesNotCollide(t *testing.T) {
	tr := parse(t, "SELECT a_b, a$b FROM t")
	var refs []string
	for _, c := range tr.ColumnRefs() {
		refs = append(refs, strings.Join(c.Fields, "."))
	}
	if strings.Join(refs, ",") != "a_b,a$b" {
		t.Errorf("column refs = %v", refs)
	}
}

// A rewrite that turns the word into an rqlite keyword would change the
// statement's meaning (`current$date` is a column to the engine,
// CURRENT_DATE is a value function): refused, never parsed.
func TestParse_DollarIdentifierBecomingKeywordRefused(t *testing.T) {
	for _, src := range []string{"SELECT current$date FROM t", "SELECT a FROM t WHERE current$timestamp = 1"} {
		_, err := Frontend{}.Parse(src)
		pe, ok := err.(*dialect.ParseError)
		if !ok {
			t.Fatalf("%q: err = %v, want a ParseError", src, err)
		}
		if want := strings.Index(src, "current$"); pe.Pos != want || !strings.Contains(pe.Msg, "`$`") {
			t.Errorf("%q: got (%d, %q), want Pos %d and a `$` message", src, pe.Pos, pe.Msg, want)
		}
	}
}

// Every rewritten word must come back as exactly one identifier node;
// one that reaches the AST any other way (or not at all) is refused, so
// a name the facades would report in rewritten form can never escape.
func TestRestoreIdents_UncoveredWordRefused(t *testing.T) {
	src := "SELECT a$b FROM t"
	stmts, err := rsql.NewParser(strings.NewReader("SELECT a_b FROM t")).ParseStatements()
	if err != nil {
		t.Fatal(err)
	}
	r2b := runeToByteTable(src)
	words := []rewrittenWord{{start: 7, end: 10, runes: 3}, {start: 0, end: 6, runes: 6}} // SELECT: no ident node covers it
	if err := restoreIdents(src, stmts, r2b, words); err == nil {
		t.Fatal("an uncovered rewritten word must be refused")
	}
	stmts, _ = rsql.NewParser(strings.NewReader("SELECT a_b FROM t")).ParseStatements()
	if err := restoreIdents(src, stmts, r2b, words[:1]); err != nil {
		t.Fatalf("covered word: %v", err)
	}
	if name := stmts[0].(*rsql.SelectStatement).Columns[0].Expr.(*rsql.Ident).Name; name != "a$b" {
		t.Errorf("restored name = %q", name)
	}
}

// Quoted identifiers, strings, comments and engine variables are not
// identifiers to the profile, so their `$` is never rewritten.
func TestParse_DollarOutsideIdentifiersUntouched(t *testing.T) {
	tr := parse(t, `SELECT "q$x", 'a$b' /* c$d */ FROM t WHERE e = $v`)
	for _, c := range tr.ColumnRefs() {
		if strings.Join(c.Fields, ".") == "q_x" {
			t.Errorf("quoted identifier was rewritten: %+v", c)
		}
	}
}

// rqlite/sql's scanner rejects every non-ASCII byte in an unquoted
// identifier; the engine (and the profile) treat any byte >= 0x80 as an
// identifier character — accented letters, CJK, U+3000, even invalid
// UTF-8. Each non-ASCII rune is rewritten to ONE `_`: the byte length
// changes but the rune count does not, and rqlite reports rune offsets,
// so every position still maps (through the original's rune table) to
// the original bytes.
func TestParse_NonASCIIIdentifiers(t *testing.T) {
	src := "SELECT é.名前, é.a　b AS 日本 FROM café AS é JOIN u$ ON u$.id = é.id WHERE é.名前 = ? AND n\xffm = 1"
	tr := parse(t, src)
	rels := tr.Relations()
	if len(rels) != 2 || rels[0].Table != "café" || rels[0].Alias != "é" || rels[0].Loc != strings.Index(src, "café") ||
		rels[1].Table != "u$" || rels[1].Loc != strings.Index(src, "u$ ON") {
		t.Fatalf("relations = %+v", rels)
	}
	var refs []string
	for _, c := range tr.ColumnRefs() {
		refs = append(refs, strings.Join(c.Fields, "."))
	}
	got := "," + strings.Join(refs, ",") + ","
	for _, want := range []string{"é.名前", "é.a　b", "u$.id", "é.id", "n\xffm"} {
		if !strings.Contains(got, ","+want+",") {
			t.Errorf("column refs %q lack %q", got, want)
		}
	}
	if strings.Contains(got, "_") {
		t.Errorf("a rewritten name leaked: %q", got)
	}
	if items := tr.TargetItems(); len(items) != 2 || items[1].Name != "日本" {
		t.Errorf("target items = %+v", items)
	}
	for _, c := range tr.ColumnRefs() {
		if strings.Join(c.Fields, ".") == "n\xffm" && c.Loc != strings.Index(src, "n\xffm") {
			t.Errorf("Loc after multibyte runes = %d, want %d", c.Loc, strings.Index(src, "n\xffm"))
		}
	}
}

// A non-ASCII rewrite that forms a keyword is refused like a `$` one.
func TestParse_NonASCIIIdentifierBecomingKeywordRefused(t *testing.T) {
	src := "SELECT currentédate FROM t"
	_, err := Frontend{}.Parse(src)
	pe, ok := err.(*dialect.ParseError)
	if !ok || pe.Pos != strings.Index(src, "current") {
		t.Fatalf("err = %v, want a ParseError at the identifier", err)
	}
}

// A parse error after a rewritten multibyte identifier still points at
// the original byte.
func TestParse_NonASCIIErrorPosition(t *testing.T) {
	src := "SELECT 日本 FROM t WHERE AND"
	_, err := Frontend{}.Parse(src)
	pe, ok := err.(*dialect.ParseError)
	if !ok || pe.Pos != strings.Index(src, "AND") {
		t.Fatalf("err = %v, want a ParseError at byte %d", err, strings.Index(src, "AND"))
	}
}
