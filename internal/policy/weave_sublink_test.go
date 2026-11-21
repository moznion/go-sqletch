package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// Sublink weaving, phase 1 (design 14 §15): on PostgreSQL, the body of
// an EXISTS / IN / op ANY / op ALL / scalar subquery in the WHERE or
// HAVING of an eligible core of a SELECT is scoped at its own WHERE (or
// its own join's ON), recursively. Each golden output must also satisfy
// Enforce.
func TestWeave_Sublink_Golden(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "IN",
			body: "SELECT id FROM reports WHERE order_id IN (SELECT id FROM orders)",
			want: "SELECT id FROM reports WHERE order_id IN (SELECT id FROM orders WHERE (orders.tenant_id = $1))",
		},
		{
			name: "NOT IN with an inner WHERE",
			body: "SELECT u.id FROM users u WHERE u.id NOT IN (SELECT o.user_id FROM orders o WHERE o.ok)",
			want: "SELECT u.id FROM users u WHERE u.id NOT IN (SELECT o.user_id FROM orders o WHERE (o.tenant_id = $1) AND o.ok)",
		},
		{
			name: "correlated NOT EXISTS, outer core designated too",
			body: "SELECT o.id FROM orders o WHERE NOT EXISTS (SELECT 1 FROM order_items i WHERE i.order_id = o.id)",
			want: "SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND NOT EXISTS (SELECT 1 FROM order_items i WHERE (i.tenant_id = $1) AND i.order_id = o.id)",
		},
		{
			name: "op ANY",
			body: "SELECT a.id FROM acc a WHERE a.lim = ANY (SELECT total FROM orders)",
			want: "SELECT a.id FROM acc a WHERE a.lim = ANY (SELECT total FROM orders WHERE (orders.tenant_id = $1))",
		},
		{
			name: "op ALL",
			body: "SELECT a.id FROM acc a WHERE a.lim >= ALL (SELECT total FROM orders)",
			want: "SELECT a.id FROM acc a WHERE a.lim >= ALL (SELECT total FROM orders WHERE (orders.tenant_id = $1))",
		},
		{
			name: "scalar subquery with tail clauses",
			body: "SELECT o.id FROM orders o WHERE o.total > (SELECT total FROM orders ORDER BY created_at DESC LIMIT 1)",
			want: "SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND o.total > (SELECT total FROM orders WHERE (orders.tenant_id = $1) ORDER BY created_at DESC LIMIT 1)",
		},
		{
			name: "row comparison against a subquery (a scalar sublink in the raw tree)",
			body: "SELECT a.id FROM acc a WHERE (a.x, a.y) = (SELECT id, id FROM orders)",
			want: "SELECT a.id FROM acc a WHERE (a.x, a.y) = (SELECT id, id FROM orders WHERE (orders.tenant_id = $1))",
		},
		{
			name: "HAVING sublink",
			body: "SELECT a.kind, count(*) FROM acc a GROUP BY a.kind HAVING count(*) > (SELECT count(*) FROM orders)",
			want: "SELECT a.kind, count(*) FROM acc a GROUP BY a.kind HAVING count(*) > (SELECT count(*) FROM orders WHERE (orders.tenant_id = $1))",
		},
		{
			name: "set operation inside IN, inside a CTE body",
			body: "WITH x AS (SELECT id FROM acc WHERE id IN (SELECT user_id FROM orders UNION SELECT order_id FROM order_items)) SELECT id FROM x",
			want: "WITH x AS (SELECT id FROM acc WHERE id IN (SELECT user_id FROM orders WHERE (orders.tenant_id = $1) UNION SELECT order_id FROM order_items WHERE (order_items.tenant_id = $1))) SELECT id FROM x",
		},
		{
			name: "OR around a sublink",
			body: "SELECT o.id FROM orders o WHERE o.flag OR o.id IN (SELECT order_id FROM order_items)",
			want: "SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND (o.flag OR o.id IN (SELECT order_id FROM order_items WHERE (order_items.tenant_id = $1)))",
		},
		{
			name: "sublink inside a set-operation branch",
			body: "SELECT id FROM archive WHERE id IN (SELECT id FROM orders) UNION SELECT id FROM users",
			want: "SELECT id FROM archive WHERE id IN (SELECT id FROM orders WHERE (orders.tenant_id = $1)) UNION SELECT id FROM users",
		},
		{
			name: "sublink inside a sublink",
			body: "SELECT id FROM acc WHERE EXISTS (SELECT 1 FROM users u WHERE u.id IN (SELECT user_id FROM orders))",
			want: "SELECT id FROM acc WHERE EXISTS (SELECT 1 FROM users u WHERE u.id IN (SELECT user_id FROM orders WHERE (orders.tenant_id = $1)))",
		},
		{
			name: "sublink inside CASE",
			body: "SELECT id FROM acc WHERE CASE WHEN ok THEN id IN (SELECT user_id FROM orders) ELSE false END",
			want: "SELECT id FROM acc WHERE CASE WHEN ok THEN id IN (SELECT user_id FROM orders WHERE (orders.tenant_id = $1)) ELSE false END",
		},
		{
			name: "sublink as a function argument",
			body: "SELECT id FROM acc WHERE coalesce((SELECT max(id) FROM orders), 0) > id",
			want: "SELECT id FROM acc WHERE coalesce((SELECT max(id) FROM orders WHERE (orders.tenant_id = $1)), 0) > id",
		},
		{
			name: "parenthesized sublink body",
			body: "SELECT id FROM acc WHERE id IN ((SELECT user_id FROM orders))",
			want: "SELECT id FROM acc WHERE id IN ((SELECT user_id FROM orders WHERE (orders.tenant_id = $1)))",
		},
		{
			name: "null-extended side inside a sublink: ON-weave",
			body: "SELECT id FROM acc WHERE EXISTS (SELECT 1 FROM users u LEFT JOIN orders o ON o.user_id = u.id)",
			want: "SELECT id FROM acc WHERE EXISTS (SELECT 1 FROM users u LEFT JOIN orders o ON o.user_id = u.id AND (o.tenant_id = $1))",
		},
		{
			name: "hand-scoped sublink is not double-woven",
			body: "SELECT id FROM acc WHERE id IN (SELECT o.user_id FROM orders o WHERE o.tenant_id = :tenant_id)",
			want: "SELECT id FROM acc WHERE id IN (SELECT o.user_id FROM orders o WHERE o.tenant_id = $1)",
		},
		{
			name: "FROM subquery inside a sublink",
			body: "SELECT id FROM acc WHERE EXISTS (SELECT 1 FROM (SELECT id FROM orders) d)",
			want: "SELECT id FROM acc WHERE EXISTS (SELECT 1 FROM (SELECT id FROM orders WHERE (orders.tenant_id = $1)) d)",
		},
		{
			name: "CTE inside a sublink",
			body: "SELECT id FROM acc WHERE EXISTS (WITH z AS (SELECT id FROM orders) SELECT 1 FROM z)",
			want: "SELECT id FROM acc WHERE EXISTS (WITH z AS (SELECT id FROM orders WHERE (orders.tenant_id = $1)) SELECT 1 FROM z)",
		},
		{
			name: "sublink as the left operand of IN",
			body: "SELECT id FROM acc WHERE (SELECT max(id) FROM orders) IN (SELECT order_id FROM order_items)",
			want: "SELECT id FROM acc WHERE (SELECT max(id) FROM orders WHERE (orders.tenant_id = $1)) IN (SELECT order_id FROM order_items WHERE (order_items.tenant_id = $1))",
		},
		{
			name: "no designated table in the sublink: untouched",
			body: "SELECT id FROM acc WHERE id IN (SELECT id FROM users)",
			want: "SELECT id FROM acc WHERE id IN (SELECT id FROM users)",
		},
	}
	for _, tc := range cases {
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

// Positions §15 leaves out stay SQLETCH125.
func TestWeave_Sublink_Unweavable(t *testing.T) {
	cases := []struct {
		name, body, wantMsg string
		pol                 Policy
	}{
		{name: "SELECT-list sublink", body: "SELECT u.id, (SELECT count(*) FROM orders o WHERE o.user_id = u.id) FROM users u", wantMsg: "outside a WHERE/HAVING"},
		{name: "ORDER BY sublink", body: "SELECT a.id FROM acc a ORDER BY (SELECT max(id) FROM orders)", wantMsg: "outside a WHERE/HAVING"},
		{name: "ON sublink", body: "SELECT a.id FROM acc a JOIN users u ON u.id IN (SELECT user_id FROM orders)", wantMsg: "outside a WHERE/HAVING"},
		{name: "ARRAY sublink", body: "SELECT a.id FROM acc a WHERE a.ids @> ARRAY(SELECT id FROM orders)", wantMsg: "ARRAY"},
		{name: "sublink of a DELETE", body: "DELETE FROM acc WHERE id IN (SELECT user_id FROM orders)", wantMsg: "statement kind"},
		{name: "sublink of an UPDATE", body: "UPDATE acc SET n = 1 WHERE id IN (SELECT user_id FROM orders)", wantMsg: "statement kind"},
		{name: "WITH RECURSIVE inside a sublink", body: "SELECT id FROM acc WHERE EXISTS (WITH RECURSIVE z AS (SELECT id FROM orders) SELECT 1 FROM z)", wantMsg: "WITH RECURSIVE"},
		{name: "TABLE operand inside a sublink", body: "SELECT id FROM acc WHERE EXISTS (TABLE orders)", wantMsg: "TABLE operand"},
		{
			name:    "{}-less predicate in a sublink",
			body:    "SELECT id FROM acc WHERE EXISTS (SELECT 1 FROM orders)",
			wantMsg: "without a `{}` placeholder",
			pol:     Policy{Name: "tenant_scope", Tables: []string{"orders"}, Predicate: "tenant_id = :tenant_id", ParamName: "tenant_id"},
		},
		{
			name:    "sublink inside an @if-present body",
			body:    "SELECT id FROM acc WHERE id > 0\n@if-present(tag)\nAND id IN (SELECT user_id FROM orders WHERE tag = :tag)\n@endif",
			wantMsg: "template construct body",
		},
		{
			name:    "sublink inside a @when body",
			body:    "SELECT id FROM acc WHERE id > 0\n@when(mode = 'own')\nAND id IN (SELECT user_id FROM orders)\n@end",
			wantMsg: "template construct body",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pol := tc.pol
			if pol.Name == "" {
				pol = tenantPolicy()
			}
			q := scanOne(t, "-- name: Q :many\n"+tc.body+"\n")
			res := weaveOne(t, "-- name: Q :many\n"+tc.body+"\n", pol)
			if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
				t.Fatalf("diags = %+v, want exactly one SQLETCH125", res.Diags)
			}
			if !strings.Contains(res.Diags[0].Message, tc.wantMsg) {
				t.Errorf("message %q does not mention %q", res.Diags[0].Message, tc.wantMsg)
			}
			if renderSQL(t, res.Query) != renderSQL(t, q) {
				t.Errorf("rejected query must not be woven")
			}
		})
	}
}

