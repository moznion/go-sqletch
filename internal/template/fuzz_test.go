package template

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
)

// fuzzProfiles are every lexer profile the scanner ships. Each input is
// run through all of them: the quoting rules are what differ (dollar
// quoting, backticks, bracket quoting) and they are exactly where a
// scanner mis-tracks state, so a per-dialect target would let two of the
// three go unexercised.
var fuzzProfiles = []struct {
	name    string
	profile dialect.LexerProfile
}{
	{"postgres", postgres.Profile{}},
	{"mysql", mysql.Profile{}},
	{"sqlite", sqlite.Profile{}},
}

// FuzzScan asserts the scanner never panics and upholds the span
// invariants of design 01 §9 on arbitrary input, for every dialect:
//
//   - per query, item raw spans are contiguous from the header end and
//     within file bounds;
//   - every diagnostic carries an in-bounds span naming the scanned
//     file, because the excerpt renderer and the LSP's UTF-16 position
//     conversion both index the source with it;
//   - rendering a diagnostic is itself panic-free (its caret geometry
//     does rune arithmetic over bytes that need not be valid UTF-8).
func FuzzScan(f *testing.F) {
	f.Add(useCase1)
	f.Add("-- name: A :one\nSELECT 1;")
	f.Add("-- name: B :many\nSELECT 1 FROM t WHERE TRUE\n@if-present(x)\n  AND t.x = :x\n@endif\n;")
	f.Add("@if-present(")
	f.Add("-- name: C :many\n@choose(s)@case(a)ORDER BY 1@end;")
	f.Add("SELECT 'unterminated")
	f.Add("-- name: D :many\nSELECT $tag$ @endif $tag$ @> :p;")

	// Dialect-specific quoting: a directive hidden inside each profile's
	// quote forms must stay inert, and an unterminated one must not run
	// the scanner off the end.
	f.Add("-- name: E :many\nSELECT `@endif` FROM `t` WHERE `x` = :x;")
	f.Add("-- name: F :many\nSELECT `unterminated")
	f.Add("-- name: G :many\nSELECT [@endif] FROM [t] WHERE [x] = :x;")
	f.Add("-- name: H :many\nSELECT [unterminated")
	f.Add("-- name: I :many\nSELECT \"@if-present(x)\" FROM t;")
	f.Add("-- name: J :many\nSELECT 'a''b' FROM t WHERE t.x = :x;")
	f.Add("-- name: K :many\nSELECT 1 /* @endif */ FROM t;")
	f.Add("-- name: L :many\nSELECT 1 /* unterminated")

	// Directive-shaped comments, the annotation surface added in v0.4.
	f.Add("-- name: M :many\n-- @param x: text\n-- @column n: bigint\nSELECT count(*) AS n FROM t WHERE t.x = :x;")
	f.Add("-- name: N :many\n-- @param :\nSELECT 1;")

	// Constructs whose bodies carry their own spans.
	f.Add("-- name: O :many\nSELECT 1 FROM t WHERE TRUE\n  AND @filter-tree!(s)\n@predicate(a)\nt.x = :s_x\n@end;")
	f.Add("-- name: P :many\nSELECT 1 FROM t WHERE t.x @in(:xs);")
	f.Add("-- name: Q :many\nSELECT 1 FROM t\n@order-by(s)\n@key(a)\nt.a\n@default\nORDER BY t.id\n@end;")
	f.Add("-- name: R :many\nSELECT 1 FROM t WHERE TRUE\n@when(f = false)\n  AND t.v\n@end;")

	f.Fuzz(func(t *testing.T, src string) {
		for _, p := range fuzzProfiles {
			file, diags := NewScanner(p.profile).ScanFile("fuzz.sql", []byte(src))

			for qi, q := range file.Queries {
				pos := q.HeaderSpan.End
				for ii, it := range q.Items {
					r := it.Raw()
					if r.Start != pos || r.End < r.Start || r.End > len(src) {
						t.Fatalf("%s: query %d item %d: span %+v violates contiguity from %d (len %d)",
							p.name, qi, ii, r, pos, len(src))
					}
					pos = r.End
				}
			}

			for di, d := range diags {
				s := d.Span
				if s.File != "fuzz.sql" {
					t.Fatalf("%s: diag %d (%s): span file = %q, want the scanned file",
						p.name, di, d.Code, s.File)
				}
				if s.Start < 0 || s.End < s.Start || s.End > len(src) {
					t.Fatalf("%s: diag %d (%s): span %+v out of bounds (len %d)",
						p.name, di, d.Code, s, len(src))
				}
				// Must not panic on arbitrary (possibly invalid UTF-8) bytes.
				_ = d.RenderExcerpt([]byte(src))
			}
		}
	})
}
