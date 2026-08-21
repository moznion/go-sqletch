package rules

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

// fixtureCatalog mirrors the schema used across the design docs.
func fixtureCatalog() *cache.Catalog {
	tbl := func(oid uint32, name string, cols ...string) cache.Table {
		t := cache.Table{Schema: "public", Name: name, OID: oid}
		for i, c := range cols {
			t.Cols = append(t.Cols, cache.Column{Name: c, Att: int16(i + 1), NotNull: true})
		}
		return t
	}
	return &cache.Catalog{
		SchemaFP: "fixture",
		Tables: []cache.Table{
			tbl(1001, "users", "id", "email", "status", "created_at", "tenant_id", "org_id"),
			tbl(1002, "organization_users", "user_id", "organization_id", "created_at"),
			tbl(1003, "orgs", "id", "name"),
			tbl(1004, "audits", "id", "user_id", "kind"),
		},
	}
}

func checkResolved(t *testing.T, src string) []diagnostics.Diagnostic {
	t.Helper()
	q := scanOne(t, src)
	rs, err := ast.Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	if d := CheckR1(postgres.Profile{}, postgres.Frontend{}, q, rs); len(d) != 0 {
		t.Fatalf("R1 diagnostics (test precondition): %+v", d)
	}
	tree, err := postgres.Frontend{}.Parse(rs[0].SQL)
	if err != nil {
		t.Fatal(err)
	}
	return CheckResolved(postgres.Profile{}, q, rs[0], tree, fixtureCatalog())
}

// H3: an UNqualified column used as the LHS of `IN (subquery)` (or a
// scalar `= (subquery)`) is semantically in the OUTER scope — it is the
// SubLink's Testexpr, not part of the subquery body. R3 must still check
// it. `kind` exists only on the optional-join relation `audits`, so an
// unguarded reference to it must be rejected (SQLETCH115) exactly as the
// qualified form `a.kind` already is.
func TestCheckResolved_R3_UnqualifiedInSubqueryLHS(t *testing.T) {
	inSrc := `-- name: Bad :many
SELECT u.id FROM users AS u
@if-present(k)
JOIN audits AS a ON a.user_id = u.id AND a.id = :k
@endif
WHERE kind IN (SELECT email FROM users)
;
`
	if diags := checkResolved(t, inSrc); !hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("IN-subquery LHS: want SQLETCH115, got %+v", diags)
	}

	// The scalar `= (subquery)` form shares the SubLink Testexpr path and
	// must also be rejected.
	scalarSrc := `-- name: Bad :many
SELECT u.id FROM users AS u
@if-present(k)
JOIN audits AS a ON a.user_id = u.id AND a.id = :k
@endif
WHERE kind = (SELECT count(*) FROM users)
;
`
	if diags := checkResolved(t, scalarSrc); !hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("scalar-subquery LHS: want SQLETCH115, got %+v", diags)
	}

	// The qualified form was already rejected — keep it pinned.
	qualSrc := `-- name: Bad :many
SELECT u.id FROM users AS u
@if-present(k)
JOIN audits AS a ON a.user_id = u.id AND a.id = :k
@endif
WHERE a.kind IN (SELECT email FROM users)
;
`
	if diags := checkResolved(t, qualSrc); !hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("qualified LHS: want SQLETCH115, got %+v", diags)
	}
}

// H3 (no false positive): references that legitimately live INSIDE the
// subquery body stay unchecked by R3 (bare-column innermost resolution
// is unmodeled), and an outer LHS that resolves to an unguarded skeleton
// relation must not be flagged.
func TestCheckResolved_R3_InSubqueryBodyStillSkipped(t *testing.T) {
	src := `-- name: OK :many
SELECT u.id FROM users AS u
WHERE u.tenant_id IN (SELECT a.user_id FROM audits AS a WHERE a.kind = u.id)
;
`
	if diags := checkResolved(t, src); len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
}

func TestCheckResolved_Clean(t *testing.T) {
	src := `-- name: SearchUsers :many
SELECT u.id, u.email, u.status
FROM users AS u
@if-present(organization_id)
JOIN organization_users AS ou
  ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif
WHERE TRUE
@if-present(status)
  AND u.status = :status
@endif
@choose(sort)
@case(created_at_desc)
ORDER BY u.created_at DESC
@default
ORDER BY u.id ASC
@end
LIMIT :limit;
`
	if diags := checkResolved(t, src); len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
}

func TestCheckResolved_R3_QualifiedRefOutsideGuard(t *testing.T) {
	src := `-- name: Bad :many
SELECT u.id FROM users AS u
@if-present(organization_id)
JOIN organization_users AS ou ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif
WHERE TRUE
  AND ou.created_at > :since
;
`
	diags := checkResolved(t, src)
	if !hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("want SQLETCH115, got %+v", diags)
	}
}

