package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// Set-operation branch weaving (design 14 §13): every leaf core of a
// statement-level UNION/INTERSECT/EXCEPT is scoped independently —
// its own WHERE (synthesized when absent), or its own join's ON for a
// null-extended occurrence — exactly as if it were a top-level
// statement. Each golden output must also satisfy Enforce.
func TestWeave_SetOp_Golden(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "designated table in the first branch only",
			body: "SELECT o.id FROM orders o WHERE o.status = :status UNION ALL SELECT a.id FROM archive a",
			want: "SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND o.status = $2 UNION ALL SELECT a.id FROM archive a",
		},
		{
			name: "designated table in the last branch only, no WHERE, set-level ORDER BY",
			body: "SELECT a.id FROM archive a UNION SELECT o.id FROM orders o ORDER BY 1",
			want: "SELECT a.id FROM archive a UNION SELECT o.id FROM orders o WHERE (o.tenant_id = $1) ORDER BY 1",
		},
		{
			name: "every branch scoped, WHERE synthesized mid-statement",
			body: "SELECT id FROM orders UNION SELECT id FROM orders WHERE ok INTERSECT SELECT order_id FROM order_items",
			want: "SELECT id FROM orders WHERE (orders.tenant_id = $1) UNION SELECT id FROM orders WHERE (orders.tenant_id = $1) AND ok INTERSECT SELECT order_id FROM order_items WHERE (order_items.tenant_id = $1)",
		},
		{
			name: "EXCEPT right operand is scoped like any other read",
			body: "SELECT id FROM users EXCEPT SELECT user_id FROM orders",
			want: "SELECT id FROM users EXCEPT SELECT user_id FROM orders WHERE (orders.tenant_id = $1)",
		},
		{
			name: "parenthesized operands: before the operand's own LIMIT, before its closing paren",
			body: "(SELECT id FROM orders o LIMIT 5) UNION ALL (SELECT id FROM order_items i) ORDER BY 1",
			want: "(SELECT id FROM orders o WHERE (o.tenant_id = $1) LIMIT 5) UNION ALL (SELECT id FROM order_items i WHERE (i.tenant_id = $1)) ORDER BY 1",
		},
		{
			name: "nested operands",
			body: "SELECT id FROM users UNION (SELECT id FROM orders UNION SELECT id FROM archive) EXCEPT SELECT id FROM order_items WHERE ok",
			want: "SELECT id FROM users UNION (SELECT id FROM orders WHERE (orders.tenant_id = $1) UNION SELECT id FROM archive) EXCEPT SELECT id FROM order_items WHERE (order_items.tenant_id = $1) AND ok",
		},
		{
			name: "a branch WHERE with a top-level OR is wrapped in that branch only",
			body: "SELECT id FROM orders WHERE a OR b UNION SELECT id FROM orders WHERE c",
			want: "SELECT id FROM orders WHERE (orders.tenant_id = $1) AND (a OR b) UNION SELECT id FROM orders WHERE (orders.tenant_id = $1) AND c",
		},
		{
			name: "same alias in two branches: each branch binds its own",
			body: "SELECT o.id FROM archive o UNION SELECT o.id FROM orders o WHERE o.x = 1",
			want: "SELECT o.id FROM archive o UNION SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND o.x = 1",
		},
		{
			name: "null-extended occurrence inside a branch weaves into that join's ON",
			body: "SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id UNION SELECT id FROM archive",
			want: "SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND (o.tenant_id = $1) UNION SELECT id FROM archive",
		},
		{
			name: "a statement-level WITH that reads no designated table is fine",
			body: "WITH x AS (SELECT 1 AS id) SELECT id FROM x UNION SELECT id FROM orders",
			want: "WITH x AS (SELECT 1 AS id) SELECT id FROM x UNION SELECT id FROM orders WHERE (orders.tenant_id = $1)",
		},
		{
			name: "idempotence per branch: only the unscoped branch is woven",
			body: "SELECT id FROM orders UNION SELECT id FROM orders WHERE orders.tenant_id = :tenant_id",
			want: "SELECT id FROM orders WHERE (orders.tenant_id = $1) UNION SELECT id FROM orders WHERE orders.tenant_id = $1",
		},
		{
			name: "no designated table anywhere: untouched",
			body: "SELECT id FROM users UNION SELECT id FROM archive",
			want: "SELECT id FROM users UNION SELECT id FROM archive",
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

// A predicate without a `{}` placeholder is one WHERE conjunct per
// branch that reads a designated table (it references no joined
// columns, so it never needs an ON clause).
func TestWeave_SetOp_NoPlaceholderPredicate(t *testing.T) {
	pol := Policy{Name: "live", Tables: []string{"orders"}, Predicate: "current_setting('app.live') = 'on'"}
	res := weaveOne(t, "-- name: Q :many\nSELECT id FROM orders UNION SELECT id FROM users UNION SELECT id FROM orders o\n", pol)
	noDiags(t, res)
	want := "SELECT id FROM orders WHERE (current_setting('app.live') = 'on') UNION SELECT id FROM users UNION SELECT id FROM orders o WHERE (current_setting('app.live') = 'on')"
	if got := renderSQL(t, res.Query); got != want {
		t.Errorf("got: %s\nwant: %s", got, want)
	}
	if len(res.Woven) != 1 || len(res.Woven[0].Conjuncts) != 2 {
		t.Errorf("Woven = %+v, want one policy with two conjunct records", res.Woven)
	}
	if diags := enforceOn(t, res.Query, pol); len(diags) != 0 {
		t.Errorf("enforce: %+v", diags)
	}
}

// Positions the branch weave still cannot scope stay SQLETCH125.
func TestWeave_SetOp_Unweavable(t *testing.T) {
	cases := []struct {
		name, body, wantMsg string
	}{
		{
			name:    "subquery inside a branch",
			body:    "SELECT id FROM users WHERE id IN (SELECT user_id FROM orders) UNION SELECT id FROM archive",
			wantMsg: "subquery",
		},
		{
			name:    "statement-level CTE body",
			body:    "WITH x AS (SELECT id FROM orders) SELECT id FROM x UNION SELECT id FROM users",
			wantMsg: "subquery",
		},
		{
			name:    "subquery in the set-level ORDER BY",
			body:    "SELECT id FROM users UNION SELECT id FROM archive ORDER BY (SELECT max(id) FROM orders)",
			wantMsg: "subquery",
		},
		{
			name:    "a TABLE operand cannot carry a WHERE clause",
			body:    "SELECT id FROM users UNION TABLE orders",
			wantMsg: "TABLE",
		},
		{
			name:    "guarded join inside a branch",
			body:    "SELECT u.id FROM users u\n@if-present(with_orders)\nJOIN orders o ON o.user_id = u.id AND :with_orders\n@endif\nUNION SELECT id FROM archive",
			wantMsg: "guarded",
		},
		{
			name:    "USING join on a null-extended side inside a branch",
			body:    "SELECT u.id FROM users u LEFT JOIN orders o USING (user_id) UNION SELECT id FROM archive",
			wantMsg: "no ON expression",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := scanOne(t, "-- name: Q :many\n"+tc.body+"\n")
			res := weaveOne(t, "-- name: Q :many\n"+tc.body+"\n", tenantPolicy())
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

// Enforcement re-derives presence per branch: a conjunct in one branch
// never vouches for another branch's read — the pre-§13 SQLite leak
// shape (the WHERE of a DIFFERENT branch sharing the alias) included.
func TestEnforce_SetOp_PerBranch(t *testing.T) {
	cases := []struct {
		name, body string
		wantDiag   bool
	}{
		{
			name:     "every designated branch scoped by hand",
			body:     "SELECT o.id FROM orders o WHERE o.tenant_id = :tenant_id UNION SELECT id FROM users",
			wantDiag: false,
		},
		{
			name:     "second branch unscoped",
			body:     "SELECT id FROM orders WHERE orders.tenant_id = :tenant_id UNION SELECT id FROM orders",
			wantDiag: true,
		},
		{
			name:     "conjunct in a different branch sharing the alias",
			body:     "SELECT o.id FROM orders o UNION SELECT o.id FROM archive o WHERE o.tenant_id = :tenant_id",
			wantDiag: true,
		},
		{
			name:     "conjunct under a top-level OR of the branch does not count",
			body:     "SELECT o.id FROM orders o WHERE o.tenant_id = :tenant_id OR o.public UNION SELECT 1",
			wantDiag: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := scanOne(t, "-- name: Q :many\n"+tc.body+"\n")
			diags := enforceOn(t, q, tenantPolicy())
			got := len(diags) == 1 && diags[0].Code == diagnostics.CodePolicyUnscoped
			if got != tc.wantDiag || (!tc.wantDiag && len(diags) != 0) {
				t.Errorf("diags = %+v, want SQLETCH124=%v", diags, tc.wantDiag)
			}
		})
	}
}
