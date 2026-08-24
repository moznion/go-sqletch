package policy

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/template"
)

// End-to-end for the audit-14 findings: a policy-designated table read
// ONLY through an INSERT ON CONFLICT DO UPDATE subquery or a DELETE
// RETURNING subquery used to be invisible to the SQLite facade's
// tableWalker, so the weaver neither wove nor refused — a silent tenant
// leak. Now the table surfaces in DeepTables() but not Relations() (none
// of these are a FROM occurrence the weaver can scope), so the weaver
// raises SQLETCH125 (loud beats silent, doc 14 §D6). Before the facade
// fix this test finds NO diagnostic.
func TestWeave_SQLite_DesignatedTableHiddenInDMLSubquery(t *testing.T) {
	for _, src := range []string{
		"-- name: Q :exec\nINSERT INTO orders (id, v) VALUES (:id, :v) ON CONFLICT (id) DO UPDATE SET v = (SELECT secret FROM tenants WHERE tenants.id = :id)\n",
		"-- name: Q :exec\nINSERT INTO orders (id, v) VALUES (:id, :v) ON CONFLICT (id) DO UPDATE SET v = 3 WHERE v IN tenants\n",
		"-- name: Q :many\nDELETE FROM orders WHERE id = :id RETURNING (SELECT secret FROM tenants)\n",
	} {
		f, diags := template.NewScanner(sqlite.Profile{}).ScanFile("test.sql", []byte(src))
		if diagnostics.HasErrors(diags) {
			t.Fatalf("scan diagnostics: %+v", diags)
		}
		q := f.Queries[0]
		if _, err := ast.Render(sqlite.Profile{}, q, nil); err != nil {
			t.Fatalf("render: %v", err)
		}

		pol := Policy{Name: "tenant", Tables: []string{"tenants"}, Predicate: "{}.tenant_id = 1"}
		res := Weave(sqlite.Profile{}, sqlite.Frontend{}, []Policy{pol}, q)

		found := false
		for _, d := range res.Diags {
			if d.Code == diagnostics.CodePolicyUnweavable {
				found = true
			}
		}
		if !found {
			t.Fatalf("src %q: expected SQLETCH125 (designated table hidden in a DML subquery, unweavable), got diags=%+v woven=%+v", src, res.Diags, res.Woven)
		}
	}
}
