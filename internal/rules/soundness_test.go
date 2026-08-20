package rules

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/template"
)

// ---- F1: guarded-join derived table must not slip past R1/R2/R3 -----------

// A CHAIN of joins smuggled into one @if-present join fragment
// (`JOIN a ON … JOIN (SELECT …) AS d ON …`) used to pass the postgres
// ProbeJoinItem, which only required one FROM entry with a JoinExpr.
// The derived table `d` (Loc == -1) then detached from every guard, so
// a `d.cnt` reference outside the guard shipped an unverified shape that
// fails at execution ("missing FROM-clause entry for table d"). The
// fixed probe requires exactly two relations, rejecting the chain (R1).
func TestF1_GuardedJoinChainSmugglesDerivedTable(t *testing.T) {
	src := `-- name: Bad :many
SELECT u.id, d.cnt FROM users AS u
@if-present(x)
JOIN audits AS a ON a.user_id = u.id AND a.kind = :x JOIN (SELECT 1 AS cnt) AS d ON TRUE
@endif
WHERE TRUE;
`
	diags := checkR1(t, src)
	if !hasCode(diags, diagnostics.CodeNodeIncomplete) {
		t.Fatalf("want SQLETCH102 for the smuggled join chain, got %+v", diags)
	}
}

// The same hole via a null-extending FULL JOIN of a derived relation
// (an R2 bypass): the chain must be rejected rather than shipping a
// per-shape nullability change.
func TestF1_GuardedFullJoinChainOfDerivedRel(t *testing.T) {
	src := `-- name: Bad :many
SELECT u.id, d.cnt FROM users AS u
@if-present(x)
JOIN audits AS a ON a.user_id = u.id AND a.kind = :x FULL JOIN (SELECT 1 AS cnt) AS d ON TRUE
@endif
WHERE TRUE;
`
	diags := checkR1(t, src)
	if !hasCode(diags, diagnostics.CodeNodeIncomplete) {
		t.Fatalf("want SQLETCH102 for the smuggled FULL-join chain, got %+v", diags)
	}
}

// Defense in depth: even a SINGLE guarded join of a derived table (which
// the probe accepts as one join item) must be rejected — its columns
// carry no offset and can be attributed to no guard, so R2/R3 can never
// verify them. checkJoinMembership rejects it.
func TestF1_GuardedSingleDerivedJoinRejected(t *testing.T) {
	src := `-- name: Bad :many
SELECT u.id FROM users AS u
@if-present(x)
JOIN (SELECT 1 AS cnt) AS d ON d.cnt = u.id AND u.id = :x
@endif
WHERE TRUE;
`
	diags := checkR1(t, src)
	if !hasCode(diags, diagnostics.CodeNodeIncomplete) {
		t.Fatalf("want SQLETCH102 for the guarded derived-table join, got %+v", diags)
	}
	var found bool
	for _, d := range diags {
		if d.Code == diagnostics.CodeNodeIncomplete && strings.Contains(d.Message, "derived table") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the derived-table rejection message, got %+v", diags)
	}
}

// A legitimate SKELETON derived table (unguarded) must still pass R1.
func TestF1_SkeletonDerivedTableStillPasses(t *testing.T) {
	src := `-- name: Ok :many
SELECT u.id, d.cnt FROM users AS u
JOIN (SELECT 1 AS cnt) AS d ON d.cnt = u.id
WHERE TRUE
@if-present(x)
  AND u.status = :x
@endif
;
`
	if diags := checkR1(t, src); len(diags) != 0 {
		t.Fatalf("legitimate skeleton derived table rejected: %+v", diags)
	}
}

// A legitimate single guarded named join must still pass R1.
func TestF1_LegitGuardedNamedJoinPasses(t *testing.T) {
	src := `-- name: Ok :many
SELECT u.id FROM users AS u
@if-present(x)
JOIN audits AS a ON a.user_id = u.id AND a.kind = :x
@endif
WHERE TRUE;
`
	if diags := checkR1(t, src); len(diags) != 0 {
		t.Fatalf("legitimate guarded named join rejected: %+v", diags)
	}
}

// ---- F2: a SELECT output alias must not mask R3 in WHERE ------------------

// `kind` is an output alias for u.id but is referenced in WHERE, where
// SQL output aliases are invisible: the reference binds to a.kind of the
// guarded join. The old outputAliases skip suppressed the check for any
// name equal to an alias regardless of clause, shipping a guard-off
// shape. R3 must fire.
func TestF2_OutputAliasDoesNotMaskWhereRef(t *testing.T) {
	src := `-- name: Bad :many
SELECT u.id AS kind FROM users AS u
@if-present(x)
JOIN audits AS a ON a.user_id = u.id AND a.id = :x
@endif
WHERE TRUE
  AND kind = 'k'
;
`
	diags := checkResolved(t, src)
	if !hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("want SQLETCH115 for the alias-masked WHERE ref, got %+v", diags)
	}
}

// A genuine output-alias reference in ORDER BY (where aliases ARE
// visible and bind to the alias) must still pass — no false positive.
func TestF2_OutputAliasInOrderByStillPasses(t *testing.T) {
	src := `-- name: Ok :many
SELECT u.status AS kind FROM users AS u
@if-present(x)
JOIN audits AS a ON a.user_id = u.id AND a.id = :x
@endif
WHERE TRUE
ORDER BY kind
;
`
	if diags := checkResolved(t, src); hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("output alias in ORDER BY wrongly flagged: %+v", diags)
	}
}

// ---- F3: case-insensitive dialects must fold identifiers in R3 -----------

