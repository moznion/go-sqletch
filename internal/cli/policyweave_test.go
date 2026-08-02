package cli

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

const policyProjectYAML = `version: 1
dialect: postgres
server_version: "16"
schema:
  files: [db/schema.sql]
queries: [queries/*.sql]
output:
  package: gen
  path: gen
policies:
  - name: tenant_scope
    tables: [orders]
    predicate: "{}.tenant_id = :tenant_id"
    param:
      name: tenant_id
      type: bigint
`

// The OfflineChecker weaves exactly like the pipeline (§11.5: one
// shared scanChecks); the woven template and its renderings carry the
// policy conjunct.
func TestOffline_PolicyWeaves(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{
		"sqletch.yaml":       policyProjectYAML,
		"db/schema.sql":      "CREATE TABLE orders (id bigint NOT NULL, tenant_id bigint NOT NULL, status text);",
		"queries/orders.sql": "-- name: ListOrders :many\nSELECT id FROM orders WHERE status = :status;\n",
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
	m := c.memo[path]
	wq := m.wovenq["ListOrders"]
	if wq == nil {
		t.Fatal("no woven template memoized")
	}
	r, err := ast.Render(c.drv.profile, wq, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.SQL, "orders.tenant_id = $1 AND") {
		t.Errorf("woven rendering lacks the policy conjunct:\n%s", r.SQL)
	}
	if rs := m.rends["ListOrders"]; len(rs) == 0 || rs[0].SQL != r.SQL {
		t.Errorf("memoized renderings are not of the woven template")
	}
}

func TestOffline_PolicyUnweavableIsDiagnosed(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{
		"sqletch.yaml": policyProjectYAML,
		"queries/leaky.sql": "-- name: Leaky :many\n" +
			"SELECT u.id FROM u LEFT JOIN orders o ON o.id = u.id WHERE u.name = :name;\n",
	})
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.Abs("queries/leaky.sql")
	found := false
	for _, d := range res.Diags[path] {
		if d.Code == diagnostics.CodePolicyUnweavable {
			found = true
		}
	}
	if !found {
		t.Errorf("no SQLETCH125 for the nullable-side join: %+v", res.Diags[path])
	}
}

// A broken policy degrades the checker to unwoven analysis and pins
// SQLETCH303 on the config file — never a crash, never half-woven
// output.
func TestOffline_BrokenPolicyDegrades(t *testing.T) {
	broken := strings.Replace(policyProjectYAML,
		`predicate: "{}.tenant_id = :tenant_id"`,
		`predicate: "ORDER BY oops"`, 1)
	cfg := writeOfflineProject(t, map[string]string{
		"sqletch.yaml":       broken,
		"queries/orders.sql": "-- name: ListOrders :many\nSELECT id FROM orders WHERE id = :id;\n",
	})
	c := NewOfflineChecker(cfg)
	res, err := c.Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range res.Diags[cfg.Path] {
		if d.Code == diagnostics.CodePolicyInvalid {
			found = true
		}
	}
	if !found {
		t.Fatalf("no SQLETCH303 against the config: %+v", res.Diags)
	}
	path := cfg.Abs("queries/orders.sql")
	m := c.memo[path]
	wq := m.wovenq["ListOrders"]
	r, err := ast.Render(c.drv.profile, wq, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.SQL, "tenant_id") {
		t.Errorf("query was woven despite a broken policy set:\n%s", r.SQL)
	}
}

// R6 runs on the UNWOVEN template: a policy must never make an
// all-optional WHERE valid — template validity cannot depend on
// configuration (design 14 §4.2).
func TestOffline_AnchorRuleIsConfigIndependent(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{
		"sqletch.yaml": policyProjectYAML,
		"queries/orders.sql": "-- name: Anchorless :many\n" +
			"SELECT id FROM orders WHERE\n@if-present(s)\nAND status = :s\n@endif\n;\n",
	})
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.Abs("queries/orders.sql")
	found := false
	for _, d := range res.Diags[path] {
		if d.Code == diagnostics.CodeUnanchoredClause {
			found = true
		}
	}
	if !found {
		t.Errorf("SQLETCH113 suppressed by the policy: %+v", res.Diags[path])
	}
}
