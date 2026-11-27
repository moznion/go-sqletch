package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// A designated relation inside an aliased join group `(… JOIN …) AS j`
// is hidden by the group's alias, so a conjunct qualified with its name
// resolves to a same-named relation of an ENCLOSING query (a correlated
// sublink, a LATERAL subquery) — leaving it unscoped, proven on real
// PostgreSQL by TestPolicyWeave_JoinAliasHidden_Counterexample — or
// fails. Design 14 §11.11: refused everywhere a `{}` predicate binds to
// it, whichever slot the conjunct would take.
func TestWeave_JoinAliasHidden_Refused(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"correlated sublink, outer same-named relation", "SELECT o.id FROM orders o WHERE EXISTS (SELECT 1 FROM (orders o JOIN users u ON u.id = o.uid) AS j WHERE j.x = 1)"},
		{"sublink, alias without AS", "SELECT o.id FROM orders o WHERE EXISTS (SELECT 1 FROM (orders o JOIN users u ON u.id = o.uid) j)"},
		{"sublink, unaliased member", "SELECT orders.id FROM orders WHERE EXISTS (SELECT 1 FROM (orders JOIN users u ON u.id = orders.uid) AS j)"},
		{"LATERAL subquery", "SELECT o.id FROM orders o, LATERAL (SELECT 1 FROM (orders o JOIN users u ON u.id = o.uid) AS j) l"},
		{"CTE body", "WITH x AS (SELECT 1 FROM (orders o JOIN users u ON u.id = o.uid) AS j) SELECT * FROM x"},
		{"top level", "SELECT j.uid FROM (orders o JOIN users u ON u.id = o.uid) AS j"},
		{"group nested in an aliased group", "SELECT 1 FROM ((orders o JOIN users u ON true) JOIN acc a ON true) AS j"},
		{"null-extended side inside the group", "SELECT 1 FROM acc a LEFT JOIN (users u LEFT JOIN orders o ON o.uid = u.id) AS j ON j.id = a.id"},
		{"group on a null-extended side", "SELECT o.id FROM orders o WHERE EXISTS (SELECT 1 FROM users u LEFT JOIN (orders o JOIN acc a ON a.id = o.aid) AS j ON j.uid = u.id)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "-- name: Q :many\n" + tc.body + "\n"
			q := scanOne(t, src)
			res := weaveOne(t, src, tenantPolicy())
			if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
				t.Fatalf("diags = %+v, want exactly one SQLETCH125", res.Diags)
			}
			if !strings.Contains(res.Diags[0].Message, "join group with its own alias") {
				t.Errorf("message %q does not state the join-alias rule", res.Diags[0].Message)
			}
			if !strings.Contains(res.Diags[0].Hint, "drop the join group's alias") {
				t.Errorf("hint %q does not show the rewrite", res.Diags[0].Hint)
			}
			if renderSQL(t, res.Query) != renderSQL(t, q) {
				t.Errorf("rejected query must not be woven")
			}
		})
	}
}

// What the rule leaves alone: an unaliased group, a USING alias (which
// hides no table name), and a non-designated relation in an aliased
// group.
func TestWeave_JoinAliasHidden_Unaffected(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{
			"unaliased group",
			"SELECT 1 FROM acc a JOIN (orders o JOIN users u ON u.id = o.uid) ON a.id = o.aid",
			"SELECT 1 FROM acc a JOIN (orders o JOIN users u ON u.id = o.uid) ON a.id = o.aid WHERE (o.tenant_id = $1)",
		},
		{
			"USING alias",
			"SELECT o.id FROM orders o JOIN users u USING (id) AS x",
			"SELECT o.id FROM orders o JOIN users u USING (id) AS x WHERE (o.tenant_id = $1)",
		},
		{
			"designated table outside the aliased group",
			"SELECT o.id FROM orders o JOIN (users u JOIN acc a ON a.id = u.aid) AS j ON j.id = o.uid",
			"SELECT o.id FROM orders o JOIN (users u JOIN acc a ON a.id = u.aid) AS j ON j.id = o.uid WHERE (o.tenant_id = $1)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := weaveOne(t, "-- name: Q :many\n"+tc.body+"\n", tenantPolicy())
			noDiags(t, res)
			if got := renderSQL(t, res.Query); got != tc.want {
				t.Errorf("woven rendering:\n got: %s\nwant: %s", got, tc.want)
			}
			if diags := enforceOn(t, res.Query, tenantPolicy()); len(diags) != 0 {
				t.Errorf("enforcement rejects the weaver's own output: %+v", diags)
			}
		})
	}
}

// Enforcement never credits a conjunct bound to a hidden name, even one
// written by hand exactly as the weaver would have spliced it (that
// conjunct reads the OUTER relation).
func TestEnforce_JoinAliasHidden_NeverCredited(t *testing.T) {
	for _, body := range []string{
		"SELECT o.id FROM orders o WHERE o.tenant_id = :tenant_id AND EXISTS (SELECT 1 FROM (orders o JOIN users u ON u.id = o.uid) AS j WHERE o.tenant_id = :tenant_id)",
		"SELECT o.id FROM orders o, LATERAL (SELECT 1 FROM (orders o JOIN users u ON u.id = o.uid) AS j WHERE o.tenant_id = :tenant_id) l WHERE o.tenant_id = :tenant_id",
	} {
		q := scanOne(t, "-- name: Q :many\n"+body+"\n")
		diags := enforceOn(t, q, tenantPolicy())
		if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped || !strings.Contains(diags[0].Message, "join group with its own alias") {
			t.Errorf("%q: diags = %+v, want exactly one SQLETCH124 naming the join-alias rule", body, diags)
		}
	}
}
