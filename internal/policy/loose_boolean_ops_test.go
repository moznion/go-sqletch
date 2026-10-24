package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/template"
)

// Security finding F2/F3 (CWE-783): the clause scanners set hasOR only
// for the token text `OR`, but MySQL has two more boolean operators that
// bind LOOSER than AND — `XOR` and `||` (logical OR under the default
// sql_mode; TiDB grammar: `pipes`/`or` < `xor` < `and`). The weaver then
// took the unwrapped splice, so
//
//	WHERE archived XOR flagged  →  WHERE (tenant) AND archived XOR flagged
//
// parsed as ((tenant) AND archived) XOR flagged: every tenant's
// flagged-but-not-archived rows leaked. Enforce shares the scanner and
// credited it. Both operators are now OR-class: the clause is wrapped,
// and idempotence matching / enforcement crediting are poisoned exactly
// as by a depth-0 OR.

func weaveRender(t *testing.T, profile dialect.LexerProfile, fe dialect.Frontend, src string, pols ...Policy) string {
	t.Helper()
	f, diags := template.NewScanner(profile).ScanFile("t.sql", []byte(src))
	if diagnostics.HasErrors(diags) {
		t.Fatalf("scan %q: %+v", src, diags)
	}
	res := Weave(profile, fe, pols, f.Queries[0])
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("weave %q: %+v", src, res.Diags)
	}
	r, err := ast.Render(profile, res.Query, nil)
	if err != nil {
		t.Fatalf("render %q: %v", src, err)
	}
	return strings.TrimSpace(r.SQL)
}

func TestWeave_MySQL_LooseBooleanOps_WhereWraps(t *testing.T) {
	pol := Policy{Name: "t", Tables: []string{"orders"}, Predicate: "tenant_id = :tid", ParamName: "tid", ParamType: "bigint"}
	cases := []struct{ src, want string }{
		{
			"-- name: Q :many\nSELECT id FROM orders WHERE archived XOR flagged\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (archived XOR flagged)",
		},
		{
			"-- name: Q :many\nSELECT id FROM orders WHERE archived xor flagged LIMIT 5\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (archived xor flagged) LIMIT 5",
		},
		{
			"-- name: Q :many\nSELECT id FROM orders WHERE a = 1 || b = 1\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (a = 1 || b = 1)",
		},
		{
			// No spaces: the `||` is still a token of its own.
			"-- name: Q :many\nSELECT id FROM orders WHERE a=1||b=1 ORDER BY id\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (a=1||b=1) ORDER BY id",
		},
		{
			// The MySQL lexer munches operator runs greedily: `||-` is ONE
			// operator token that still carries the logical OR.
			"-- name: Q :many\nSELECT id FROM orders WHERE a = 1 ||-b\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (a = 1 ||-b)",
		},
		{
			// A keyword-column in operand position before XOR must not
			// hide it (the audit-19 discipline, extended).
			"-- name: Q :many\nSELECT id FROM orders WHERE x LIKE offset XOR active\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (x LIKE offset XOR active)",
		},
	}
	for _, tc := range cases {
		if got := weaveRender(t, mysql.Profile{}, mysql.Frontend{}, tc.src, pol); got != tc.want {
			t.Errorf("loose boolean operator escaped the tenant scope:\n got: %s\nwant: %s", got, tc.want)
		}
	}
}

// The D2a ON-clause path: a designated table on a join's nullable side is
// scoped in that join's ON, which must be wrapped the same way.
func TestWeave_MySQL_LooseBooleanOps_OnWraps(t *testing.T) {
	pol := Policy{Name: "t", Tables: []string{"orders"}, Predicate: "{}.tenant_id = :tid", ParamName: "tid", ParamType: "bigint"}
	cases := []struct{ src, want string }{
		{
			"-- name: Q :many\nSELECT a.id FROM acc a LEFT JOIN orders o ON o.aid = a.id XOR o.k = 1\n",
			"SELECT a.id FROM acc a LEFT JOIN orders o ON (o.aid = a.id XOR o.k = 1) AND (o.tenant_id = ?)",
		},
		{
			"-- name: Q :many\nSELECT a.id FROM acc a LEFT JOIN orders o ON o.aid = a.id || o.k = 1 WHERE a.ok\n",
			"SELECT a.id FROM acc a LEFT JOIN orders o ON (o.aid = a.id || o.k = 1) AND (o.tenant_id = ?) WHERE a.ok",
		},
	}
	for _, tc := range cases {
		if got := weaveRender(t, mysql.Profile{}, mysql.Frontend{}, tc.src, pol); got != tc.want {
			t.Errorf("loose boolean operator escaped the ON scope:\n got: %s\nwant: %s", got, tc.want)
		}
	}
}

