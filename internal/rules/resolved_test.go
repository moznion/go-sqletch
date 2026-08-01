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
	return CheckResolved(q, rs[0], tree, fixtureCatalog())
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
