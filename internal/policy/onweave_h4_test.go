package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// Audit-12 H4 (tenant leak, D2a wrong-join, right-operand refinement):
// when the designated relation is the RIGHT operand of a join that owns
// no ON of its own — a CROSS JOIN or a NATURAL JOIN — the forward token
// scan reaches an ENCLOSING join's depth-0 ON. That ON belongs to the
// enclosing join, not to the designated relation's own (ON-less) join,
// so weaving the tenant conjunct there gates nothing when the enclosing
// join preserves the side the table sits in (FULL preserves both sides;
// RIGHT preserves its left side). Every other-tenant row still ships.
// The weaver must REFUSE (SQLETCH125) rather than weave the leak.
func TestWeaveH4_RightOperandOnLessRefuses(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			// orders is the RIGHT operand of an ON-less CROSS JOIN; the
			// whole (a CROSS orders) group is the right operand of a FULL
			// JOIN, which preserves BOTH sides. The located ON
			// (`ON x.id=a.id`) belongs to the FULL JOIN and gates nothing
			// for orders — every tenant's orders rows survive.
			name: "FULL parent, right-operand CROSS",
			src:  "-- name: Q :many\nSELECT x.id FROM x FULL JOIN a CROSS JOIN orders o ON x.id=a.id\n",
		},
		{
			// The RIGHT-nested variant: orders is the right operand of a
			// CROSS inside a RIGHT JOIN group; the located ON is again a
			// different join's.
			name: "RIGHT-nested, right-operand CROSS",
			src:  "-- name: Q :many\nSELECT z.id FROM z LEFT JOIN (x RIGHT JOIN a CROSS JOIN orders o ON x.id=a.id) ON z.id=a.id\n",
		},
		{
			// NATURAL variant: the PG frontend folds NATURAL to JoinInner,
			// erasing the "owns no ON" fact, so onGates alone cannot see
			// it — the fix detects the introducing NATURAL lexically.
			name: "FULL parent, right-operand NATURAL",
			src:  "-- name: Q :many\nSELECT x.id FROM x FULL JOIN a NATURAL JOIN orders o ON x.id=a.id\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := weaveOne(t, tc.src, tenantPolicy())
			if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
				t.Fatalf("want exactly one SQLETCH125, got %+v", res.Diags)
			}
			if got := renderSQL(t, res.Query); strings.Contains(got, "tenant_id") {
				t.Errorf("leak: a scoping conjunct was woven anyway:\n%s", got)
			}
		})
	}
}

// The enforcement pass re-derives the same decision independently: a
// hand-written scoping conjunct placed in the mis-attributed ON does NOT
// satisfy the invariant, so SQLETCH124 fires (weaver-regression
// backstop, sharing joinOn/onGates with the weaver).
func TestEnforceH4_RightOperandOnLessRejected(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "FULL parent, right-operand CROSS",
			src:  "-- name: Q :many\nSELECT x.id FROM x FULL JOIN a CROSS JOIN orders o ON x.id=a.id AND o.tenant_id = :tenant_id\n",
		},
		{
			name: "FULL parent, right-operand NATURAL",
			src:  "-- name: Q :many\nSELECT x.id FROM x FULL JOIN a NATURAL JOIN orders o ON x.id=a.id AND o.tenant_id = :tenant_id\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := scanOne(t, tc.src)
			diags := enforceOn(t, q, tenantPolicy())
			if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
				t.Fatalf("want exactly one SQLETCH124, got %+v", diags)
			}
		})
	}
}

// Control 1: a genuine INNER-join right-operand occurrence that IS gated
// by the located ON (an inner-join group later null-extended) must STILL
// weave into that inner ON — the fix must not over-refuse.
func TestWeaveH4_InnerRightOperandStillWeaves(t *testing.T) {
	src := "-- name: Q :many\nSELECT v.id FROM base v LEFT JOIN (a JOIN orders o ON a.id = o.id) ON v.z = a.z\n"
	res := weaveOne(t, src, tenantPolicy())
	noDiags(t, res)
	want := "SELECT v.id FROM base v LEFT JOIN (a JOIN orders o ON a.id = o.id AND (o.tenant_id = $1)) ON v.z = a.z"
	if got := renderSQL(t, res.Query); got != want {
		t.Errorf("woven rendering:\n got: %s\nwant: %s", got, want)
	}
}

// Control 2: a plain top-level FROM occurrence still weaves into WHERE.
func TestWeaveH4_PlainFromStillWeaves(t *testing.T) {
	src := "-- name: Q :many\nSELECT id FROM orders WHERE ok\n"
	res := weaveOne(t, src, tenantPolicy())
	noDiags(t, res)
	want := "SELECT id FROM orders WHERE (orders.tenant_id = $1) AND ok"
	if got := renderSQL(t, res.Query); got != want {
		t.Errorf("woven rendering:\n got: %s\nwant: %s", got, want)
	}
}

// The right-operand CROSS under a NON-preserving parent (a plain
// `x LEFT JOIN a CROSS JOIN orders o ON …`) was previously woven
// correctly, but onGates cannot prove the located ON gates orders
// without knowing the enclosing join's type — so the fix conservatively
// REGRESSES it to a loud SQLETCH125 (loud beats a silent leak; audit-12
// H4). This pins that deliberate refusal.
func TestWeaveH4_RightOperandCrossUnderLeftNowRefuses(t *testing.T) {
	src := "-- name: Q :many\nSELECT x.id FROM x LEFT JOIN a CROSS JOIN orders o ON x.id=a.id\n"
	res := weaveOne(t, src, tenantPolicy())
	if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
		t.Fatalf("want exactly one SQLETCH125, got %+v", res.Diags)
	}
	if got := renderSQL(t, res.Query); strings.Contains(got, "tenant_id") {
		t.Errorf("leak: a scoping conjunct was woven anyway:\n%s", got)
	}
}
