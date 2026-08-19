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
// directly in FROM. Recursive analysis (design 05 §2b) tracks orgs
// THROUGH the derived table as null-extended — and, unlike the old
// wholesale kill-switch, keeps narrowing the direct users columns.
func TestAnalyze_DerivedTableProvenanceNeverNarrows(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id, s.name FROM users AS u
LEFT JOIN (SELECT o.id, o.name FROM orgs AS o) AS s ON s.id = u.org_id;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), col("name", 200, 2),
	}}, nil)
	assertNullable(t, got, []bool{false, true})
}

// The recursion's precision positive: an INNER-joined derived table
// over a NOT NULL column narrows — the sub-level has no null
// extension and neither does the enclosing side.
func TestAnalyze_DerivedTableInnerJoinNarrows(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id, s.name FROM users AS u
JOIN (SELECT o.id, o.name FROM orgs AS o) AS s ON s.id = u.org_id;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), col("name", 200, 2),
	}}, nil)
	assertNullable(t, got, []bool{false, false})
}

// Null extension INSIDE the derived body compounds outward: orgs on
// the null-extended side of the sub-level LEFT JOIN stays nullable
// even though the outer join is INNER.
func TestAnalyze_DerivedTableInnerBodyLeftJoin(t *testing.T) {
	src := `-- name: Q :many
SELECT s.name FROM users AS u
JOIN (SELECT u2.id, o.name FROM users AS u2 LEFT JOIN orgs AS o ON o.id = u2.org_id) AS s
  ON s.id = u.id;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("name", 200, 2),
	}}, nil)
	assertNullable(t, got, []bool{true})
}

// Two levels of derived tables narrow when every level is clean.
func TestAnalyze_NestedDerivedNarrows(t *testing.T) {
	src := `-- name: Q :many
SELECT s.name FROM (SELECT inner2.name FROM (SELECT o.name FROM orgs AS o) AS inner2) AS s;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("name", 200, 2),
	}}, nil)
	assertNullable(t, got, []bool{false})
}

// A set operation INSIDE a derived table poisons every table it
// mentions: SQLite attributes compound output to the FIRST branch's
// table, so even a direct instance of that table must stop narrowing.
func TestAnalyze_SetOpInDerivedPoisons(t *testing.T) {
	src := `-- name: Q :many
SELECT o1.name FROM orgs AS o1
JOIN (SELECT o.id FROM orgs AS o UNION ALL SELECT u.org_id FROM users AS u) AS s
  ON s.id = o1.id
WHERE o1.name IS NOT NULL;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("name", 200, 2),
	}}, nil)
	// Poisoned beats both catalog NOT NULL and the IS NOT NULL filter
	// (the filter's single-instance requirement rejects poisoned OIDs).
	assertNullable(t, got, []bool{true})
}

// A recursive CTE poisons the tables its body mentions.
func TestAnalyze_RecursiveCTEPoisons(t *testing.T) {
	src := `-- name: Q :many
WITH RECURSIVE r AS (
  SELECT o.id, o.name FROM orgs AS o
  UNION ALL
  SELECT r.id, r.name FROM r WHERE r.id < 10
)
SELECT r.name FROM r;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("name", 200, 2),
	}}, nil)
	assertNullable(t, got, []bool{true})
}

// A CTE shadowing a real table must resolve to the CTE: the catalog
// table named "users" is NOT granted presence by `FROM users` when a
// CTE of that name is in scope.
func TestAnalyze_CTEShadowingResolvesToCTE(t *testing.T) {
	src := `-- name: Q :many
WITH users AS (SELECT o.id, o.name FROM orgs AS o)
SELECT u.id FROM users AS u;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		// The engine would attribute through the CTE to orgs.id; a
		// hypothetical users-attributed column must stay nullable.
		col("id", 100, 1),
	}}, nil)
	assertNullable(t, got, []bool{true})
}

// Forward CTE-name reference (H5): inside the body of an EARLIER CTE,
// a name matching a LATER CTE binds to the base relation, not to the
// forward CTE — PostgreSQL/MySQL resolve a non-recursive body against
// preceding definitions only. Here `a`'s body LEFT JOINs `orgs`, which
// (because the CTE `orgs` is defined afterwards) is the base table on a
// null-extended side; a schema-qualified `public.orgs` supplies a
// second, clean instance. Analyzing the forward reference as the later
// CTE would hide the null extension and leave orgs a single clean
// instance, unsoundly narrowing org_name. Positional scoping records
// both instances, so org_name stays nullable.
func TestAnalyze_ForwardCTEReferenceNeverNarrows(t *testing.T) {
	src := `-- name: Q :many
WITH a AS (
  SELECT o.name AS org_name
  FROM users AS u LEFT JOIN orgs AS o ON o.id = u.org_id
),
orgs AS (SELECT 1 AS one)
SELECT a.org_name FROM a, public.orgs AS po WHERE po.id = 1;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		// The engine attributes a.org_name through CTE `a` to the base
		// table orgs.name (NOT NULL); it can still be NULL for a member
		// with no org.
		col("org_name", 200, 2),
	}}, nil)
	assertNullable(t, got, []bool{true})
}

