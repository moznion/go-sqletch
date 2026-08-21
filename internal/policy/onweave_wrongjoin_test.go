package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// D2a soundness: the ON-clause weave is only correct when the located
// ON belongs to the join that null-extends THIS occurrence — i.e.
// gating that ON actually removes the designated table's own rows.
// When it does not (a FULL join preserves both sides; a table on the
// preserved side of its own join is null-extended farther out), the
// weaver must REFUSE with SQLETCH125 rather than weave a conjunct that
// silently leaks every other-tenant row.
func TestWeaveON_WrongJoinRefuses(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			// FULL JOIN preserves BOTH operands: the designated table's
			// rows survive whether or not the ON matches, so an ON
			// conjunct cannot scope them. (Leak repro 1.)
			name: "FULL JOIN, designated is the right operand",
			src:  "-- name: Q :many\nSELECT u.id FROM users u FULL JOIN orders o ON o.user_id = u.id WHERE u.ok\n",
		},
		{
			// FULL JOIN with the designated table as the left operand:
			// still preserved, still a leak.
			name: "FULL JOIN, designated is the left operand",
			src:  "-- name: Q :many\nSELECT u.id FROM orders o FULL JOIN users u ON o.user_id = u.id WHERE u.ok\n",
		},
		{
			// orders is preserved by its own LEFT JOIN but null-extended
			// by the enclosing RIGHT JOIN; the conjunct would land in the
			// LEFT JOIN's ON, which only gates whether `users` matches —
			// every all-tenant `orders` row survives. (Leak repro 2.)
			name: "nested: preserved side of own join, null-extended farther out",
			src:  "-- name: Q :many\nSELECT v.id FROM orders o LEFT JOIN users u ON o.uid = u.id RIGHT JOIN vendors v ON v.oid = o.id\n",
		},
		{
			// Same leak, expressed with an explicit parenthesized group:
			// orders is the left operand of the inner LEFT JOIN, and the
			// whole group is null-extended by the outer LEFT JOIN.
			name: "nested parenthesized: designated on the preserved inner side",
			src:  "-- name: Q :many\nSELECT v.id FROM vendors v LEFT JOIN (orders o LEFT JOIN users u ON o.uid = u.id) ON v.oid = o.id\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := weaveOne(t, tc.src, tenantPolicy())
			if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
				t.Fatalf("want exactly one SQLETCH125, got %+v", res.Diags)
			}
			// And nothing was woven: the template is unchanged.
			if got := renderSQL(t, res.Query); strings.Contains(got, "tenant_id") {
				t.Errorf("leak: a scoping conjunct was woven anyway:\n%s", got)
			}
		})
	}
}

// The enforcement pass must re-derive the same decision independently:
// a hand-written scoping conjunct placed in a non-gating ON does NOT
// satisfy the invariant, so SQLETCH124 fires. (Weaver-regression
// backstop; a correct weaver already refuses these at weave time.)
func TestEnforceON_WrongJoinRejected(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "FULL JOIN ON conjunct does not scope the preserved table",
			src:  "-- name: Q :many\nSELECT u.id FROM users u FULL JOIN orders o ON o.user_id = u.id AND o.tenant_id = :tenant_id WHERE u.ok\n",
		},
		{
			name: "conjunct in the non-null-extending inner ON",
			src:  "-- name: Q :many\nSELECT v.id FROM orders o LEFT JOIN users u ON o.uid = u.id AND o.tenant_id = :tenant_id RIGHT JOIN vendors v ON v.oid = o.id\n",
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

// Regression guard: the two LEGITIMATE null-extending cases must keep
// weaving into the ON — the designated table IS the null-extended
// operand of the located join (a simple LEFT JOIN's right operand, and
// a RIGHT JOIN's left operand).
func TestWeaveON_LegitimateNullExtensionStillWeaves(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "LEFT JOIN right operand (the canonical D2a case)",
			src:  "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE u.ok\n",
			want: "SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND (o.tenant_id = $1) WHERE u.ok",
		},
		{
			name: "RIGHT JOIN left operand",
			src:  "-- name: Q :many\nSELECT u.id FROM orders o RIGHT JOIN users u ON o.user_id = u.id WHERE u.ok\n",
			want: "SELECT u.id FROM orders o RIGHT JOIN users u ON o.user_id = u.id AND (o.tenant_id = $1) WHERE u.ok",
		},
		{
			// Proven-equivalent inner crossing: the designated table sits
			// in an inner-join group that is later null-extended; scoping
			// the inner ON physically removes its rows before the group is
			// null-extended, so it is safe to weave there.
			name: "inner-join group null-extended farther out",
			src:  "-- name: Q :many\nSELECT v.id FROM orders o JOIN users u ON o.uid = u.id RIGHT JOIN vendors v ON v.oid = o.id\n",
			want: "SELECT v.id FROM orders o JOIN users u ON o.uid = u.id AND (o.tenant_id = $1) RIGHT JOIN vendors v ON v.oid = o.id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := weaveOne(t, tc.src, tenantPolicy())
			noDiags(t, res)
			if got := renderSQL(t, res.Query); got != tc.want {
				t.Errorf("woven rendering:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}
