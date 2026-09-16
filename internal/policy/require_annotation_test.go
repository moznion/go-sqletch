package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

func requiringTenantPolicy() Policy {
	p := tenantPolicy()
	p.RequireAnnotation = true
	return p
}

// The bare requirement: an applicable query with neither annotation.
func TestRequireAnnotation_MissingIsUnannotated(t *testing.T) {
	res := weaveOne(t, "-- name: Q :many\nSELECT o.id FROM orders o WHERE o.status = :status\n",
		requiringTenantPolicy())
	if !hasCode(res.Diags, diagnostics.CodePolicyUnannotated) {
		t.Fatalf("no SQLETCH127: %+v", res.Diags)
	}
}

// §12.2: the annotation is an acknowledgment, never a switch. The
// conjunct is woven whether or not it is present — including in the
// SQLETCH127 case, so that a query which fails the requirement fails
// SCOPED. A diagnostic that also silently unscoped the query would
// make the safe direction depend on the caller honoring the error.
func TestRequireAnnotation_UnannotatedQueryIsStillWoven(t *testing.T) {
	res := weaveOne(t, "-- name: Q :many\nSELECT o.id FROM orders o WHERE o.status = :status\n",
		requiringTenantPolicy())
	got := renderSQL(t, res.Query)
	if !strings.Contains(got, "o.tenant_id = $1") {
		t.Errorf("unannotated query was left unscoped:\n%s", got)
	}
	if len(res.Woven) != 1 || res.Woven[0].OptedOut {
		t.Errorf("coverage record = %+v", res.Woven)
	}
}

func TestRequireAnnotation_ApplySatisfies(t *testing.T) {
	res := weaveOne(t, "-- name: Q :many\n-- @policy-apply: tenant_scope\nSELECT o.id FROM orders o WHERE o.status = :status\n",
		requiringTenantPolicy())
	noDiags(t, res)
	got := renderSQL(t, res.Query)
	if !strings.Contains(got, "o.tenant_id = $1") {
		t.Errorf("acknowledged query not woven:\n%s", got)
	}
}

func TestRequireAnnotation_ApplyWithReasonSatisfies(t *testing.T) {
	res := weaveOne(t, "-- name: Q :many\n-- @policy-apply: tenant_scope (dashboard is per-tenant)\nSELECT o.id FROM orders o WHERE o.status = :status\n",
		requiringTenantPolicy())
	noDiags(t, res)
}

func TestRequireAnnotation_OptOutSatisfies(t *testing.T) {
	res := weaveOne(t, "-- name: Q :many\n-- @policy-optout: tenant_scope (batch job)\nSELECT o.id FROM orders o WHERE o.status = :status\n",
		requiringTenantPolicy())
	noDiags(t, res)
	if len(res.Woven) != 1 || !res.Woven[0].OptedOut {
		t.Errorf("coverage record = %+v", res.Woven)
	}
}

// §12.3: the obligation uses the applicability predicate, so a query
// the policy does not touch owes nothing.
func TestRequireAnnotation_InapplicableQueryOwesNothing(t *testing.T) {
	res := weaveOne(t, "-- name: Q :many\nSELECT u.id FROM users u WHERE u.status = :status\n",
		requiringTenantPolicy())
	noDiags(t, res)
}

// An INSERT … VALUES filters no rows, so no policy applies and no
// annotation is owed — the same carve-out weaving already has.
func TestRequireAnnotation_InsertValuesOwesNothing(t *testing.T) {
	res := weaveOne(t, "-- name: Q :exec\nINSERT INTO orders (tenant_id, status) VALUES (:tenant_id, :status)\n",
		requiringTenantPolicy())
	noDiags(t, res)
}

// A policy without the key never contributes an obligation, so it can
// be rolled out one policy at a time.
func TestRequireAnnotation_OffByDefault(t *testing.T) {
	res := weaveOne(t, "-- name: Q :many\nSELECT o.id FROM orders o WHERE o.status = :status\n",
		tenantPolicy())
	noDiags(t, res)
}

// §12.3: per policy, not per query. Two requiring policies on one
// query owe two annotations, and satisfying one leaves the other
// outstanding.
func TestRequireAnnotation_PerPolicy(t *testing.T) {
	soft := Policy{
		Name:              "soft_delete",
		Tables:            []string{"orders"},
		Predicate:         "{}.deleted_at IS NULL",
		RequireAnnotation: true,
	}
	res := weaveOne(t, "-- name: Q :many\n-- @policy-apply: tenant_scope\nSELECT o.id FROM orders o WHERE o.status = :status\n",
		requiringTenantPolicy(), soft)
	var unannotated []diagnostics.Diagnostic
	for _, d := range res.Diags {
		if d.Code == diagnostics.CodePolicyUnannotated {
			unannotated = append(unannotated, d)
		}
	}
	if len(unannotated) != 1 {
		t.Fatalf("want exactly one SQLETCH127, got %d: %+v", len(unannotated), res.Diags)
	}
	if !strings.Contains(unannotated[0].Message, "soft_delete") {
		t.Errorf("diagnostic names the wrong policy: %q", unannotated[0].Message)
	}
}

// The message must name the policy and the hint must spell both
// escape hatches: the author cannot act on "annotate this" alone.
func TestRequireAnnotation_DiagnosticSpellsTheFix(t *testing.T) {
	res := weaveOne(t, "-- name: Q :many\nSELECT o.id FROM orders o WHERE o.status = :status\n",
		requiringTenantPolicy())
	var d diagnostics.Diagnostic
	for _, c := range res.Diags {
		if c.Code == diagnostics.CodePolicyUnannotated {
			d = c
		}
	}
	if !strings.Contains(d.Message, "tenant_scope") {
		t.Errorf("message does not name the policy: %q", d.Message)
	}
	if !strings.Contains(d.Hint, "@policy-apply") || !strings.Contains(d.Hint, "@policy-optout") {
		t.Errorf("hint does not spell both annotations: %q", d.Hint)
	}
	// The query is scoped either way; a message implying otherwise
	// would send the author looking for a leak that is not there.
	if strings.Contains(strings.ToLower(d.Message), "unscoped") {
		t.Errorf("message misreports the query as unscoped: %q", d.Message)
	}
}

// An unweavable position is still SQLETCH125 first: restructuring or
// opting out is the author's next move, and an opt-out discharges the
// annotation obligation too.
func TestRequireAnnotation_UnweavableStillReportsUnweavable(t *testing.T) {
	res := weaveOne(t, "-- name: Q :many\nSELECT u.id FROM users u WHERE u.id IN (SELECT o.user_id FROM orders o)\n",
		requiringTenantPolicy())
	if !hasCode(res.Diags, diagnostics.CodePolicyUnweavable) {
		t.Fatalf("no SQLETCH125: %+v", res.Diags)
	}
	if !hasCode(res.Diags, diagnostics.CodePolicyUnannotated) {
		t.Fatalf("no SQLETCH127 alongside it: %+v", res.Diags)
	}
	if res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
		t.Errorf("SQLETCH125 must come first, got %s", res.Diags[0].Code)
	}
}