// The backward CTE reference (the ordinary case) must still narrow: `b`
// legitimately references the earlier `a`, whose clean INNER-joined
// body attributes org name non-null. Positional scoping keeps earlier
// definitions visible, so precision is preserved.
func TestAnalyze_BackwardCTEReferenceStillNarrows(t *testing.T) {
	src := `-- name: Q :many
WITH a AS (SELECT o.id, o.name FROM orgs AS o),
     b AS (SELECT u.id, a.name AS org_name FROM users AS u JOIN a ON a.id = u.org_id)
SELECT b.org_name FROM b;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("org_name", 200, 2),
	}}, nil)
	assertNullable(t, got, []bool{false})
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
// table, proven by the devdb cte_on_null_side case): the recursion
// compounds the reference's null extension into the body's tables
// while keeping direct users columns narrowed.
func TestAnalyze_CTEProvenanceNeverNarrows(t *testing.T) {
	src := `-- name: Q :many
WITH s AS (SELECT o.id, o.name FROM orgs AS o)
SELECT u.id, s.name FROM users AS u LEFT JOIN s ON s.id = u.org_id;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), col("name", 200, 2),
	}}, nil)
	assertNullable(t, got, []bool{false, true})
}

// The CTE precision positive: an INNER-joined clean CTE body narrows.
func TestAnalyze_CTEInnerJoinNarrows(t *testing.T) {
	src := `-- name: Q :many
WITH s AS (SELECT o.id, o.name FROM orgs AS o)
SELECT u.id, s.name FROM users AS u JOIN s ON s.id = u.org_id;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1), col("name", 200, 2),
	}}, nil)
	assertNullable(t, got, []bool{false, false})
}

// The IS NOT NULL filter's single-instance requirement counts
// instances ACROSS levels: a second instance inside a derived table
// shares the (SrcRel, SrcAtt) key and blocks the narrowing.
func TestAnalyze_IsNotNullCrossLevelInstanceBlocks(t *testing.T) {
	src := `-- name: Q :many
SELECT s.org_id FROM users AS u
JOIN (SELECT u2.id, u2.org_id FROM users AS u2) AS s ON s.id = u.id
WHERE u.org_id IS NOT NULL;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("org_id", 100, 3),
	}}, nil)
	assertNullable(t, got, []bool{true})
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

// ---- precision pack (design 05 §3a) ----------------------------------------

// A skeleton depth-0 `IS NOT NULL` conjunct narrows past both catalog
// nullability and outer-join null-extension: WHERE runs after joins
// and the conjunct is present in every shape.
func TestAnalyze_SkeletonIsNotNullNarrows(t *testing.T) {
	src := `-- name: Q :many
SELECT u.org_id, o.name FROM users AS u
LEFT JOIN orgs AS o ON o.id = u.org_id
WHERE u.org_id IS NOT NULL AND o.name IS NOT NULL;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("org_id", 100, 3), col("name", 200, 2),
	}}, nil)
	assertNullable(t, got, []bool{false, false})
}

// The (SrcRel, SrcAtt) key cannot tell two instances of one table
// apart: with a self join, filtering ONE instance must not narrow —
// the described column could come from the other.
func TestAnalyze_IsNotNullSelfJoinNeverNarrows(t *testing.T) {
	src := `-- name: Q :many
SELECT u2.org_id FROM users AS u1
JOIN users AS u2 ON u2.id = u1.id
WHERE u1.org_id IS NOT NULL;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("org_id", 100, 3),
	}}, nil)
	assertNullable(t, got, []bool{true})
}