// The soundness-critical case from the review: an UNQUALIFIED
// reference resolving into the optional join must be caught (R3 is
// resolution-based, not textual).
func TestCheckResolved_R3_UnqualifiedRef(t *testing.T) {
	src := `-- name: Bad :many
SELECT u.id FROM users AS u
@if-present(oid)
JOIN organization_users AS ou ON ou.user_id = u.id AND ou.organization_id = :oid
@endif
WHERE TRUE
  AND organization_id = :oid
;
`
	diags := checkResolved(t, src)
	if !hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("want SQLETCH115 for unqualified ref, got %+v", diags)
	}
}

// @choose case bodies have the empty guard set: referencing an
// optional join from a case is a scope violation (review fix F3).
func TestCheckResolved_R3_ChooseCaseRefsOptionalJoin(t *testing.T) {
	src := `-- name: Bad :many
SELECT u.id FROM users AS u
@if-present(organization_id)
JOIN organization_users AS ou ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif
WHERE TRUE
@choose(sort)
@case(by_org)
ORDER BY ou.created_at DESC
@default
ORDER BY u.id ASC
@end
;
`
	diags := checkResolved(t, src)
	if !hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("want SQLETCH115 for choose-case ref, got %+v", diags)
	}
}

// Guard-superset chains are legal: a fragment guarded by (a, b) may
// reference a join guarded by (a).
func TestCheckResolved_R3_GuardSupersetChain(t *testing.T) {
	src := `-- name: Chain :many
SELECT u.id FROM users AS u
@if-present(organization_id)
JOIN organization_users AS ou ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif
WHERE TRUE
@if-present(organization_id, since)
  AND ou.created_at > :since
@endif
;
`
	if diags := checkResolved(t, src); len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
}

func TestCheckResolved_R3_SubsetGuardFails(t *testing.T) {
	src := `-- name: Bad :many
SELECT u.id FROM users AS u
@if-present(organization_id)
JOIN organization_users AS ou ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif
WHERE TRUE
@if-present(since)
  AND ou.created_at > :since
@endif
;
`
	diags := checkResolved(t, src)
	if !hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("want SQLETCH115 for subset guard, got %+v", diags)
	}
}

func TestCheckResolved_AmbiguousUnqualified(t *testing.T) {
	src := `-- name: Bad :many
SELECT u.id FROM users AS u
JOIN organization_users AS ou ON ou.user_id = u.id
WHERE TRUE
  AND created_at > :since
;
`
	diags := checkResolved(t, src)
	if !hasCode(diags, diagnostics.CodeAmbiguousRef) {
		t.Fatalf("want SQLETCH114, got %+v", diags)
	}
}

func TestCheckResolved_StarExpansion(t *testing.T) {
	base := `-- name: Q :many
SELECT %s FROM users AS u
@if-present(x)
JOIN audits AS a ON a.user_id = u.id AND a.kind = :x
@endif
WHERE TRUE;
`
	tests := []struct {
		proj string
		want bool
	}{
		{"*", true},    // bare star would absorb the optional join
		{"a.*", true},  // projecting the optional join directly
		{"u.*", false}, // skeleton-qualified star is shape-constant
	}
	for _, tt := range tests {
		src := "-- name: Q :many\nSELECT " + tt.proj + ` FROM users AS u
@if-present(x)
JOIN audits AS a ON a.user_id = u.id AND a.kind = :x
@endif
WHERE TRUE;
`
		diags := checkResolved(t, src)
		if got := hasCode(diags, diagnostics.CodeStarExpansion); got != tt.want {
			t.Errorf("proj %q: SQLETCH117 = %v, want %v (%+v)", tt.proj, got, tt.want, diags)
		}
	}
	_ = base
}

