package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// D2a soundness, ON-less-CROSS refinement: when the designated table's
// OWN introducing join is a CROSS JOIN (which has no ON clause), the
// forward scan skips past it to a LATER enclosing join's ON. That ON
// belongs to a different join whose preserve/null-extend relationship
// to the designated table is not modeled — weaving a scoping conjunct
// there gates nothing and silently leaks every other tenant's rows.
// The weaver must REFUSE (SQLETCH125) rather than weave the leak.
//
// The leak is asymmetric: it needs the designated table as the LEFT
// operand of the CROSS (so the ON-less CROSS is crossed first and a
// later join's ON is mis-attributed to it).
func TestWeaveON_CrossJoinOnLessRefuses(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			// The verified repro: orders is the LEFT operand of an ON-less
			// CROSS JOIN, and the whole parenthesized group is null-extended
			// by the enclosing LEFT JOIN. The first depth-0 ON belongs to
			// `LEFT JOIN t3`, which gates t3, not orders — every orders row
			// (all tenants) survives.
			name: "designated is the left operand of an ON-less CROSS join",
			src: "-- name: Q :many\nSELECT x.id FROM base x\n" +
				"  LEFT JOIN (orders o CROSS JOIN t2 LEFT JOIN t3 ON t2.y = t3.y) ON x.z = o.z\n",
		},
		{
			// The same shape with the CROSS's right operand also null-
			// extended: still ON-less for orders, still a leak.
			name: "ON-less CROSS then INNER join with an ON",
			src: "-- name: Q :many\nSELECT x.id FROM base x\n" +
				"  LEFT JOIN (orders o CROSS JOIN t2 JOIN t3 ON t2.y = t3.y) ON x.z = o.z\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := weaveOne(t, tc.src, tenantPolicy())
			if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
				t.Fatalf("want exactly one SQLETCH125, got %+v", res.Diags)
			}
			// And nothing was woven: no scoping conjunct leaked into the
			// wrong ON.
			if got := renderSQL(t, res.Query); strings.Contains(got, "tenant_id") {
				t.Errorf("leak: a scoping conjunct was woven anyway:\n%s", got)
			}
		})
	}
}

// The enforcement pass must re-derive the same decision independently:
// a hand-written scoping conjunct placed in the mis-attributed ON does
// NOT satisfy the invariant, so SQLETCH124 fires (weaver-regression
// backstop, sharing joinOn/onGates with the weaver).
func TestEnforceON_CrossJoinOnLessRejected(t *testing.T) {
	src := "-- name: Q :many\nSELECT x.id FROM base x\n" +
		"  LEFT JOIN (orders o CROSS JOIN t2 LEFT JOIN t3 ON t2.y = t3.y AND o.tenant_id = :tenant_id) ON x.z = o.z\n"
	q := scanOne(t, src)
	diags := enforceOn(t, q, tenantPolicy())
	if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
		t.Fatalf("want exactly one SQLETCH124, got %+v", diags)
	}
}

// The CROSS's RIGHT operand refuses too, but for the sound reason: the
// designated table's own join is the enclosing ON-less/outer join, so
// there is no gating ON to weave into. This asserts the fix does not
// merely paper over the left-operand case while leaving other CROSS
// arrangements weaving into a wrong ON.
func TestWeaveON_CrossJoinRightOperandRefuses(t *testing.T) {
	src := "-- name: Q :many\nSELECT x.id FROM base x\n" +
		"  LEFT JOIN (t1 CROSS JOIN orders o LEFT JOIN t3 ON t1.y = t3.y) ON x.z = t1.z\n"
	res := weaveOne(t, src, tenantPolicy())
	if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
		t.Fatalf("want exactly one SQLETCH125, got %+v", res.Diags)
	}
	if got := renderSQL(t, res.Query); strings.Contains(got, "tenant_id") {
		t.Errorf("leak: a scoping conjunct was woven anyway:\n%s", got)
	}
}
