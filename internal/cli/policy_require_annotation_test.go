package cli

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

const requireAnnotationYAML = policyProjectYAML + "    require_annotation: true\n"

// §12.5: the obligation is catalog-free, so it is checked in the
// weaver (cli.scanChecks) and reaches the editor on a COLD cache,
// like every other lexical and structural diagnostic — not only when
// the committed cache happens to hold every rendering.
func TestOffline_RequireAnnotationReportedOffline(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{
		"sqletch.yaml":       requireAnnotationYAML,
		"db/schema.sql":      "CREATE TABLE orders (id bigint NOT NULL, tenant_id bigint NOT NULL, status text);",
		"queries/orders.sql": "-- name: ListOrders :many\nSELECT id FROM orders WHERE status = :status;\n",
	})
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	diags := res.Diags[cfg.Abs("queries/orders.sql")]
	if !hasDiagCode(diags, diagnostics.CodePolicyUnannotated) {
		t.Fatalf("no SQLETCH127 from the offline checker: %+v", diags)
	}
}

// The annotation satisfies the requirement end to end, and leaves the
// woven conjunct exactly where it was: an acknowledgment is not a
// switch (§12.2).
func TestOffline_RequireAnnotationSatisfied(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{
		"sqletch.yaml":  requireAnnotationYAML,
		"db/schema.sql": "CREATE TABLE orders (id bigint NOT NULL, tenant_id bigint NOT NULL, status text);",
		"queries/orders.sql": "-- name: ListOrders :many\n-- @policy-apply: tenant_scope\n" +
			"SELECT id FROM orders WHERE status = :status;\n",
	})
	c := NewOfflineChecker(cfg)
	res, err := c.Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.Abs("queries/orders.sql")
	if diags := res.Diags[path]; len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	wq := c.memo[path].wovenq["ListOrders"]
	if wq == nil {
		t.Fatal("no woven template memoized")
	}
	r, err := ast.Render(c.drv.profile, wq, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.SQL, "(orders.tenant_id = $1) AND") {
		t.Errorf("acknowledged query lost its conjunct:\n%s", r.SQL)
	}
	// The annotation stays in the rendered SQL verbatim, which is why
	// adding one re-keys the query's oracle entries (§12.8).
	if !strings.Contains(r.SQL, "@policy-apply: tenant_scope") {
		t.Errorf("annotation excised from the rendering:\n%s", r.SQL)
	}
}

// An opt-out discharges the obligation, so the two annotations are
// exhaustive: there is no third state a query can be left in.
func TestOffline_RequireAnnotationOptOutSatisfies(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{
		"sqletch.yaml":  requireAnnotationYAML,
		"db/schema.sql": "CREATE TABLE orders (id bigint NOT NULL, tenant_id bigint NOT NULL, status text);",
		"queries/orders.sql": "-- name: ListOrders :many\n-- @policy-optout: tenant_scope (ops dashboard)\n" +
			"SELECT id FROM orders WHERE status = :status;\n",
	})
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	if diags := res.Diags[cfg.Abs("queries/orders.sql")]; len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
}

func hasDiagCode(diags []diagnostics.Diagnostic, code diagnostics.Code) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}
