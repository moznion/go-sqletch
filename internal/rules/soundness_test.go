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
	return checkResolvedDialectCat(t, profile, fe, src, f3Catalog())
}

func checkResolvedDialectCat(t *testing.T, profile dialect.LexerProfile, fe dialect.Frontend, src string, cat *cache.Catalog) []diagnostics.Diagnostic {
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

// SQLite folds identifier case for ASCII letters ONLY (sqlite3StrICmp):
// `É` and `é` are different aliases to the engine. Unicode folding
// merged them in R3's by-name map, so the unguarded `é` registered after
// the guarded `É` overwrote it and the skeleton reference `É.kind`
// resolved to the unguarded join — a missed SQLETCH115. (Reachable once
// the frontend parses non-ASCII identifiers.)
func TestF3_SQLiteNonASCIICaseVariantsAreDistinct(t *testing.T) {
	src := `-- name: Bad :many
-- @param x: integer
SELECT u.id FROM users AS u
@if-present(x)
JOIN audits AS É ON É.user_id = u.id AND É.id = :x
@endif
JOIN audits AS é ON é.user_id = u.id
WHERE TRUE
  AND É.kind = 'k'
;
`
	diags := checkResolvedDialect(t, sqlite.Profile{}, sqlite.Frontend{}, src)
	if !hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("want SQLETCH115 for the skeleton ref to the guarded `É`, got %+v", diags)
	}
}

// The other direction: a skeleton reference to the unguarded `é` is fine
// even though a guarded `É` exists (no false positive from merging).
func TestF3_SQLiteNonASCIIUnguardedVariantIsFine(t *testing.T) {
	src := `-- name: Ok :many
-- @param x: integer
SELECT u.id FROM users AS u
JOIN audits AS é ON é.user_id = u.id
@if-present(x)
JOIN audits AS É ON É.user_id = u.id AND É.id = :x
@endif
WHERE TRUE
  AND é.kind = 'k'
;
`
	if diags := checkResolvedDialect(t, sqlite.Profile{}, sqlite.Frontend{}, src); hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("unguarded `é` ref must not fire, got %+v", diags)
	}
}

// MySQL relation names compare exactly under lower_case_table_names=0
// and case-insensitively under 1/2; sqletch cannot see which. A
// qualifier is checked against EVERY relation it may name: one Unicode-
// folded by-name slot let the unguarded `a` (registered last) stand in
// for the guarded `A`, so `A.kind` in the skeleton passed (missed
// SQLETCH115 under lower_case_table_names=0, where `A` is exactly the
// guarded join).
func TestF3_MySQLCaseVariantAliasesCheckEveryCandidate(t *testing.T) {
	for _, ref := range []string{"A", "a"} { // `a.kind` is the guarded join under 1/2
		src := `-- name: Bad :many
-- @param x: bigint
SELECT u.id FROM users AS u
@if-present(x)
JOIN audits AS A ON A.user_id = u.id AND A.id = :x
@endif
JOIN audits AS a ON a.user_id = u.id
WHERE TRUE
  AND ` + ref + `.kind = 'k'
;
`
		diags := checkResolvedDialect(t, mysql.Profile{}, mysql.Frontend{}, src)
		if !hasCode(diags, diagnostics.CodeScopeViolation) {
			t.Errorf("ref %s.kind: want SQLETCH115, got %+v", ref, diags)
		}
	}
}

// Shadowing SKIPS a check, so a subquery alias shadows a top-level
// relation only when the names certainly match: `a` does not shadow the
// guarded `A` under lower_case_table_names=0, so the correlated `A.kind`
// must still be checked.
func TestF3_MySQLCaseVariantSubqueryAliasDoesNotShadow(t *testing.T) {
	src := `-- name: Bad :many
-- @param x: bigint
SELECT u.id FROM users AS u
@if-present(x)
JOIN audits AS A ON A.user_id = u.id AND A.id = :x
@endif
WHERE TRUE
  AND EXISTS (SELECT 1 FROM audits AS a WHERE a.id = A.id)
;
`
	diags := checkResolvedDialect(t, mysql.Profile{}, mysql.Frontend{}, src)
	if !hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("want SQLETCH115 for the correlated A.id, got %+v", diags)
	}
}

// MySQL column names fold through the server's own table, which merges
// strictly LESS than Go's strings.ToLower (486 BMP pairs, e.g. `ẞ`/`ß`):
// `ẞ` is not the guarded join's column `ß`, so an unqualified `ẞ` in the
// skeleton must not be reported against it (Go's fold made it a false
// SQLETCH115). Nothing MySQL merges is kept apart by Go, so the old fold
// over-reported only; this pins the precision, the relation-name tests
// above pin the soundness.
func TestF3_MySQLServerColumnFold(t *testing.T) {
	cat := f3Catalog()
	cat.Tables[1].Cols = append(cat.Tables[1].Cols, cache.Column{Name: "ß", Att: 4, NotNull: true})
	cat.Tables[0].Cols = append(cat.Tables[0].Cols, cache.Column{Name: "ẞ", Att: 3, NotNull: true})
	src := `-- name: Ok :many
-- @param x: bigint
SELECT u.id FROM users AS u
@if-present(x)
JOIN audits AS au ON au.user_id = u.id AND au.id = :x
@endif
WHERE TRUE
  AND ẞ = 'k'
;
`
	diags := checkResolvedDialectCat(t, mysql.Profile{}, mysql.Frontend{}, src, cat)
	if hasCode(diags, diagnostics.CodeScopeViolation) || hasCode(diags, diagnostics.CodeAmbiguousRef) {
		t.Fatalf("`ẞ` is users' own column, not audits' `ß`; got %+v", diags)
	}
}
