package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/template"
)

// Audit-17: #120's operand-position tracking (afterOperand) modeled AND/OR/
// NOT and the infix word-operators, but NOT the CASE arms (CASE/WHEN/THEN/
// ELSE), which each introduce a value at paren depth 0. So a bare
// non-reserved keyword-column in a CASE arm (`THEN offset`) sat in what the
// scanner read as operator position and was misclassified as a clause
// terminator, ending the WHERE scan early, missing a following depth-0 OR,
// and weaving an unwrapped conjunct — a silent tenant leak (valid SQL,
// Weave+Enforce both silent). Fixed by treating the CASE arms as operand-
// introducing.
func TestWeave_KeywordColumnInCASE_Wraps(t *testing.T) {
	cases := []struct {
		name string
		prof dialect.LexerProfile
		fe   dialect.Frontend
		src  string
		want string
	}{
		{
			"mysql-then-offset",
			mysql.Profile{}, mysql.Frontend{},
			"-- name: Q :many\nSELECT id FROM orders WHERE CASE WHEN a = 1 THEN offset ELSE 0 END = 1 OR active\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (CASE WHEN a = 1 THEN offset ELSE 0 END = 1 OR active)",
		},
		{
			"mysql-when-offset",
			mysql.Profile{}, mysql.Frontend{},
			"-- name: Q :many\nSELECT id FROM orders WHERE CASE WHEN offset = 1 THEN 1 ELSE 0 END = 1 OR active\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (CASE WHEN offset = 1 THEN 1 ELSE 0 END = 1 OR active)",
		},
		{
			"sqlite-then-window",
			sqlite.Profile{}, sqlite.Frontend{},
			"-- name: Q :many\nSELECT id FROM orders WHERE CASE WHEN a = 1 THEN window ELSE 0 END = 1 OR active\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (CASE WHEN a = 1 THEN window ELSE 0 END = 1 OR active)",
		},
		{
			"sqlite-when-for",
			sqlite.Profile{}, sqlite.Frontend{},
			"-- name: Q :many\nSELECT id FROM orders WHERE CASE WHEN for = 1 THEN 1 ELSE 0 END = 1 OR active\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (CASE WHEN for = 1 THEN 1 ELSE 0 END = 1 OR active)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typ := "bigint"
			if _, ok := tc.prof.(sqlite.Profile); ok {
				typ = "int"
			}
			pol := Policy{Name: "t", Tables: []string{"orders"}, Predicate: "tenant_id = :tid", ParamName: "tid", ParamType: typ}
			f, diags := template.NewScanner(tc.prof).ScanFile("t.sql", []byte(tc.src))
			if diagnostics.HasErrors(diags) {
				t.Fatalf("scan: %+v", diags)
			}
			q := f.Queries[0]
			res := Weave(tc.prof, tc.fe, []Policy{pol}, q)
			if diagnostics.HasErrors(res.Diags) {
				t.Fatalf("unexpected diags: %+v", res.Diags)
			}
			r, err := ast.Render(tc.prof, res.Query, nil)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got := strings.TrimSpace(r.SQL); got != tc.want {
				t.Errorf("keyword-column in CASE leaked (OR escaped scope):\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// Control: a normal CASE with no keyword-column and no OR still weaves as a
// plain AND-append, and a CASE followed by a real trailing clause keeps
// that clause outside the wrap.
func TestWeave_CASE_Controls(t *testing.T) {
	pol := Policy{Name: "t", Tables: []string{"orders"}, Predicate: "tenant_id = :tid", ParamName: "tid", ParamType: "bigint"}
	cases := []struct{ src, want string }{
		{
			"-- name: Q :many\nSELECT id FROM orders WHERE CASE WHEN a = 1 THEN 1 ELSE 0 END = 1\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND CASE WHEN a = 1 THEN 1 ELSE 0 END = 1",
		},
		{
			"-- name: Q :many\nSELECT id FROM orders WHERE CASE WHEN a = 1 THEN 1 ELSE 0 END = 1 OR b = 2 LIMIT 5\n",
			"SELECT id FROM orders WHERE (tenant_id = ?) AND (CASE WHEN a = 1 THEN 1 ELSE 0 END = 1 OR b = 2) LIMIT 5",
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