func TestCheckResolved_PlannerSensitive_ForUpdateWithOptionalLeftJoin(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id FROM users AS u
@if-present(x)
LEFT JOIN audits AS a ON a.user_id = u.id AND a.kind = :x
@endif
WHERE TRUE
FOR UPDATE;
`
	diags := checkResolved(t, src)
	if !hasCode(diags, diagnostics.CodePlannerSensitive) {
		t.Fatalf("want SQLETCH116, got %+v", diags)
	}

	inner := `-- name: Q :many
SELECT u.id FROM users AS u
@if-present(x)
JOIN audits AS a ON a.user_id = u.id AND a.kind = :x
@endif
WHERE TRUE
FOR UPDATE;
`
	if diags := checkResolved(t, inner); hasCode(diags, diagnostics.CodePlannerSensitive) {
		t.Fatalf("INNER join must not trigger SQLETCH116: %+v", diags)
	}
}

// References inside the optional join's own ON clause are covered by
// the join's guard (the fragment encloses them).
func TestCheckResolved_OnClauseRefsAreFine(t *testing.T) {
	src := `-- name: Q :many
SELECT u.id FROM users AS u
@if-present(x)
JOIN audits AS a ON a.user_id = u.id AND a.kind = :x
@endif
WHERE TRUE;
`
	if diags := checkResolved(t, src); len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
}

// Correlated qualified refs inside subqueries are still checked.
func TestCheckResolved_R3_CorrelatedSubqueryRef(t *testing.T) {
	src := `-- name: Bad :many
SELECT u.id FROM users AS u
@if-present(organization_id)
JOIN organization_users AS ou ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif
WHERE TRUE
  AND EXISTS (SELECT 1 FROM audits AS a WHERE a.user_id = ou.user_id)
;
`
	diags := checkResolved(t, src)
	if !hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("want SQLETCH115 for correlated ref, got %+v", diags)
	}
}

// A subquery-local alias that shadows a guarded top-level join's alias
// binds innermost-first, so R3 must NOT demand the top-level guard for a
// reference through that inner alias (design 03 §6). This was a spurious
// SQLETCH115 before qualified-ref scope resolution.
func TestCheckResolved_R3_SubqueryAliasShadowsGuardedJoin(t *testing.T) {
	src := `-- name: Ok :many
SELECT u.id FROM users AS u
@if-present(organization_id)
JOIN orgs AS o ON o.id = u.org_id
 AND o.id = :organization_id
@endif
WHERE TRUE
  AND EXISTS (SELECT 1 FROM audits AS o WHERE o.user_id = u.id)
;
`
	diags := checkResolved(t, src)
	if hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("subquery alias o shadows the guarded top-level orgs o; want no SQLETCH115, got %+v", diags)
	}
}

// A join alias hides the inner relation names (PostgreSQL §7.2.1.2), so
// an inner alias must NOT enter ScopeAliases: a reference through that
// name inside the subquery is correlated to the same-named guarded
// top-level relation and must still raise SQLETCH115. Over-collecting the
// hidden inner names (the pre-fix behaviour) silently dropped the guard
// demand and shipped a guard-off shape that only fails at runtime.
func TestCheckResolved_R3_AliasedJoinHidesInnerNames(t *testing.T) {
	src := `-- name: Bad :many
SELECT u.id FROM users AS u
@if-present(organization_id)
JOIN orgs AS o ON o.id = u.org_id
 AND o.id = :organization_id
@endif
WHERE TRUE
  AND EXISTS (
    SELECT 1 FROM (audits AS o JOIN users AS y ON y.id = o.user_id) AS j
    WHERE o.id = u.id
  )
;
`
	diags := checkResolved(t, src)
	if !hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("join alias j hides inner audits o; correlated o.id must raise SQLETCH115, got %+v", diags)
	}
}

// A schema-qualified reference (schema.table.column) can never be
// alias-shadowed: a bare FROM alias carries no schema. So a three-field
// correlated reference to a guarded top-level relation must raise
// SQLETCH115 even when an inner subquery introduces a same-named alias.
func TestCheckResolved_R3_SchemaQualifiedCorrelatedRef(t *testing.T) {
	src := `-- name: Bad :many
SELECT u.id FROM users AS u
@if-present(organization_id)
JOIN organization_users ON organization_users.user_id = u.id
 AND organization_users.organization_id = :organization_id
@endif
WHERE TRUE
  AND EXISTS (
    SELECT 1 FROM audits AS organization_users
    WHERE public.organization_users.user_id = u.id
  )
;
`
	diags := checkResolved(t, src)
	if !hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("schema-qualified public.organization_users.user_id cannot be alias-shadowed; want SQLETCH115, got %+v", diags)
	}
}

// Two-level shadowing: the reference's qualifier is introduced by the
// innermost subquery, so the union of enclosing subquery FROMs must
// suppress the top-level guard demand.
func TestCheckResolved_R3_NestedSubqueryShadowing(t *testing.T) {
	src := `-- name: Ok :many
SELECT u.id FROM users AS u
@if-present(organization_id)
JOIN orgs AS o ON o.id = u.org_id
 AND o.id = :organization_id
@endif
WHERE TRUE
  AND EXISTS (
    SELECT 1 FROM audits AS a
    WHERE EXISTS (SELECT 1 FROM organization_users AS o WHERE o.user_id = a.id)
  )
;
`
	diags := checkResolved(t, src)
	if hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("nested subquery alias o shadows the guarded top-level orgs o; want no SQLETCH115, got %+v", diags)
	}
}