func f3Catalog() *cache.Catalog {
	tbl := func(name string, cols ...string) cache.Table {
		tt := cache.Table{Schema: "public", Name: name}
		for i, c := range cols {
			tt.Cols = append(tt.Cols, cache.Column{Name: c, Att: int16(i + 1), NotNull: true})
		}
		return tt
	}
	return &cache.Catalog{
		SchemaFP: "f3",
		Tables: []cache.Table{
			tbl("users", "id", "status"),
			tbl("audits", "id", "user_id", "kind"),
		},
	}
}

func checkResolvedDialect(t *testing.T, profile dialect.LexerProfile, fe dialect.Frontend, src string) []diagnostics.Diagnostic {
	t.Helper()
	f, diags := template.NewScanner(profile).ScanFile("t.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	rs, err := ast.Renderings(profile, q)
	if err != nil {
		t.Fatal(err)
	}
	if d := CheckR1(profile, fe, q, rs); len(d) != 0 {
		t.Fatalf("R1 diagnostics (test precondition): %+v", d)
	}
	tree, err := fe.Parse(rs[0].SQL)
	if err != nil {
		t.Fatal(err)
	}
	return CheckResolved(profile, q, rs[0], tree, f3Catalog())
}

// MySQL resolves `a.kind` case-insensitively to the mixed-case alias `A`
// of the guarded join, so R3 must fire — exact-string matching missed it.
func TestF3_MySQLMixedCaseQualifierRaisesScopeViolation(t *testing.T) {
	src := `-- name: Bad :many
-- @param x: bigint
SELECT u.id FROM users AS u
@if-present(x)
JOIN audits AS A ON A.user_id = u.id AND A.id = :x
@endif
WHERE TRUE
  AND a.kind = 'k'
;
`
	diags := checkResolvedDialect(t, mysql.Profile{}, mysql.Frontend{}, src)
	if !hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("want SQLETCH115 for the mixed-case MySQL ref, got %+v", diags)
	}
}

// SQLite shares the case-insensitive resolution and the same code path.
func TestF3_SQLiteMixedCaseQualifierRaisesScopeViolation(t *testing.T) {
	src := `-- name: Bad :many
-- @param x: integer
SELECT u.id FROM users AS u
@if-present(x)
JOIN audits AS A ON A.user_id = u.id AND A.id = :x
@endif
WHERE TRUE
  AND a.kind = 'k'
;
`
	diags := checkResolvedDialect(t, sqlite.Profile{}, sqlite.Frontend{}, src)
	if !hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("want SQLETCH115 for the mixed-case SQLite ref, got %+v", diags)
	}
}

// A same-case reference through the guard's own ON clause must still NOT
// fire on either dialect (no false positive from folding).
func TestF3_MySQLGuardedOnClauseRefIsFine(t *testing.T) {
	src := `-- name: Ok :many
-- @param x: bigint
SELECT u.id FROM users AS u
@if-present(x)
JOIN audits AS A ON A.user_id = u.id AND A.kind = :x
@endif
WHERE TRUE
;
`
	if diags := checkResolvedDialect(t, mysql.Profile{}, mysql.Frontend{}, src); hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("guarded ON-clause ref wrongly flagged: %+v", diags)
	}
}

// ---- R7 companion warning (SQLETCH212) must fold identifier case ----------

// checkResolvedCat runs CheckResolved under an arbitrary dialect/catalog.
func checkResolvedCat(t *testing.T, profile dialect.LexerProfile, fe dialect.Frontend,
	cat *cache.Catalog, src string) []diagnostics.Diagnostic {
	t.Helper()
	f, diags := template.NewScanner(profile).ScanFile("t.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	rs, err := ast.Renderings(profile, q)
	if err != nil {
		t.Fatal(err)
	}
	if d := CheckR1(profile, fe, q, rs); len(d) != 0 {
		t.Fatalf("R1 diagnostics (test precondition): %+v", d)
	}
	tree, err := fe.Parse(rs[0].SQL)
	if err != nil {
		t.Fatal(err)
	}
	return CheckResolved(profile, q, rs[0], tree, cat)
}

// widgetsCat holds a mixed-case NOT-NULL column with no default; the
// template spells it in a different case, which resolves at runtime on a
// case-insensitive dialect. The R7 companion warning must fold both
// sides like the rest of R2/R3, or it silently misses the column.
func widgetsCat() *cache.Catalog {
	return &cache.Catalog{
		SchemaFP: "widgets",
		Tables: []cache.Table{{
			Schema: "public", Name: "widgets", OID: 2001,
			Cols: []cache.Column{
				{Name: "id", Att: 1, NotNull: true, HasDefault: true},
				{Name: "Status", Att: 2, NotNull: true, HasDefault: false},
			},
		}},
	}
}

const widgetsInsert = `-- name: Ins :exec
INSERT INTO widgets (
    id
@if-present(status)
  , status
@endif
) VALUES (
    :id
@if-present(status)
  , :status
@endif
);
`

func TestR7Fold_MySQLMixedCaseNotNullColumnWarns(t *testing.T) {
	diags := checkResolvedCat(t, mysql.Profile{}, mysql.Frontend{}, widgetsCat(), widgetsInsert)
	if !hasCode(diags, diagnostics.CodeOptionalInsertNotNull) {
		t.Fatalf("want SQLETCH212 for the mixed-case NOT NULL column, got %+v", diags)
	}
}

func TestR7Fold_SQLiteMixedCaseNotNullColumnWarns(t *testing.T) {
	diags := checkResolvedCat(t, sqlite.Profile{}, sqlite.Frontend{}, widgetsCat(), widgetsInsert)
	if !hasCode(diags, diagnostics.CodeOptionalInsertNotNull) {
		t.Fatalf("want SQLETCH212 for the mixed-case NOT NULL column, got %+v", diags)
	}
}
