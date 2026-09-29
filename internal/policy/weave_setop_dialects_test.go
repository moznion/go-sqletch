package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/template"
)

type dialectKit struct {
	profile dialect.LexerProfile
	fe      dialect.Frontend
}

var sqliteKit = dialectKit{sqlite.Profile{}, sqlite.Frontend{}}

func (k dialectKit) scan(t *testing.T, body string) *template.QueryTemplate {
	t.Helper()
	f, diags := template.NewScanner(k.profile).ScanFile("test.sql", []byte("-- name: Q :many\n"+body+"\n"))
	if diagnostics.HasErrors(diags) {
		t.Fatalf("scan diagnostics: %+v", diags)
	}
	return f.Queries[0]
}

func (k dialectKit) render(t *testing.T, q *template.QueryTemplate) (string, ast.Rendering) {
	t.Helper()
	r, err := ast.Render(k.profile, q, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return strings.TrimSpace(r.SQL), r
}

func (k dialectKit) enforce(t *testing.T, q *template.QueryTemplate, pols ...Policy) []diagnostics.Diagnostic {
	t.Helper()
	_, r := k.render(t, q)
	tree, err := k.fe.Parse(r.SQL)
	if err != nil {
		t.Fatalf("parse %q: %v", r.SQL, err)
	}
	return Enforce(k.profile, k.fe, pols, q, tree, r)
}

func literalTenant() Policy {
	return Policy{Name: "tenant", Tables: []string{"orders"}, Predicate: "{}.tenant_id = 1"}
}

// Regression (pre-§13 silent leak, SQLite only): the SQLite facade's
// whole-statement Relations() of a compound SELECT is the FIRST core's,
// so the weaver saw `orders` as a top-level occurrence and spliced its
// conjunct at the statement's first WHERE keyword — the SECOND branch's.
// With both branches aliased `o` and archive carrying a tenant_id
// column, the result was valid SQL that scoped archive and left orders
// wide open, and Enforce (reading the same first WHERE) passed it.
func TestWeave_SQLite_CompoundBranchLeakRegression(t *testing.T) {
	k := sqliteKit
	body := "SELECT o.id FROM orders o UNION SELECT o.id FROM archive o WHERE o.x = 1"
	res := Weave(k.profile, k.fe, []Policy{literalTenant()}, k.scan(t, body))
	noDiags(t, res)
	got, _ := k.render(t, res.Query)
	want := "SELECT o.id FROM orders o WHERE (o.tenant_id = 1) UNION SELECT o.id FROM archive o WHERE o.x = 1"
	if got != want {
		t.Errorf("woven:\n got: %s\nwant: %s", got, want)
	}
	if diags := k.enforce(t, res.Query, literalTenant()); len(diags) != 0 {
		t.Errorf("enforce rejects the weaver's output: %+v", diags)
	}

	// The old (leaking) woven shape must now fail enforcement.
	leaked := k.scan(t, "SELECT o.id FROM orders o UNION SELECT o.id FROM archive o WHERE (o.tenant_id = 1) AND o.x = 1")
	diags := k.enforce(t, leaked, literalTenant())
	if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
		t.Fatalf("leaked shape: want one SQLETCH124, got %+v", diags)
	}
}

func TestWeave_SetOp_SQLite(t *testing.T) {
	cases := []struct {
		name string
		kit  dialectKit
		body string
		want string // "" = expect SQLETCH125
	}{
		{
			name: "sqlite: compound ORDER BY/LIMIT stay set-level",
			kit:  sqliteKit,
			body: "SELECT id FROM users UNION ALL SELECT id FROM orders ORDER BY 1 LIMIT 3",
			want: "SELECT id FROM users UNION ALL SELECT id FROM orders WHERE (orders.tenant_id = 1) ORDER BY 1 LIMIT 3",
		},
		{
			name: "sqlite: every branch, EXCEPT and INTERSECT",
			kit:  sqliteKit,
			body: "SELECT id FROM orders EXCEPT SELECT id FROM orders o WHERE o.gone INTERSECT SELECT id FROM users",
			want: "SELECT id FROM orders WHERE (orders.tenant_id = 1) EXCEPT SELECT id FROM orders o WHERE (o.tenant_id = 1) AND o.gone INTERSECT SELECT id FROM users",
		},
		{
			name: "sqlite: subquery in the compound ORDER BY is refused",
			kit:  sqliteKit,
			body: "SELECT id FROM users UNION SELECT id FROM archive ORDER BY (SELECT max(id) FROM orders)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := tc.kit
			q := k.scan(t, tc.body)
			res := Weave(k.profile, k.fe, []Policy{literalTenant()}, q)
			if tc.want == "" {
				if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
					t.Fatalf("diags = %+v, want exactly one SQLETCH125", res.Diags)
				}
				if res.Query != q {
					t.Errorf("rejected query must not be woven")
				}
				return
			}
			noDiags(t, res)
			if got, _ := k.render(t, res.Query); got != tc.want {
				t.Errorf("woven:\n got: %s\nwant: %s", got, tc.want)
			}
			if diags := k.enforce(t, res.Query, literalTenant()); len(diags) != 0 {
				t.Errorf("enforce rejects the weaver's output: %+v", diags)
			}
		})
	}
}