// Idempotence: a hand-written scoping conjunct followed by a looser
// operator is NOT a top-level conjunct (`tenant AND a XOR b` is
// `(tenant AND a) XOR b`), so the weaver must not skip, and Enforce must
// not credit it.
func TestWeave_MySQL_LooseBooleanOps_PoisonIdempotence(t *testing.T) {
	pol := Policy{Name: "t", Tables: []string{"orders"}, Predicate: "tenant_id = :tid", ParamName: "tid", ParamType: "bigint"}
	cases := []struct{ src, want string }{
		{
			"-- name: Q :many\nSELECT id FROM orders WHERE tenant_id = :tid AND a XOR b\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (tenant_id = ? AND a XOR b)",
		},
		{
			"-- name: Q :many\nSELECT id FROM orders WHERE tenant_id = :tid AND a || b\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (tenant_id = ? AND a || b)",
		},
	}
	for _, tc := range cases {
		if got := weaveRender(t, mysql.Profile{}, mysql.Frontend{}, tc.src, pol); got != tc.want {
			t.Errorf("hand-written conjunct under a loose operator was trusted:\n got: %s\nwant: %s", got, tc.want)
		}
	}
}

func TestEnforce_MySQL_LooseBooleanOps_NotCredited(t *testing.T) {
	pol := Policy{Name: "t", Tables: []string{"orders"}, Predicate: "{}.tenant_id = :tid", ParamName: "tid", ParamType: "bigint"}
	srcs := []string{
		"-- name: Q :many\nSELECT o.id FROM orders o WHERE o.tenant_id = :tid AND o.a XOR o.b\n",
		"-- name: Q :many\nSELECT o.id FROM orders o WHERE o.tenant_id = :tid AND o.a || o.b\n",
		"-- name: Q :many\nSELECT a.id FROM acc a LEFT JOIN orders o ON o.tenant_id = :tid AND o.aid = a.id XOR o.k = 1\n",
		"-- name: Q :many\nSELECT a.id FROM acc a LEFT JOIN orders o ON o.tenant_id = :tid AND o.aid = a.id || o.k = 1\n",
	}
	for _, src := range srcs {
		f, diags := template.NewScanner(mysql.Profile{}).ScanFile("t.sql", []byte(src))
		if diagnostics.HasErrors(diags) {
			t.Fatalf("scan: %+v", diags)
		}
		q := f.Queries[0]
		r, err := ast.Render(mysql.Profile{}, q, nil)
		if err != nil {
			t.Fatal(err)
		}
		tree, err := mysql.Frontend{}.Parse(r.SQL)
		if err != nil {
			t.Fatalf("parse %q: %v", r.SQL, err)
		}
		ed := Enforce(mysql.Profile{}, mysql.Frontend{}, []Policy{pol}, q, tree, r)
		if len(ed) != 1 || ed[0].Code != diagnostics.CodePolicyUnscoped {
			t.Errorf("src %q: want exactly one SQLETCH124 (conjunct is under a looser operator), got %+v", src, ed)
		}
	}
}

// Controls: on PostgreSQL and SQLite `||` is string concatenation, which
// binds TIGHTER than AND — it neither wraps nor poisons idempotence
// there (the woven output of existing templates is unchanged).
func TestWeave_PipesConcat_NotORClass(t *testing.T) {
	pol := Policy{Name: "t", Tables: []string{"orders"}, Predicate: "tenant_id = :tid", ParamName: "tid", ParamType: "bigint"}
	type fx struct {
		name    string
		profile dialect.LexerProfile
		fe      dialect.Frontend
		ph      string
	}
	for _, d := range []fx{
		{"postgres", postgres.Profile{}, postgres.Frontend{}, "$1"},
		{"sqlite", sqlite.Profile{}, sqlite.Frontend{}, "?"},
	} {
		got := weaveRender(t, d.profile, d.fe, "-- name: Q :many\nSELECT id FROM orders WHERE name || 'x' = 'ax'\n", pol)
		want := "SELECT id FROM orders WHERE (tenant_id = " + d.ph + ") AND name || 'x' = 'ax'"
		if got != want {
			t.Errorf("%s: concat `||` treated as OR-class:\n got: %s\nwant: %s", d.name, got, want)
		}
		got = weaveRender(t, d.profile, d.fe, "-- name: Q :many\nSELECT id FROM orders WHERE tenant_id = :tid AND name || 'x' = 'ax'\n", pol)
		want = "SELECT id FROM orders WHERE tenant_id = " + d.ph + " AND name || 'x' = 'ax'"
		if got != want {
			t.Errorf("%s: concat `||` poisoned idempotence:\n got: %s\nwant: %s", d.name, got, want)
		}
	}
}
