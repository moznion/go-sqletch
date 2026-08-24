package policy

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/template"
)

// End-to-end for the audit-13 finding: a designated table read ONLY
// through a subquery inside a window OVER(...) or a named WINDOW clause
// used to be invisible to the SQLite facade's tableWalker, so the weaver
// neither wove a predicate nor refused — a silent tenant leak (the H5
// class relocated to the window clause). Now the table surfaces in
// DeepTables() but not Relations() (a window-subquery read is not a FROM
// occurrence the weaver can scope), so the weaver raises SQLETCH125
// (loud beats silent, doc 14 §D6). Before the facade fix this test finds
// NO diagnostic.
func TestWeave_SQLite_DesignatedTableHiddenInWindowSubquery(t *testing.T) {
	for _, src := range []string{
		"-- name: Q :many\nSELECT t.x, row_number() OVER (PARTITION BY (SELECT max(id) FROM orders)) AS rn FROM t\n",
		"-- name: Q :many\nSELECT row_number() OVER (ORDER BY (SELECT max(id) FROM orders)) FROM t\n",
		"-- name: Q :many\nSELECT row_number() OVER w FROM t WINDOW w AS (ORDER BY (SELECT max(id) FROM orders))\n",
	} {
		f, diags := template.NewScanner(sqlite.Profile{}).ScanFile("test.sql", []byte(src))
		if diagnostics.HasErrors(diags) {
			t.Fatalf("scan diagnostics: %+v", diags)
		}
		q := f.Queries[0]
		if _, err := ast.Render(sqlite.Profile{}, q, nil); err != nil {
			t.Fatalf("render: %v", err)
		}

		pol := Policy{Name: "tenant", Tables: []string{"orders"}, Predicate: "{}.tenant_id = 1"}
		res := Weave(sqlite.Profile{}, sqlite.Frontend{}, []Policy{pol}, q)

		found := false
		for _, d := range res.Diags {
			if d.Code == diagnostics.CodePolicyUnweavable {
				found = true
			}
		}
		if !found {
			t.Fatalf("src %q: expected SQLETCH125 (designated table hidden in a window subquery, unweavable), got diags=%+v woven=%+v", src, res.Diags, res.Woven)
		}
	}
}

// A window over non-subquery expressions must not create a spurious
// hidden occurrence: a policy on the real FROM table t weaves normally.
func TestWeave_SQLite_PlainWindowStillWeaves(t *testing.T) {
	src := "-- name: Q :many\n" +
		"SELECT row_number() OVER (PARTITION BY t.x ORDER BY t.y) FROM t WHERE t.ok\n"

	f, diags := template.NewScanner(sqlite.Profile{}).ScanFile("test.sql", []byte(src))
	if diagnostics.HasErrors(diags) {
		t.Fatalf("scan diagnostics: %+v", diags)
	}
	q := f.Queries[0]
	if _, err := ast.Render(sqlite.Profile{}, q, nil); err != nil {
		t.Fatalf("render: %v", err)
	}

	pol := Policy{Name: "tenant", Tables: []string{"t"}, Predicate: "{}.tenant_id = 1"}
	res := Weave(sqlite.Profile{}, sqlite.Frontend{}, []Policy{pol}, q)
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("unexpected diagnostics: a plain window must not create a hidden occurrence: %+v", res.Diags)
	}
	if len(res.Woven) != 1 || res.Woven[0].OptedOut {
		t.Fatalf("expected the t policy to be woven, got %+v", res.Woven)
	}
}
