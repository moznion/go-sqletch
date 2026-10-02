package rules

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

// ColumnResolver is the one exported view of the R3 resolver
// (internal/lint's SQLETCHL005 uses it instead of forking resolution):
// a qualifier names a top-level relation by alias or table name; an
// unqualified name must match exactly one relation's columns.
func TestColumnResolver(t *testing.T) {
	q := scanOne(t, `-- name: Q :many
SELECT u.id FROM users AS u JOIN orgs ON orgs.id = u.org_id WHERE u.status = :s
`)
	rs, err := ast.Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := postgres.Frontend{}.Parse(rs[0].SQL)
	if err != nil {
		t.Fatal(err)
	}
	res := NewColumnResolver(postgres.Profile{}, q, rs[0], tree, fixtureCatalog())
	cases := []struct {
		qualifier, name string
		want            string // "" = unresolved
	}{
		{"u", "email", "email"},
		{"orgs", "name", "name"},
		{"", "status", "status"}, // only users has it
		{"", "id", ""},           // users and orgs both have it: ambiguous
		{"", "nope", ""},
		{"x", "id", ""},     // unknown qualifier
		{"users", "id", ""}, // aliased: the table name is not in scope
		{"u", "nope", ""},
	}
	for _, c := range cases {
		got := ""
		if col := res.Column(c.qualifier, c.name); col != nil {
			got = col.Name
		}
		if got != c.want {
			t.Errorf("Column(%q, %q) = %q, want %q", c.qualifier, c.name, got, c.want)
		}
	}
}