// Enforcement re-derives presence per core: a conjunct in the outer
// WHERE never vouches for a sublink's read, nor the reverse.
func TestEnforce_Sublink_PerCore(t *testing.T) {
	cases := []struct {
		name, body string
		wantDiag   bool
	}{
		{
			name:     "sublink scoped by hand",
			body:     "SELECT id FROM acc WHERE id IN (SELECT o.user_id FROM orders o WHERE o.tenant_id = :tenant_id)",
			wantDiag: false,
		},
		{
			name:     "sublink unscoped",
			body:     "SELECT id FROM acc WHERE id IN (SELECT user_id FROM orders)",
			wantDiag: true,
		},
		{
			name:     "outer conjunct does not scope a sublink read",
			body:     "SELECT o.id FROM orders o WHERE o.tenant_id = :tenant_id AND EXISTS (SELECT 1 FROM order_items i WHERE i.order_id = o.id)",
			wantDiag: true,
		},
		{
			name:     "correlated outer-alias conjunct inside a sublink does not scope the sublink read",
			body:     "SELECT o.id FROM orders o WHERE o.tenant_id = :tenant_id AND EXISTS (SELECT 1 FROM order_items i WHERE o.tenant_id = :tenant_id)",
			wantDiag: true,
		},
		{
			name:     "sublink conjunct does not scope the outer read",
			body:     "SELECT o.id FROM orders o WHERE EXISTS (SELECT 1 FROM order_items i WHERE i.tenant_id = :tenant_id AND i.order_id = o.id)",
			wantDiag: true,
		},
		{
			name:     "both scoped by hand",
			body:     "SELECT o.id FROM orders o WHERE o.tenant_id = :tenant_id AND EXISTS (SELECT 1 FROM order_items i WHERE i.tenant_id = :tenant_id AND i.order_id = o.id)",
			wantDiag: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := scanOne(t, "-- name: Q :many\n"+tc.body+"\n")
			diags := enforceOn(t, q, tenantPolicy())
			if tc.wantDiag {
				if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
					t.Fatalf("want exactly one SQLETCH124, got %+v", diags)
				}
				return
			}
			if len(diags) != 0 {
				t.Fatalf("unexpected diagnostics: %+v", diags)
			}
		})
	}
}

// SQLite reports no nested cores: a designated read in a sublink stays
// refused (design 14 §15, PostgreSQL only).
func TestWeave_Sublink_SQLiteStillRefused(t *testing.T) {
	for _, body := range []string{
		"SELECT id FROM acc WHERE id IN (SELECT user_id FROM orders)",
		"SELECT id FROM acc WHERE EXISTS (SELECT 1 FROM orders)",
	} {
		q := sqliteKit.scan(t, body)
		res := Weave(sqliteKit.profile, sqliteKit.fe, []Policy{literalTenant()}, q)
		if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
			t.Errorf("%q: diags = %+v, want exactly one SQLETCH125", body, res.Diags)
		}
	}
}
