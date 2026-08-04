package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/template"
)

// enforceOn parses the query's maximal rendering and runs Enforce —
// exactly what cli.resolvedChecks does.
func enforceOn(t *testing.T, q *template.QueryTemplate, pols ...Policy) []diagnostics.Diagnostic {
	t.Helper()
	r, err := ast.Render(postgres.Profile{}, q, nil)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := postgres.Frontend{}.Parse(r.SQL)
	if err != nil {
		t.Fatal(err)
	}
	return Enforce(postgres.Profile{}, pols, q, tree)
}

// The every-shape quantifier is what makes the check a proof rather
// than a formality: a hand-written scoping conjunct inside
// @if-present satisfies the guard-on shape only, and must FAIL.
func TestEnforce_GuardedConjunctFails(t *testing.T) {
	src := "-- name: Q :many\nSELECT o.id FROM orders o WHERE o.ok\n" +
		"@if-present(tenant_id)\nAND o.tenant_id = :tenant_id\n@endif\n"
	q := scanOne(t, src)
	diags := enforceOn(t, q, tenantPolicy())
	if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
		t.Fatalf("want exactly one SQLETCH124, got %+v", diags)
	}
	if !strings.Contains(diags[0].Hint, "@if-present") {
		t.Errorf("hint should explain why the guarded copy does not count: %q", diags[0].Hint)
	}
}

// An unconditional hand-written conjunct satisfies the invariant.
func TestEnforce_SkeletonConjunctPasses(t *testing.T) {
	src := "-- name: Q :many\nSELECT o.id FROM orders o WHERE o.tenant_id = :tenant_id AND o.ok\n"
	q := scanOne(t, src)
	if diags := enforceOn(t, q, tenantPolicy()); len(diags) != 0 {
		t.Errorf("unexpected diagnostics: %+v", diags)
	}
}

// The woven output of Weave must always satisfy Enforce — the two
// passes agreeing is the whole design (§6.1: the check reduces to
// "the conjunct is in the skeleton").
func TestEnforce_AcceptsWeaverOutput(t *testing.T) {
	srcs := []string{
		"-- name: A :many\nSELECT o.id FROM orders o WHERE o.status = :status\n",
		"-- name: B :many\nSELECT id FROM orders ORDER BY id\n",
		"-- name: C :exec\nDELETE FROM orders\n",
		"-- name: D :many\nSELECT a.id FROM orders a JOIN orders b ON a.id = b.parent_id WHERE a.ok\n",
	}
	for _, src := range srcs {
		res := weaveOne(t, src, tenantPolicy())
		noDiags(t, res)
		if diags := enforceOn(t, res.Query, tenantPolicy()); len(diags) != 0 {
			t.Errorf("enforcement rejects the weaver's own output for %q: %+v", src, diags)
		}
	}
}

func TestEnforce_OptOut(t *testing.T) {
	// Honored: the policy applies, the opt-out names it — no 124.
	src := "-- name: Q :many\n-- @policy-optout: tenant_scope (ops report)\nSELECT id FROM orders\n"
	q := scanOne(t, src)
	if diags := enforceOn(t, q, tenantPolicy()); len(diags) != 0 {
		t.Errorf("honored opt-out still diagnosed: %+v", diags)
	}

	// Unknown policy name.
	src = "-- name: Q :many\n-- @policy-optout: tnant_scope (typo)\nSELECT id FROM orders\n"
	q = scanOne(t, src)
	diags := enforceOn(t, q, tenantPolicy())
	if len(diags) != 2 {
		t.Fatalf("want SQLETCH126 + SQLETCH124, got %+v", diags)
	}
	codes := map[diagnostics.Code]bool{}
	for _, d := range diags {
		codes[d.Code] = true
	}
	if !codes[diagnostics.CodePolicyBadOptOut] || !codes[diagnostics.CodePolicyUnscoped] {
		t.Errorf("a typoed opt-out must both flag the opt-out and keep the query unscoped: %+v", diags)
	}

	// Policy exists but does not apply to this query.
	src = "-- name: Q :many\n-- @policy-optout: tenant_scope (pointless)\nSELECT id FROM users\n"
	q = scanOne(t, src)
	diags = enforceOn(t, q, tenantPolicy())
	if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyBadOptOut {
		t.Fatalf("want exactly SQLETCH126, got %+v", diags)
	}
}

// Weaver side of opt-out: an honored opt-out suppresses weaving and
// the SQLETCH125 rejections alike, and is recorded for explain.
func TestWeave_OptOut(t *testing.T) {
	src := "-- name: Q :many\n-- @policy-optout: tenant_scope (batch)\nSELECT id FROM orders WHERE ok\n"
	q := scanOne(t, src)
	res := Weave(postgres.Profile{}, postgres.Frontend{}, []Policy{tenantPolicy()}, q)
	noDiags(t, res)
	if res.Query != q {
		t.Errorf("opted-out query was rewritten")
	}
	if len(res.Woven) != 1 || !res.Woven[0].OptedOut || res.Woven[0].OptOutReason != "batch" {
		t.Errorf("opt-out not recorded for explain: %+v", res.Woven)
	}

	// Even an unweavable position stays quiet under an opt-out.
	src = "-- name: Q :many\n-- @policy-optout: tenant_scope (report)\n" +
		"SELECT id FROM reports WHERE order_id IN (SELECT id FROM orders)\n"
	q = scanOne(t, src)
	res = Weave(postgres.Profile{}, postgres.Frontend{}, []Policy{tenantPolicy()}, q)
	noDiags(t, res)
	if res.Query != q {
		t.Errorf("opted-out query was rewritten")
	}
}

// Enforce must consume the same statement-kind gates as the weaver.
func TestEnforce_KindGates(t *testing.T) {
	p := tenantPolicy()
	p.Kinds = []dialect.StmtKind{dialect.StmtSelect}
	q := scanOne(t, "-- name: Q :exec\nDELETE FROM orders\n")
	if diags := enforceOn(t, q, p); len(diags) != 0 {
		t.Errorf("kind-excluded query diagnosed: %+v", diags)
	}
	q = scanOne(t, "-- name: Q :exec\nINSERT INTO orders (id) VALUES (:id)\n")
	if diags := enforceOn(t, q, tenantPolicy()); len(diags) != 0 {
		t.Errorf("INSERT VALUES diagnosed: %+v", diags)
	}
}
