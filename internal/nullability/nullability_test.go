package nullability

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/template"
)

// Catalog fixture: users (id/email NOT NULL, org_id nullable),
// orgs (id/name NOT NULL).
func cat() *cache.Catalog {
	return &cache.Catalog{Tables: []cache.Table{
		{Schema: "public", Name: "users", OID: 100, Cols: []cache.Column{
			{Name: "id", Att: 1, NotNull: true},
			{Name: "email", Att: 2, NotNull: true},
			{Name: "org_id", Att: 3, NotNull: false},
		}},
		{Schema: "public", Name: "orgs", OID: 200, Cols: []cache.Column{
			{Name: "id", Att: 1, NotNull: true},
			{Name: "name", Att: 2, NotNull: true},
		}},
	}}
}

// analyze compiles a template through scan+render+parse and runs
// Analyze with a fabricated Desc.
func analyze(t *testing.T, src string, desc dialect.Desc, overrides map[string]bool) []bool {
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
	return Analyze(tree, rs[0], desc, cat(), overrides)
}

func col(name string, rel uint32, att int16) dialect.ColumnDesc {
	return dialect.ColumnDesc{Name: name, SrcRel: rel, SrcAtt: att}
}

func TestAnalyze_SkeletonBasics(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id, u.email, u.org_id FROM users AS u;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), col("email", 100, 2), col("org_id", 100, 3),
	}}, nil)
	want := []bool{false, false, true} // NOT NULL, NOT NULL, nullable
	assertNullable(t, got, want)
}

func TestAnalyze_SkeletonLeftJoinNullExtends(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id, o.name FROM users AS u LEFT JOIN orgs AS o ON o.id = u.org_id;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), col("name", 200, 2),
	}}, nil)
	// orgs.name is catalog-NOT NULL but null-extended by the LEFT JOIN.
	assertNullable(t, got, []bool{false, true})
}

func TestAnalyze_SkeletonRightJoinNullExtendsLeftSide(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id, o.name FROM users AS u RIGHT JOIN orgs AS o ON o.id = u.org_id;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), col("name", 200, 2),
	}}, nil)
	// RIGHT JOIN null-extends the LEFT operand (users).
	assertNullable(t, got, []bool{true, false})
}

func TestAnalyze_SkeletonInnerJoinKeepsNotNull(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id, o.name FROM users AS u JOIN orgs AS o ON o.id = u.org_id;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), col("name", 200, 2),
	}}, nil)
	assertNullable(t, got, []bool{false, false})
}

// Review counterexample F1a: a guarded IS NOT NULL predicate must not
// narrow — org_id stays nullable even though the maximal shape filters
// nulls out.
func TestAnalyze_GuardedPredicateNeverNarrows(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id, u.org_id FROM users AS u
WHERE TRUE
@if-present(only_org)
  AND u.org_id IS NOT NULL AND u.org_id = :only_org
@endif
;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), col("org_id", 100, 3),
	}}, nil)
	assertNullable(t, got, []bool{false, true})
}

// Review counterexample F1b: a guarded INNER join over the nullable FK
// must not imply the FK is non-null — the join-off shape returns NULLs.
func TestAnalyze_GuardedInnerJoinNeverNarrows(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id, u.org_id FROM users AS u
@if-present(org_name)
JOIN orgs AS o ON o.id = u.org_id AND o.name = :org_name
@endif
WHERE TRUE;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), col("org_id", 100, 3),
	}}, nil)
	assertNullable(t, got, []bool{false, true})
}

func TestAnalyze_ExpressionWhitelist(t *testing.T) {
	src := `-- name: Q :many
SELECT count(*) AS n, now() AS at, upper(u.email) AS e FROM users AS u;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		{Name: "n"}, {Name: "at"}, {Name: "e"},
	}}, nil)
	// count/now are total; upper(...) stays conservatively nullable.
	assertNullable(t, got, []bool{false, false, true})
}

func TestAnalyze_Overrides(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id, upper(u.email) AS e FROM users AS u;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), {Name: "e"},
	}}, map[string]bool{"id": true, "e": false})
	assertNullable(t, got, []bool{true, false})
}

// A skeleton instance of a table must not inherit a guarded instance's
// exclusion: the skeleton LEFT JOIN still null-extends even though the
// same table also appears in a guarded join.
func TestAnalyze_SelfJoinSkeletonGoverns(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id, o.name FROM users AS u
LEFT JOIN orgs AS o ON o.id = u.org_id
@if-present(x)
JOIN orgs AS o2 ON o2.id = u.org_id AND o2.name = :x
@endif
WHERE TRUE;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), col("name", 200, 2),
	}}, nil)
	assertNullable(t, got, []bool{false, true})
}

func assertNullable(t *testing.T, got, want []bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("column %d nullable = %v, want %v", i, got[i], want[i])
		}
	}
}
