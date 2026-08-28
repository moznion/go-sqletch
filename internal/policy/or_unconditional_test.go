package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/template"
)

// Audit-19: afterOperand treats the infix/prefix operator keywords
// (IS/IN/LIKE/GLOB/MATCH/REGEXP/DIV/MOD/XOR/…, plus INTERVAL/BINARY) as
// operand-introducing so a keyword-column used as their operand is not a
// clause terminator. But those words are NON-RESERVED bare column names in
// SQLite, and when one is used as a bare boolean column the tracker left
// expectOperand=true, so a following depth-0 OR was not recognized and the
// weave was appended unwrapped — every OR-arm row leaked across tenants
// (PR #122 made this worse by adding INTERVAL/BINARY, but the whole set
// was affected). Fix: AND/OR are reserved in every dialect and can never
// be a bare column, so they are recognized regardless of operand-position
// state — a keyword-column mis-classification can no longer hide a
// following OR.
func TestWeave_KeywordColumnBeforeOR_AllWrap(t *testing.T) {
	pol := Policy{Name: "t", Tables: []string{"orders"}, Predicate: "tenant_id = :tid", ParamName: "tid", ParamType: "int"}
	// These are non-reserved keyword-columns that the ncruces engine
	// accepts as ordinary column names (verified: "no such column" not a
	// syntax error), so a real leak was reachable for each.
	// Non-reserved-in-SQLite keyword-columns (the ncruces engine accepts
	// each as an ordinary column name — verified "no such column", not a
	// syntax error). Reserved words like IN/IS/BETWEEN/ALL are a loud
	// engine parse error and are out of scope.
	for _, col := range []string{"binary", "interval", "glob", "match", "regexp", "like", "some", "any", "div", "mod", "xor"} {
		src := "-- name: Q :many\nSELECT id FROM orders WHERE " + col + " OR active\n"
		f, diags := template.NewScanner(sqlite.Profile{}).ScanFile("t.sql", []byte(src))
		if diagnostics.HasErrors(diags) {
			continue // reserved-in-SQLite words are a loud parse error, out of scope
		}
		q := f.Queries[0]
		res := Weave(sqlite.Profile{}, sqlite.Frontend{}, []Policy{pol}, q)
		if diagnostics.HasErrors(res.Diags) {
			t.Fatalf("col %q: unexpected diags %+v", col, res.Diags)
		}
		r, err := ast.Render(sqlite.Profile{}, res.Query, nil)
		if err != nil {
			t.Fatalf("col %q render: %v", col, err)
		}
		got := strings.TrimSpace(r.SQL)
		want := "SELECT id FROM orders WHERE (tenant_id = ?) AND (" + col + " OR active)"
		if got != want {
			t.Errorf("col %q: OR escaped the tenant scope (unwrapped weave):\n got: %s\nwant: %s", col, got, want)
		}
	}
}

// Regression: the tailKeyword termination and the ordinary controls still
// hold (a real trailing clause bounds WHERE; a keyword-column operand of
// an infix operator is still a column; a plain OR/AND still splits).
func TestWeave_ORUnconditional_Controls(t *testing.T) {
	pol := Policy{Name: "t", Tables: []string{"orders"}, Predicate: "tenant_id = :tid", ParamName: "tid", ParamType: "int"}
	cases := []struct{ src, want string }{
		{
			"-- name: Q :many\nSELECT id FROM orders WHERE offset = 1 OR active GROUP BY id\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (offset = 1 OR active) GROUP BY id",
		},
		{
			"-- name: Q :many\nSELECT id FROM orders WHERE a = 1 AND b = 2\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND a = 1 AND b = 2",
		},
		{
			"-- name: Q :many\nSELECT id FROM orders WHERE name LIKE offset OR active\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (name LIKE offset OR active)",
		},
	}
	for _, tc := range cases {
		f, _ := template.NewScanner(sqlite.Profile{}).ScanFile("t.sql", []byte(tc.src))
		q := f.Queries[0]
		res := Weave(sqlite.Profile{}, sqlite.Frontend{}, []Policy{pol}, q)
		if diagnostics.HasErrors(res.Diags) {
			t.Fatalf("src %q diags %+v", tc.src, res.Diags)
		}
		r, err := ast.Render(sqlite.Profile{}, res.Query, nil)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got := strings.TrimSpace(r.SQL); got != tc.want {
			t.Errorf("got:  %s\nwant: %s", got, tc.want)
		}
	}
}
