package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/template"
)

// Audit-18: #120/#121's operand-position tracking modeled infix operators
// and the CASE arms, but not the MySQL value-introducing PREFIX keyword
// operators that are lexed as identifiers — `INTERVAL <qty> <unit>` and
// the `BINARY <expr>` cast. afterOperand fell to its default
// operand-complete state after them, so a keyword-column in their operand
// slot (`INTERVAL offset DAY`, `BINARY offset`) was misread as a clause
// terminator, ending the WHERE scan early, missing a following depth-0 OR,
// and weaving an unwrapped conjunct — a silent tenant leak (valid SQL,
// Weave+Enforce both silent). Fixed by treating INTERVAL/BINARY as
// operand-introducing. MySQL-only (SQLite has no such ident-lexed prefix
// operator; PG has no bare keyword-columns).
func TestWeave_PrefixKeywordOperand_Wraps(t *testing.T) {
	pol := Policy{Name: "t", Tables: []string{"orders"}, Predicate: "tenant_id = :tid", ParamName: "tid", ParamType: "bigint"}
	cases := []struct{ src, want string }{
		{
			"-- name: Q :many\nSELECT id FROM orders WHERE d > c + INTERVAL offset DAY OR active\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (d > c + INTERVAL offset DAY OR active)",
		},
		{
			"-- name: Q :many\nSELECT id FROM orders WHERE d > c + INTERVAL returning DAY OR active\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (d > c + INTERVAL returning DAY OR active)",
		},
		{
			"-- name: Q :many\nSELECT id FROM orders WHERE x = BINARY offset OR active\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (x = BINARY offset OR active)",
		},
	}
	for _, tc := range cases {
		f, diags := template.NewScanner(mysql.Profile{}).ScanFile("t.sql", []byte(tc.src))
		if diagnostics.HasErrors(diags) {
			t.Fatalf("scan: %+v", diags)
		}
		q := f.Queries[0]
		res := Weave(mysql.Profile{}, mysql.Frontend{}, []Policy{pol}, q)
		if diagnostics.HasErrors(res.Diags) {
			t.Fatalf("src %q unexpected diags: %+v", tc.src, res.Diags)
		}
		r, err := ast.Render(mysql.Profile{}, res.Query, nil)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got := strings.TrimSpace(r.SQL); got != tc.want {
			t.Errorf("prefix keyword-operand leaked (OR escaped scope):\n got: %s\nwant: %s", got, tc.want)
		}
	}
}

// Control: INTERVAL/BINARY with ordinary (non-keyword) operands still
// weave correctly, and a real trailing clause after them stays outside
// the wrap.
func TestWeave_PrefixKeywordOperand_Controls(t *testing.T) {
	pol := Policy{Name: "t", Tables: []string{"orders"}, Predicate: "tenant_id = :tid", ParamName: "tid", ParamType: "bigint"}
	cases := []struct{ src, want string }{
		{
			"-- name: Q :many\nSELECT id FROM orders WHERE d > c + INTERVAL 1 DAY OR active\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (d > c + INTERVAL 1 DAY OR active)",
		},
		{
			"-- name: Q :many\nSELECT id FROM orders WHERE x = BINARY 'abc' OR y = 1 LIMIT 5\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (x = BINARY 'abc' OR y = 1) LIMIT 5",
		},
	}
	for _, tc := range cases {
		f, _ := template.NewScanner(mysql.Profile{}).ScanFile("t.sql", []byte(tc.src))
		q := f.Queries[0]
		res := Weave(mysql.Profile{}, mysql.Frontend{}, []Policy{pol}, q)
		if diagnostics.HasErrors(res.Diags) {
			t.Fatalf("src %q diags %+v", tc.src, res.Diags)
		}
		r, err := ast.Render(mysql.Profile{}, res.Query, nil)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got := strings.TrimSpace(r.SQL); got != tc.want {
			t.Errorf("got:  %s\nwant: %s", got, tc.want)
		}
	}
}
