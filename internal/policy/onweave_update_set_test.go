package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/template"
)

// Audit-14: in a MySQL multi-table UPDATE, the join's ON clause is
// followed by the SET clause (unlike SELECT, where a JOIN's ON is
// followed by WHERE/GROUP/ORDER). splice.go's ON-scan terminated on
// join/tail/WHERE keywords but NOT on SET, so when a policy-designated
// table sat on an outer-join nullable side (the D2a ON-weave path), the
// scan swallowed the entire SET assignment list as ON content and
// spliced the tenant conjunct AFTER the SET clause — inside the assigned
// value expression. The join stayed unscoped (`ON a.id=o.aid`, no tenant
// filter), so `orders` joined across all tenants: a silent leak that
// PREPAREs and runs. Weave and Enforce both returned zero diagnostics.
// The fix terminates the ON-scan on SET, weaving into the correct ON.
func TestWeave_MySQL_UpdateJoinSetOnWeave(t *testing.T) {
	pol := Policy{
		Name:      "tenant",
		Tables:    []string{"orders"},
		Predicate: "{}.tenant_id = :tenant_id",
		ParamName: "tenant_id",
		ParamType: "bigint",
	}
	cases := []string{
		"-- name: Q :exec\nUPDATE audit a LEFT JOIN orders o ON a.id=o.aid SET a.n=1\n",
		"-- name: Q :exec\nUPDATE audit a LEFT JOIN orders o ON a.id=o.aid SET a.n=1 WHERE a.ok\n",
		"-- name: Q :exec\nUPDATE audit a LEFT JOIN orders o ON a.id=o.aid SET a.n=o.v\n",
	}
	for _, src := range cases {
		f, diags := template.NewScanner(mysql.Profile{}).ScanFile("test.sql", []byte(src))
		if diagnostics.HasErrors(diags) {
			t.Fatalf("scan diagnostics: %+v", diags)
		}
		q := f.Queries[0]
		res := Weave(mysql.Profile{}, mysql.Frontend{}, []Policy{pol}, q)
		if len(res.Diags) != 0 {
			t.Fatalf("src %q: unexpected diagnostics: %+v", src, res.Diags)
		}
		r, err := ast.Render(mysql.Profile{}, res.Query, nil)
		if err != nil {
			t.Fatalf("src %q: render: %v", src, err)
		}
		got := strings.TrimSpace(r.SQL)
		// The conjunct must be inside the ON (before SET), scoping the
		// join; it must NOT land in the SET assignment list.
		if !strings.Contains(got, "ON a.id=o.aid AND (o.tenant_id = ?)") {
			t.Errorf("src %q: tenant filter not woven into the join ON:\n%s", src, got)
		}
		if idxSet, idxTenant := strings.Index(got, "SET "), strings.Index(got, "tenant_id"); idxSet >= 0 && idxTenant > idxSet {
			t.Errorf("leak: scoping conjunct spliced at/after the SET clause (gates nothing):\n%s", got)
		}
	}
}