// An OR-nested IS NOT NULL is not a depth-0 conjunct and must not
// narrow.
func TestAnalyze_IsNotNullInsideOrNeverNarrows(t *testing.T) {
	src := `-- name: Q :many
SELECT u.org_id FROM users AS u
WHERE u.org_id IS NOT NULL OR u.email = 'x';
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("org_id", 100, 3),
	}}, nil)
	assertNullable(t, got, []bool{true})
}

// Total expressions: data-independent never-NULL forms narrow; a
// coalesce whose fallback is a column does not (F-doc counterexample:
// NULL when all args are null).
func TestAnalyze_TotalExpressions(t *testing.T) {
	src := `-- name: Q :many
SELECT
  1 AS one,
  EXISTS (SELECT 1 FROM orgs AS o WHERE o.id = u.org_id) AS has_org,
  u.org_id IS NOT NULL AS tested,
  coalesce(u.nickname, 'anon') AS nick,
  coalesce(u.nickname, u.bio) AS both_cols,
  NULL AS n
FROM users AS u;
`
	got := analyze(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		{Name: "one"}, {Name: "has_org"}, {Name: "tested"},
		{Name: "nick"}, {Name: "both_cols"}, {Name: "n"},
	}}, nil)
	assertNullable(t, got, []bool{false, false, false, false, true, true})
}

// Strict aggregates narrow only under a plain GROUP BY over a
// provably non-null argument; every escape (no GROUP BY, nullable
// argument, FILTER clause, window form) stays nullable.
func TestAnalyze_StrictAggregates(t *testing.T) {
	grouped := `-- name: Q :many
SELECT u.status, sum(u.id) AS s, max(u.org_id) AS m,
       sum(u.id) FILTER (WHERE u.id > 3) AS sf,
       sum(u.id) OVER () AS sw
FROM users AS u GROUP BY u.status, u.id;
`
	got := analyze(t, grouped, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("status", 100, 2), {Name: "s"}, {Name: "m"}, {Name: "sf"}, {Name: "sw"},
	}}, nil)
	// sum(id): non-null column + GROUP BY -> non-null.
	// max(org_id): nullable argument -> nullable.
	// FILTER can empty the aggregated input -> nullable.
	// window form -> nullable.
	assertNullable(t, got, []bool{false, false, true, true, true})

	ungrouped := `-- name: Q :many
SELECT sum(u.id) AS s FROM users AS u;
`
	got = analyze(t, ungrouped, dialect.Desc{Columns: []dialect.ColumnDesc{{Name: "s"}}}, nil)
	// Empty input yields one NULL row without GROUP BY.
	assertNullable(t, got, []bool{true})
}

// A plain-inheritance parent's NOT NULL is not enforced on children
// (PG 16: a child can DROP the inherited constraint), so scans that
// include children must not narrow; FROM ONLY excludes them and may.
func TestAnalyze_InheritanceParentNeverNarrows(t *testing.T) {
	inhCat := cat()
	inhCat.Tables = append(inhCat.Tables, cache.Table{
		Schema: "public", Name: "parent", OID: 300, HasChildren: true,
		Cols: []cache.Column{{Name: "x", Att: 1, NotNull: true}},
	})
	run := func(src string) []bool {
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
		return Analyze(tree, rs[0], dialect.Desc{Columns: []dialect.ColumnDesc{
			col("x", 300, 1),
		}}, inhCat, nil)
	}
	assertNullable(t, run("-- name: Q :many\nSELECT p.x FROM parent AS p;\n"), []bool{true})
	assertNullable(t, run("-- name: Q :many\nSELECT p.x FROM ONLY parent AS p;\n"), []bool{false})
	// The skeleton IS NOT NULL filter runs after the scan and beats
	// the inheritance hazard.
	assertNullable(t,
		run("-- name: Q :many\nSELECT p.x FROM parent AS p WHERE p.x IS NOT NULL;\n"),
		[]bool{false})
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
SELECT u.id FROM users AS u UNION ALL SELECT u2.org_id FROM users AS u2;
`
	got := analyzeVerdictsOf(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("id", 100, 1),
	}})
	wantReason := "narrowing disabled: statement is a set operation"
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

	src = `-- name: Q :many
SELECT o1.name FROM orgs AS o1
JOIN (SELECT o.id FROM orgs AS o UNION ALL SELECT u.org_id FROM users AS u) AS s ON s.id = o1.id;
`
	got = analyzeVerdictsOf(t, src, dialect.Desc{Columns: []dialect.ColumnDesc{
		col("name", 200, 2),
	}})
	wantReason = "orgs.name is exposed through a hazardous subquery (set operation, grouping sets, or a recursive CTE)"
	if !got[0].Nullable || got[0].Reason != wantReason {
		t.Errorf("verdict = %+v, want nullable with reason %q", got[0], wantReason)
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
