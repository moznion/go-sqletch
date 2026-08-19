package policy

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/template"
)

// M10 end-to-end: a designated table hiding in a DML statement's WITH
// body used to be invisible to the SQLite facade (CTEs()/DeepTables()
// skipped DML WITH clauses), so the weaver neither wove nor rejected —
// a silent policy leak. Now the table surfaces in DeepTables() but not
// Relations(), so the weaver raises SQLETCH125 (loud and incomplete
// beats silent, doc 14 §D6). This test would find NO diagnostic before
// the facade fix.
func TestWeave_SQLite_DesignatedTableHiddenInDMLWith(t *testing.T) {
	src := "-- name: Q :exec\n" +
		"WITH sensitive AS (SELECT id FROM orders)\n" +
		"UPDATE audit SET n = 1 WHERE audit.id IN (SELECT id FROM sensitive)\n"

	f, diags := template.NewScanner(sqlite.Profile{}).ScanFile("test.sql", []byte(src))
	if diagnostics.HasErrors(diags) {
		t.Fatalf("scan diagnostics: %+v", diags)
	}
	if len(f.Queries) != 1 {
		t.Fatalf("got %d queries", len(f.Queries))
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
		t.Fatalf("expected SQLETCH125 (policy applies to a table hidden in the DML WITH body but cannot be woven), got diags=%+v woven=%+v", res.Diags, res.Woven)
	}
}
