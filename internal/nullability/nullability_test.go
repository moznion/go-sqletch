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

// ---- provenance-trust soundness vectors ------------------------------------
//
// Each fabricated Desc below mirrors wire behavior PROVEN against a
// real engine by the devdb suite (TestNullabilitySoundnessAdversarial
// and its MySQL/SQLite variants): PostgreSQL's resorigtbl resolves
// column origins THROUGH derived tables and CTEs to base-table OIDs,
// SQLite does the same through views and compound selects, and
// grouping sets null out grouping columns outright. These are the
// must-stay-nullable regressions for those counterexamples.

// Derived table on the null-extended side: the wire protocol
// attributes s.name to orgs (OID 200) even though orgs never appears
// in FROM — narrowing from it would be unsound.
func TestAnalyze_DerivedTableProvenanceNeverNarrows(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id, s.name FROM users AS u
LEFT JOIN (SELECT o.id, o.name FROM orgs AS o) AS s ON s.id = u.org_id;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), col("name", 200, 2),
	}}, nil)
	assertNullable(t, got, []bool{true, true}) // ALL narrowing off: opaque
}

// The dual-instance trap: the same table is both directly present
// (not null-extended) and wrapped in a null-extended derived table.
// Presence alone would narrow the derived column; the opaque
// kill-switch must win.
func TestAnalyze_DualInstanceDerivedNeverNarrows(t *testing.T) {
	src := `-- name: Q :many
SELECT o1.name, s.name FROM orgs AS o1
LEFT JOIN (SELECT o.id, o.name FROM orgs AS o) AS s ON s.id = o1.id;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("name", 200, 2), col("name", 200, 2),
	}}, nil)
	assertNullable(t, got, []bool{true, true})
}

// CTE on the null-extended side (same wire attribution as the derived
// table, proven by the devdb cte_on_null_side case).
func TestAnalyze_CTEProvenanceNeverNarrows(t *testing.T) {
	src := `-- name: Q :many
WITH s AS (SELECT o.id, o.name FROM orgs AS o)
SELECT u.id, s.name FROM users AS u LEFT JOIN s ON s.id = u.org_id;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), col("name", 200, 2),
	}}, nil)
	assertNullable(t, got, []bool{true, true})
}

// A set operation disables narrowing wholesale (SQLite attributes
// compound output to the FIRST branch; PostgreSQL reports no origin —
// both must stay nullable).
func TestAnalyze_SetOperationNeverNarrows(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id FROM users AS u UNION ALL SELECT u2.org_id FROM users AS u2;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1),
	}}, nil)
	assertNullable(t, got, []bool{true})
}

// Grouping sets null out grouping columns in super-aggregate rows;
// catalog NOT NULL must not narrow them. count(*) stays total.
func TestAnalyze_GroupingSetsNeverNarrow(t *testing.T) {
	for _, groupBy := range []string{
		"GROUP BY ROLLUP(u.email)",
		"GROUP BY CUBE(u.email)",
		"GROUP BY GROUPING SETS ((u.email), ())",
	} {
		src := "-- name: Q :many\nSELECT u.email, count(*) AS n FROM users AS u\n" +
			groupBy + ";\n"
		got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
			col("email", 100, 2), {Name: "n"},
		}}, nil)
		assertNullable(t, got, []bool{true, false})
	}
}

// An explicitly schema-qualified relation must resolve against that
// schema exactly: aux.orgs shares its name with public-preferred orgs,
// and its null-extended instance must not narrow (the devdb
// schema_qualified_name_collision counterexample), while a source OID
// the FROM list cannot account for stays nullable in general.
func TestAnalyze_SchemaQualifiedCollision(t *testing.T) {
	auxCat := cat()
	auxCat.Tables = append(auxCat.Tables, cache.Table{
		Schema: "aux", Name: "orgs", OID: 900, Cols: []cache.Column{
			{Name: "id", Att: 1, NotNull: true},
			{Name: "name", Att: 2, NotNull: true},
		}})
	src := `-- name: Q :many
SELECT u.id, o.name FROM users AS u LEFT JOIN aux.orgs AS o ON o.id = u.org_id;
`
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
	got := Analyze(tree, rs[0], dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), col("name", 900, 2), // the wire reports aux.orgs' true identity
	}}, auxCat, nil)
	assertNullable(t, got, []bool{false, true})
}

