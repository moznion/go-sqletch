package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// Nested-core weaving (design 14 §14): on PostgreSQL, the body of a
// non-recursive CTE and a FROM-position subquery are scoped at their own
// WHERE (or their own join's ON) like a set-operation branch. Each
// golden output must also satisfy Enforce.
func TestWeave_Nested_Golden(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "CTE body without WHERE",
			body: "WITH x AS (SELECT id FROM orders o) SELECT id FROM x",
			want: "WITH x AS (SELECT id FROM orders o WHERE (o.tenant_id = $1)) SELECT id FROM x",
		},
		{
			name: "CTE body with WHERE and a tail",
			body: "WITH x AS (SELECT id FROM orders WHERE ok ORDER BY id LIMIT 3) SELECT id FROM x WHERE id > 1",
			want: "WITH x AS (SELECT id FROM orders WHERE (orders.tenant_id = $1) AND ok ORDER BY id LIMIT 3) SELECT id FROM x WHERE id > 1",
		},
		{
			name: "set operation inside a CTE body, filtered once outside",
			body: "WITH a AS (SELECT o.id, o.created_at FROM orders o UNION ALL SELECT i.id, i.created_at FROM order_items i UNION ALL SELECT u.id, u.created_at FROM users u) " +
				"SELECT id FROM a WHERE id > :min ORDER BY created_at LIMIT 10",
			want: "WITH a AS (SELECT o.id, o.created_at FROM orders o WHERE (o.tenant_id = $1) UNION ALL SELECT i.id, i.created_at FROM order_items i WHERE (i.tenant_id = $1) UNION ALL SELECT u.id, u.created_at FROM users u) " +
				"SELECT id FROM a WHERE id > $2 ORDER BY created_at LIMIT 10",
		},
		{
			name: "FROM subquery",
			body: "SELECT d.id FROM (SELECT id FROM orders) d JOIN users u ON u.id = d.id",
			want: "SELECT d.id FROM (SELECT id FROM orders WHERE (orders.tenant_id = $1)) d JOIN users u ON u.id = d.id",
		},
		{
			name: "LATERAL subquery with a correlated WHERE",
			body: "SELECT u.id, l.n FROM users u CROSS JOIN LATERAL (SELECT count(*) AS n FROM orders o WHERE o.user_id = u.id) l",
			want: "SELECT u.id, l.n FROM users u CROSS JOIN LATERAL (SELECT count(*) AS n FROM orders o WHERE (o.tenant_id = $1) AND o.user_id = u.id) l",
		},
		{
			name: "top-level and nested occurrences are each scoped",
			body: "WITH x AS (SELECT user_id FROM orders) SELECT o.id FROM orders o JOIN x ON x.user_id = o.user_id",
			want: "WITH x AS (SELECT user_id FROM orders WHERE (orders.tenant_id = $1)) SELECT o.id FROM orders o JOIN x ON x.user_id = o.user_id WHERE (o.tenant_id = $1)",
		},
		{
			name: "nested in nested",
			body: "SELECT * FROM (WITH z AS (SELECT id FROM orders) SELECT id FROM z) d",
			want: "SELECT * FROM (WITH z AS (SELECT id FROM orders WHERE (orders.tenant_id = $1)) SELECT id FROM z) d",
		},
		{
			name: "a nested WHERE with a top-level OR is wrapped",
			body: "WITH x AS (SELECT id FROM orders WHERE a OR b) SELECT id FROM x",
			want: "WITH x AS (SELECT id FROM orders WHERE (orders.tenant_id = $1) AND (a OR b)) SELECT id FROM x",
		},
		{
			name: "null-extended occurrence inside a CTE body weaves into that join's ON",
			body: "WITH x AS (SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id) SELECT id FROM x",
			want: "WITH x AS (SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND (o.tenant_id = $1)) SELECT id FROM x",
		},
		{
			name: "derived table on a null-extended side is scoped inside it",
			body: "SELECT u.id FROM users u LEFT JOIN (SELECT user_id FROM orders) d ON d.user_id = u.id",
			want: "SELECT u.id FROM users u LEFT JOIN (SELECT user_id FROM orders WHERE (orders.tenant_id = $1)) d ON d.user_id = u.id",
		},
		{
			name: "idempotence inside a CTE body",
			body: "WITH x AS (SELECT id FROM orders o WHERE o.tenant_id = :tenant_id) SELECT id FROM x",
			want: "WITH x AS (SELECT id FROM orders o WHERE o.tenant_id = $1) SELECT id FROM x",
		},
		{
			name: "the outer WHERE keeps its own sublink tokens",
			body: "WITH x AS (SELECT id FROM orders) SELECT id FROM x WHERE id IN (SELECT id FROM users) OR id = 0",
			want: "WITH x AS (SELECT id FROM orders WHERE (orders.tenant_id = $1)) SELECT id FROM x WHERE id IN (SELECT id FROM users) OR id = 0",
		},
		{
			name: "a set operation's statement-level CTE body",
			body: "WITH x AS (SELECT id FROM orders) SELECT id FROM x UNION SELECT id FROM users",
			want: "WITH x AS (SELECT id FROM orders WHERE (orders.tenant_id = $1)) SELECT id FROM x UNION SELECT id FROM users",
		},
		{
			name: "no designated table anywhere: untouched",
			body: "WITH x AS (SELECT id FROM users) SELECT id FROM (SELECT id FROM x) d",
			want: "WITH x AS (SELECT id FROM users) SELECT id FROM (SELECT id FROM x) d",
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

// A template construct in the outer query does not disturb the nested
// cores: the guarded outer conjunct stays where it is, and the CTE body
// is scoped unconditionally.
func TestWeave_Nested_OuterConstructs(t *testing.T) {
	src := "-- name: Q :many\nWITH x AS (SELECT id, status FROM orders) SELECT id FROM x WHERE id > 0\n" +
		"@if-present(status)\nAND status = :status\n@endif\n"
	res := weaveOne(t, src, tenantPolicy())
	noDiags(t, res)
	want := "WITH x AS (SELECT id, status FROM orders WHERE (orders.tenant_id = $1)) SELECT id FROM x WHERE id > 0\n\nAND (status = $2)"
	if got := renderSQL(t, res.Query); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if diags := enforceOn(t, res.Query, tenantPolicy()); len(diags) != 0 {
		t.Errorf("enforce: %+v", diags)
	}
}

// Positions §14 leaves out stay SQLETCH125.
func TestWeave_Nested_Unweavable(t *testing.T) {
	cases := []struct {
		name, body, wantMsg string
		pol                 Policy
	}{
		{
			name:    "sublink inside a CTE body",
			body:    "WITH x AS (SELECT id FROM users WHERE id IN (SELECT user_id FROM orders)) SELECT id FROM x",
			wantMsg: "subquery",
		},
		{
			name:    "sublink in the outer WHERE of a statement with a CTE",
			body:    "WITH x AS (SELECT id FROM users) SELECT id FROM x WHERE EXISTS (SELECT 1 FROM orders)",
			wantMsg: "subquery",
		},
		{
			name:    "WITH RECURSIVE body",
			body:    "WITH RECURSIVE x AS (SELECT id FROM orders) SELECT id FROM x",
			wantMsg: "subquery",
		},
		{
			name:    "a placeholder-less predicate in a nested core",
			body:    "WITH x AS (SELECT id FROM orders) SELECT id FROM x",
			pol:     Policy{Name: "live", Tables: []string{"orders"}, Predicate: "current_setting('app.live') = 'on'"},
			wantMsg: "without a `{}` placeholder",
		},
		{
			name:    "USING join on a null-extended side inside a FROM subquery",
			body:    "SELECT * FROM (SELECT u.id FROM users u LEFT JOIN orders o USING (user_id)) d",
			wantMsg: "no ON expression",
		},
		{
			name:    "a DML statement's CTE is not a nested core",
			body:    "WITH x AS (SELECT id FROM orders) DELETE FROM users WHERE id IN (SELECT id FROM x)",
			wantMsg: "subquery",
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
// WHERE never vouches for a CTE body's read, nor the reverse.
func TestEnforce_Nested_PerCore(t *testing.T) {
	cases := []struct {
		name, body string
		wantDiag   bool
	}{
		{
			name:     "CTE body scoped by hand",
			body:     "WITH x AS (SELECT o.id, o.tenant_id FROM orders o WHERE o.tenant_id = :tenant_id) SELECT id FROM x",
			wantDiag: false,
		},
		{
			name:     "CTE body unscoped",
			body:     "WITH x AS (SELECT id FROM orders) SELECT id FROM x",
			wantDiag: true,
		},
		{
			name:     "outer conjunct sharing the alias does not scope the CTE body",
			body:     "WITH o AS (SELECT id, tenant_id FROM orders) SELECT o.id FROM o WHERE o.tenant_id = :tenant_id",
			wantDiag: true,
		},
		{
			name:     "CTE conjunct does not scope an outer read",
			body:     "WITH x AS (SELECT id FROM orders o WHERE o.tenant_id = :tenant_id) SELECT o.id FROM orders o JOIN x ON x.id = o.id",
			wantDiag: true,
		},
		{
			name:     "FROM subquery unscoped while the outer core is scoped",
			body:     "SELECT o.id FROM orders o JOIN (SELECT id FROM orders) d ON d.id = o.id WHERE o.tenant_id = :tenant_id",
			wantDiag: true,
		},
		{
			name:     "conjunct inside a sublink of the CTE body does not count",
			body:     "WITH x AS (SELECT o.id FROM orders o WHERE o.id IN (SELECT id FROM users u WHERE o.tenant_id = :tenant_id)) SELECT id FROM x",
			wantDiag: true,
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

// SQLite reports no nested cores: a designated read in a CTE body or
// FROM subquery stays refused (design 14 §14, PostgreSQL only).
func TestWeave_Nested_SQLiteStillRefused(t *testing.T) {
	for _, body := range []string{
		"WITH x AS (SELECT id FROM orders) SELECT id FROM x",
		"SELECT d.id FROM (SELECT id FROM orders) d",
	} {
		q := sqliteKit.scan(t, body)
		res := Weave(sqliteKit.profile, sqliteKit.fe, []Policy{literalTenant()}, q)
		if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
			t.Errorf("%q: diags = %+v, want exactly one SQLETCH125", body, res.Diags)
		}
	}
}
