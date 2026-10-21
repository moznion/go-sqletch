package postgres

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// PostgreSQL's scanner (scan.l: `newline [\n\r]`, `comment ("--"{non_newline}*)`
// with `non_newline [^\n\r]`, shared by pg_query) ends a `--` comment at
// '\r' as well as '\n', so — unlike MySQL/SQLite (audit F11/F12) — the
// profile keeps ending it there. Lone CRs are rejected for every dialect
// by the scanner anyway (SQLETCH024); this pins that the PG profile
// agrees with its engine on its own.
func TestLexer_LineCommentEndsAtCRLikeServer(t *testing.T) {
	src := []byte("-- c\rSELECT")
	tok, err := Profile{}.NextToken(src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Kind != dialect.KindLineComment || tok.Text != "-- c" {
		t.Fatalf("got %v %q, want line comment %q", tok.Kind, tok.Text, "-- c")
	}
}
