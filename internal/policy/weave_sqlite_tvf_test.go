package policy

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/template"
)

// End-to-end: a designated table reachable only through a table-valued
// function's argument (a subquery argument) used to be invisible to the
// SQLite facade — rqlite/sql parses `FROM json_each(…)` as a
// *QualifiedTableFunctionName that every source walker dropped, so the
// weaver's Relations/DeepTables comparison undercounted and neither wove
// nor rejected: a silent policy leak. Now the table surfaces in
// DeepTables() but not Relations(), so the weaver raises SQLETCH125
// (loud and incomplete beats silent, doc 14 §D6). Before the facade fix
// this test finds NO diagnostic.
func TestWeave_SQLite_DesignatedTableHiddenInTVFArg(t *testing.T) {
	src := "-- name: Q :many\n" +
		"SELECT value FROM json_each((SELECT tags FROM secrets LIMIT 1))\n"

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

	pol := Policy{Name: "tenant", Tables: []string{"secrets"}, Predicate: "{}.tenant_id = 1"}
	res := Weave(sqlite.Profile{}, sqlite.Frontend{}, []Policy{pol}, q)

	found := false
	for _, d := range res.Diags {
		if d.Code == diagnostics.CodePolicyUnweavable {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected SQLETCH125 (designated table hidden inside a TVF argument, unweavable), got diags=%+v woven=%+v", res.Diags, res.Woven)
	}
}

// A TVF sitting alongside a normal designated base table must not derail
// ordinary weaving: the policy still scopes the real table, and the
// opaque TVF neither adds a base table nor blocks the weave.
func TestWeave_SQLite_TVFAlongsideDesignatedTableStillWeaves(t *testing.T) {
	src := "-- name: Q :many\n" +
		"SELECT u.id, j.value FROM users u, json_each(u.tags) j WHERE u.active = 1\n"

	f, diags := template.NewScanner(sqlite.Profile{}).ScanFile("test.sql", []byte(src))
	if diagnostics.HasErrors(diags) {
		t.Fatalf("scan diagnostics: %+v", diags)
	}
	q := f.Queries[0]
	if _, err := ast.Render(sqlite.Profile{}, q, nil); err != nil {
		t.Fatalf("render: %v", err)
	}

	pol := Policy{Name: "tenant", Tables: []string{"users"}, Predicate: "{}.tenant_id = 1"}
	res := Weave(sqlite.Profile{}, sqlite.Frontend{}, []Policy{pol}, q)

	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("unexpected diagnostics weaving a normal designated table beside a TVF: %+v", res.Diags)
	}
	if len(res.Woven) != 1 || res.Woven[0].OptedOut {
		t.Fatalf("expected the users policy to be woven, got %+v", res.Woven)
	}
}