// A source OID that no skeleton FROM relation accounts for fails safe
// to nullable — the general form of every provenance counterexample.
func TestAnalyze_UnaccountedSourceStaysNullable(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id FROM users AS u;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 200, 1), // attributed to orgs, which is not in FROM
	}}, nil)
	assertNullable(t, got, []bool{true})
}

// Star expansion breaks the index alignment between target items and
// described columns; the expression whitelist must not fire then
// (a count(*) target could otherwise vouch for a different column).
func TestAnalyze_StarMisalignmentDisablesWhitelist(t *testing.T) {
	src := `-- name: Q :many
SELECT u.*, count(*) AS n FROM users AS u GROUP BY u.id, u.email, u.org_id;
`
	// users has 3 columns; desc has 4. Index 3 (count) has no target
	// at its index... and index alignment cannot be established at
	// all, so even the genuine count column stays nullable.
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), col("email", 100, 2), col("org_id", 100, 3), {Name: "n"},
	}}, nil)
	assertNullable(t, got, []bool{false, false, true, true})
}

// Reasons are part of the explain contract: each verdict names the
// gate that produced it, so a conservative verdict is auditable.
func TestAnalyzeVerdicts_Reasons(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id, o.name, u.org_id, upper(u.email) AS e, count(*) AS n
FROM users AS u LEFT JOIN orgs AS o ON o.id = u.org_id;
`
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
	got := AnalyzeVerdicts(tree, rs[0], dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), col("name", 200, 2), col("org_id", 100, 3),
		{Name: "e"}, {Name: "n"},
	}}, cat(), map[string]bool{"e": false})
	want := []Verdict{
		{Nullable: false, Reason: "users.id is NOT NULL in the catalog"},
		{Nullable: true, Reason: "orgs.name is null-extended by an outer join"},
		{Nullable: true, Reason: "users.org_id is nullable in the catalog"},
		{Nullable: false, Reason: "forced by null_overrides"},
		{Nullable: false, Reason: "total function count()"},
	}
	if len(got) != len(want) {
		t.Fatalf("verdicts = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("column %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestAnalyzeVerdicts_UntrustedReasons(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id FROM users AS u LEFT JOIN (SELECT o.id FROM orgs AS o) AS s ON s.id = u.org_id;
`
	got := analyzeVerdictsOf(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1),
	}})
	wantReason := "narrowing disabled: statement contains a derived table, CTE, or set operation"
	if !got[0].Nullable || got[0].Reason != wantReason {
		t.Errorf("verdict = %+v, want nullable with reason %q", got[0], wantReason)
	}

	src = `-- name: Q :many
SELECT u.email, count(*) AS n FROM users AS u GROUP BY ROLLUP(u.email);
`
	got = analyzeVerdictsOf(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("email", 100, 2), {Name: "n"},
	}})
	wantReason = "narrowing disabled: ROLLUP/CUBE/GROUPING SETS nulls grouping columns"
	if !got[0].Nullable || got[0].Reason != wantReason {
		t.Errorf("verdict = %+v, want nullable with reason %q", got[0], wantReason)
	}
	if got[1].Nullable {
		t.Errorf("count(*) = %+v, must stay total under grouping sets", got[1])
	}
}

func analyzeVerdictsOf(t *testing.T, src string, desc dialect.Desc) []Verdict {
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
	return AnalyzeVerdicts(tree, rs[0], desc, cat(), nil)
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
