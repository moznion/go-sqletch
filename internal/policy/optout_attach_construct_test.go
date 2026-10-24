package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/template"
)

// An @policy-optout placed after query A's last plain SQL token but
// before a trailing construct belongs to A. It used to stay pending
// until the next `-- name:` header and attach to B, so B shipped
// unscoped (opted out) while A was scoped — an all-tenant read on the
// wrong query (CWE-863). Both Weave and Enforce honour opt-outs, so the
// scanner's attachment is the only line of defense.
func TestOptOutBeforeTrailingConstruct_ScopesFollowingQuery(t *testing.T) {
	src := "-- name: ExportOrders :many\n" +
		"SELECT o.id, o.total FROM orders AS o\n" +
		"WHERE TRUE\n" +
		"-- @policy-optout: tenant_scope (ops export across tenants)\n" +
		"@if-present(since) AND o.created_at >= :since @endif\n" +
		"\n" +
		"-- name: ListOrders :many\n" +
		"SELECT o.id, o.total FROM orders AS o\n"
	f, diags := template.NewScanner(postgres.Profile{}).ScanFile("test.sql", []byte(src))
	if diagnostics.HasErrors(diags) {
		t.Fatalf("scan diagnostics: %+v", diags)
	}
	if len(f.Queries) != 2 {
		t.Fatalf("got %d queries", len(f.Queries))
	}
	export, list := f.Queries[0], f.Queries[1]
	pols := []Policy{tenantPolicy()}

	// ExportOrders: opted out, left untouched.
	res := Weave(postgres.Profile{}, postgres.Frontend{}, pols, export)
	noDiags(t, res)
	if res.Query != export {
		t.Errorf("opted-out ExportOrders was rewritten: %s", renderSQL(t, res.Query))
	}
	if len(res.Woven) != 1 || !res.Woven[0].OptedOut {
		t.Errorf("ExportOrders opt-out not recorded: %+v", res.Woven)
	}

	// ListOrders: no opt-out of its own — it must be woven.
	if len(list.PolicyOptOuts) != 0 {
		t.Fatalf("ExportOrders' opt-out attached to ListOrders: %+v", list.PolicyOptOuts)
	}
	res = Weave(postgres.Profile{}, postgres.Frontend{}, pols, list)
	noDiags(t, res)
	got := renderSQL(t, res.Query)
	if !strings.Contains(got, "WHERE (o.tenant_id = $1)") {
		t.Errorf("ListOrders not woven: %s", got)
	}
	if d := enforceOn(t, res.Query, pols...); len(d) != 0 {
		t.Errorf("woven ListOrders rejected by Enforce: %+v", d)
	}
	// And unwoven ListOrders must fail enforcement (no stray opt-out).
	if d := enforceOn(t, list, pols...); len(d) != 1 || d[0].Code != diagnostics.CodePolicyUnscoped {
		t.Errorf("unwoven ListOrders must be SQLETCH124, got %+v", d)
	}
}
