package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/template"
)

// Audit-15: whereClause classified a depth-0 identifier as a clause
// keyword purely by its text, ignoring a preceding `.`. A reserved word
// is a legal column name after a dot (`o.group`, `o.order`, …), so
// `WHERE o.group = 1 OR o.x = 2` made the scan stop at `group`, miss the
// depth-0 OR (hasOR stayed false), and weave an UNWRAPPED conjunct:
// `WHERE (tenant_id=$1) AND o.group=1 OR o.x=2` = `(... AND ...) OR o.x=2`
// — every `o.x=2` row returned for ALL tenants. Valid SQL that PREPAREs,
// so no real-DB test catches it; Enforce shared the scanner so SQLETCH124
// could not fire either. The fix suppresses keyword classification in
// indirection position, so the OR is seen and the clause is wrapped.
func TestWeave_DottedKeywordColumnBeforeOR_Wraps(t *testing.T) {
	// Every tailKeyword, used as a dotted column before a depth-0 OR.
	for _, kw := range []string{"group", "order", "having", "limit", "offset", "fetch", "for", "returning", "window", "union", "intersect", "except"} {
		src := "-- name: Q :many\nSELECT o.id FROM orders o WHERE o." + kw + " = 1 OR o.x = 2\n"
		f, diags := template.NewScanner(postgres.Profile{}).ScanFile("t.sql", []byte(src))
		if diagnostics.HasErrors(diags) {
			t.Fatalf("kw %q: scan: %+v", kw, diags)
		}
		q := f.Queries[0]
		pol := Policy{Name: "tenant", Tables: []string{"orders"}, Predicate: "tenant_id = :tenant_id", ParamName: "tenant_id", ParamType: "bigint"}
		res := Weave(postgres.Profile{}, postgres.Frontend{}, []Policy{pol}, q)
		if diagnostics.HasErrors(res.Diags) {
			t.Fatalf("kw %q: unexpected diags: %+v", kw, res.Diags)
		}
		r, err := ast.Render(postgres.Profile{}, res.Query, nil)
		if err != nil {
			t.Fatalf("kw %q: render: %v", kw, err)
		}
		got := strings.TrimSpace(r.SQL)
		// The existing OR-clause must be parenthesized so the tenant
		// filter scopes the whole disjunction.
		want := "SELECT o.id FROM orders o WHERE (tenant_id = $1) AND (o." + kw + " = 1 OR o.x = 2)"
		if got != want {
			t.Errorf("kw %q: OR escaped tenant scope:\n got: %s\nwant: %s", kw, got, want)
		}
	}
}

// Control: a non-keyword dotted column still weaves correctly (the guard
// did not break ordinary wrapping), and a bare (non-dotted) keyword still
// terminates the clause as before.
func TestWeave_DottedKeywordColumn_Controls(t *testing.T) {
	pol := Policy{Name: "tenant", Tables: []string{"orders"}, Predicate: "tenant_id = :tenant_id", ParamName: "tenant_id", ParamType: "bigint"}
	cases := []struct{ src, want string }{
		{
			"-- name: Q :many\nSELECT o.id FROM orders o WHERE o.a = 1 OR o.x = 2\n",
			"SELECT o.id FROM orders o WHERE (tenant_id = $1) AND (o.a = 1 OR o.x = 2)",
		},
		{
			// No OR: plain AND-append, no wrap.
			"-- name: Q :many\nSELECT o.id FROM orders o WHERE o.group = 1\n",
			"SELECT o.id FROM orders o WHERE (tenant_id = $1) AND o.group = 1",
		},
		{
			// A real trailing GROUP BY (bare keyword) still bounds WHERE.
			"-- name: Q :many\nSELECT o.k FROM orders o WHERE o.x = 1 OR o.y = 2 GROUP BY o.k\n",
			"SELECT o.k FROM orders o WHERE (tenant_id = $1) AND (o.x = 1 OR o.y = 2) GROUP BY o.k",
		},
	}
	for _, tc := range cases {
		f, _ := template.NewScanner(postgres.Profile{}).ScanFile("t.sql", []byte(tc.src))
		q := f.Queries[0]
		res := Weave(postgres.Profile{}, postgres.Frontend{}, []Policy{pol}, q)
		if diagnostics.HasErrors(res.Diags) {
			t.Fatalf("src %q: diags %+v", tc.src, res.Diags)
		}
		r, err := ast.Render(postgres.Profile{}, res.Query, nil)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got := strings.TrimSpace(r.SQL); got != tc.want {
			t.Errorf("got:  %s\nwant: %s", got, tc.want)
		}
	}
}

// A dotted keyword-column inside a join's ON (`ON a.group = 1 OR …`) used
// to truncate the ON scan mid-expression and splice invalid SQL; now it
// weaves into the correct ON with the disjunction wrapped. MySQL uses `?`
// placeholders. (Postgres would also apply; one dialect suffices to pin
// the joinOn guard.)
func TestWeave_DottedKeywordColumnInJoinOn(t *testing.T) {
	src := "-- name: Q :many\nSELECT a.id FROM acc a LEFT JOIN orders o ON o.group = a.g OR o.h = 1\n"
	f, diags := template.NewScanner(mysql.Profile{}).ScanFile("t.sql", []byte(src))
	if diagnostics.HasErrors(diags) {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	pol := Policy{Name: "tenant", Tables: []string{"orders"}, Predicate: "{}.tenant_id = :tenant_id", ParamName: "tenant_id", ParamType: "bigint"}
	res := Weave(mysql.Profile{}, mysql.Frontend{}, []Policy{pol}, q)
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("unexpected diags: %+v", res.Diags)
	}
	r, err := ast.Render(mysql.Profile{}, res.Query, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := strings.TrimSpace(r.SQL)
	// The conjunct weaves into the ON (a LEFT JOIN's nullable side → ON,
	// per D2a), with the existing OR wrapped and the token boundary intact.
	if !strings.Contains(got, "ON (o.group = a.g OR o.h = 1) AND (o.tenant_id = ?)") &&
		!strings.Contains(got, "ON o.group = a.g OR o.h = 1 AND (o.tenant_id = ?)") {
		t.Fatalf("joinOn weave malformed or unwrapped:\n%s", got)
	}
	if strings.Contains(got, "group AND") || strings.Contains(got, "o. AND") {
		t.Fatalf("token boundary split by a mis-scanned keyword-column:\n%s", got)
	}
}
