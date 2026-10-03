package nullability

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/template"
)

// Bare (unqualified) column names reach the analyzer through two
// paths — a skeleton `col IS NOT NULL` conjunct and a strict
// aggregate's argument — and both resolve through
// instanceResolver.resolve's unique-candidate rule: a bare name narrows
// only when EXACTLY ONE top-level catalog relation has that column.
// Zero candidates (the name belongs to a derived table, or to nothing
// the catalog knows) and several candidates (an ambiguity the engine
// would reject, but the analyzer must never break by guessing) both
// keep the column nullable.

// analyzeWithCat is analyze over a caller-supplied catalog.
func analyzeWithCat(t *testing.T, c *cache.Catalog, src string, desc dialect.Desc) []bool {
	t.Helper()
	f, diags := template.NewScanner(postgres.Profile{}).ScanFile("t.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	rs, err := ast.Renderings(postgres.Profile{}, f.Queries[0])
	if err != nil {
		t.Fatal(err)
	}
	tree, err := postgres.Frontend{}.Parse(rs[0].SQL)
	if err != nil {
		t.Fatal(err)
	}
	return Analyze(tree, rs[0], desc, c, nil)
}

func TestAnalyze_UnqualifiedIsNotNullNarrows(t *testing.T) {
	src := `-- name: Q :many
SELECT org_id FROM users
WHERE org_id IS NOT NULL;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("org_id", 100, 3),
	}}, nil)
	assertNullable(t, got, []bool{false})
}

// The unique-candidate rule looks across every top-level relation: a
// bare name that only the null-extended side has still resolves, and
// the WHERE conjunct (evaluated after the join) narrows it.
func TestAnalyze_UnqualifiedIsNotNullAcrossOuterJoin(t *testing.T) {
	src := `-- name: Q :many
SELECT o.name FROM users AS u
LEFT JOIN orgs AS o ON o.id = u.org_id
WHERE name IS NOT NULL;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("name", 200, 2),
	}}, nil)
	assertNullable(t, got, []bool{false})
}

// Must-stay-nullable: a bare name two top-level relations both carry
// is ambiguous. PostgreSQL rejects the statement, but the analyzer
// runs on the rendering and must not pick a candidate on its own.
func TestAnalyze_UnqualifiedAmbiguousNeverNarrows(t *testing.T) {
	c := &cache.Catalog{Tables: []cache.Table{
		{Schema: "public", Name: "a", OID: 100, Cols: []cache.Column{
			{Name: "id", Att: 1, NotNull: true},
			{Name: "note", Att: 2},
		}},
		{Schema: "public", Name: "b", OID: 200, Cols: []cache.Column{
			{Name: "id", Att: 1, NotNull: true},
			{Name: "note", Att: 2},
		}},
	}}
	src := `-- name: Q :many
SELECT a.note, b.note FROM a JOIN b ON b.id = a.id
WHERE note IS NOT NULL;
`
	got := analyzeWithCat(t, c, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("note", 100, 2), col("note", 200, 2),
	}})
	assertNullable(t, got, []bool{true, true})
}

// Must-stay-nullable: a bare name exposed only by a derived table has
// no top-level catalog candidate, so the filter credits nothing — the
// derived body's own provenance is not re-derived from a bare name.
func TestAnalyze_UnqualifiedDerivedColumnNeverNarrows(t *testing.T) {
	src := `-- name: Q :many
SELECT s.org_id FROM (SELECT u.org_id FROM users AS u) AS s
WHERE org_id IS NOT NULL;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("org_id", 100, 3),
	}}, nil)
	assertNullable(t, got, []bool{true})
}

// Strict aggregates over a bare argument use the same rule: a unique
// non-null candidate narrows under GROUP BY, a nullable one does not,
// and an ambiguous one never does.
func TestAnalyze_UnqualifiedStrictAggregate(t *testing.T) {
	src := `-- name: Q :many
SELECT sum(id) AS s, max(org_id) AS m FROM users
GROUP BY email;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		{Name: "s"}, {Name: "m"},
	}}, nil)
	assertNullable(t, got, []bool{false, true})

	c := &cache.Catalog{Tables: []cache.Table{
		{Schema: "public", Name: "a", OID: 100, Cols: []cache.Column{
			{Name: "id", Att: 1, NotNull: true},
			{Name: "k", Att: 2, NotNull: true},
		}},
		{Schema: "public", Name: "b", OID: 200, Cols: []cache.Column{
			{Name: "id", Att: 1, NotNull: true},
		}},
	}}
	amb := `-- name: Q :many
SELECT sum(id) AS s FROM b RIGHT JOIN a ON b.id = a.k
GROUP BY a.k;
`
	got = analyzeWithCat(t, c, amb, dialect.Desc{Columns: []dialect.ColumnDesc{{Name: "s"}}})
	// `id` is NOT NULL in both catalog tables, but b's instance is
	// null-extended; guessing a would narrow a column b can supply.
	// (b is listed first so that a last-match guess picks a — the
	// order under which a broken uniqueness check would narrow.)
	assertNullable(t, got, []bool{true})
}
