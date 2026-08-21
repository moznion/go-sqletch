package policy

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/template"
)

// End-to-end for finding H5: a designated table appearing ONLY as an
// `expr IN table-name` operand (rqlite parses `x IN secret_table` as a
// bare *Ident, `x IN aux2.secret_table` as a *QualifiedRef) used to be
// invisible to the SQLite facade's tableWalker, so the weaver's
// Relations/DeepTables comparison undercounted and neither wove nor
// rejected: a silent policy leak. Now the table surfaces in DeepTables()
// but not Relations() (an IN operand is not a FROM occurrence the weaver
// can scope), so the weaver raises SQLETCH125 (loud and incomplete beats
// silent, doc 14 §D6). Before the facade fix this test finds NO
// diagnostic.
func TestWeave_SQLite_DesignatedTableHiddenInInOperand(t *testing.T) {
	for _, src := range []string{
		"-- name: Q :many\nSELECT x FROM t WHERE x IN secret_table\n",
		"-- name: Q :many\nSELECT x FROM t WHERE x IN aux2.secret_table\n",
	} {
		f, diags := template.NewScanner(sqlite.Profile{}).ScanFile("test.sql", []byte(src))
		if diagnostics.HasErrors(diags) {
			t.Fatalf("scan diagnostics: %+v", diags)
		}
		q := f.Queries[0]
		if _, err := ast.Render(sqlite.Profile{}, q, nil); err != nil {
			t.Fatalf("render: %v", err)
		}

		pol := Policy{Name: "tenant", Tables: []string{"secret_table"}, Predicate: "{}.tenant_id = 1"}
		res := Weave(sqlite.Profile{}, sqlite.Frontend{}, []Policy{pol}, q)

		found := false
		for _, d := range res.Diags {
			if d.Code == diagnostics.CodePolicyUnweavable {
				found = true
			}
		}
		if !found {
			t.Fatalf("src %q: expected SQLETCH125 (designated table hidden in an IN operand, unweavable), got diags=%+v woven=%+v", src, res.Diags, res.Woven)
		}
	}
}

// A designated table reached through an IN (subquery) must still weave
// normally when it is a genuine FROM occurrence of that subquery — the
// IN-operand fix must not turn every IN into an unweavable position.
func TestWeave_SQLite_InSubqueryDesignatedTableStillWeaves(t *testing.T) {
	src := "-- name: Q :many\n" +
		"SELECT x FROM t WHERE x IN (1, 2, 3)\n"

	f, diags := template.NewScanner(sqlite.Profile{}).ScanFile("test.sql", []byte(src))
	if diagnostics.HasErrors(diags) {
		t.Fatalf("scan diagnostics: %+v", diags)
	}
	q := f.Queries[0]
	if _, err := ast.Render(sqlite.Profile{}, q, nil); err != nil {
		t.Fatalf("render: %v", err)
	}

	// A policy on the real FROM table t must weave with no SQLETCH125:
	// the `IN (1,2,3)` list introduces no hidden designated table.
	pol := Policy{Name: "tenant", Tables: []string{"t"}, Predicate: "{}.tenant_id = 1"}
	res := Weave(sqlite.Profile{}, sqlite.Frontend{}, []Policy{pol}, q)
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("unexpected diagnostics: an IN (expr-list) must not create a hidden occurrence: %+v", res.Diags)
	}
	if len(res.Woven) != 1 || res.Woven[0].OptedOut {
		t.Fatalf("expected the t policy to be woven, got %+v", res.Woven)
	}
}
