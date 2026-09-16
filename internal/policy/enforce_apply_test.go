package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// §12.4: `-- @policy-apply` is subject to the same sanity rules as
// `-- @policy-optout` and shares SQLETCH126. Renaming a policy must
// never silently disarm the annotations that mention it — an
// acknowledgment left behind by a rename tells a reviewer the query is
// scoped by a policy that no longer exists.
func TestEnforce_ApplyNamesUnknownPolicy(t *testing.T) {
	src := "-- name: Q :many\n-- @policy-apply: renamed_scope\n" +
		"SELECT o.id FROM orders o WHERE o.tenant_id = :tenant_id\n"
	q := scanOne(t, src)
	diags := enforceOn(t, q, tenantPolicy())
	if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyBadOptOut {
		t.Fatalf("want exactly one SQLETCH126, got %+v", diags)
	}
	if !strings.Contains(diags[0].Message, "renamed_scope") {
		t.Errorf("message does not name the annotation's policy: %q", diags[0].Message)
	}
	if !strings.Contains(diags[0].Message, "@policy-apply") {
		t.Errorf("message does not name the annotation: %q", diags[0].Message)
	}
}

// An acknowledgment on a query the policy does not touch is equally
// misleading: it claims a scoping that is not happening.
func TestEnforce_ApplyOnInapplicableQuery(t *testing.T) {
	src := "-- name: Q :many\n-- @policy-apply: tenant_scope\nSELECT u.id FROM users u\n"
	q := scanOne(t, src)
	diags := enforceOn(t, q, tenantPolicy())
	if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyBadOptOut {
		t.Fatalf("want exactly one SQLETCH126, got %+v", diags)
	}
	if !strings.Contains(diags[0].Message, "does not apply") {
		t.Errorf("message = %q", diags[0].Message)
	}
}

// A query cannot be both acknowledged and exempt. Silently preferring
// one would make the annotation pair unreviewable: the diff would read
// as scoped while the compiler exempted it (or the reverse).
func TestEnforce_ApplyAndOptOutTogether(t *testing.T) {
	src := "-- name: Q :many\n-- @policy-apply: tenant_scope\n" +
		"-- @policy-optout: tenant_scope (batch job)\n" +
		"SELECT o.id FROM orders o\n"
	q := scanOne(t, src)
	diags := enforceOn(t, q, tenantPolicy())
	if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyBadOptOut {
		t.Fatalf("want exactly one SQLETCH126, got %+v", diags)
	}
	if !strings.Contains(diags[0].Message, "both") {
		t.Errorf("message = %q", diags[0].Message)
	}
}

// A well-formed acknowledgment on the weaver's own output is silent,
// and does not disturb the SQLETCH124 re-derivation: the query is
// still proved scoped from the woven template.
func TestEnforce_AcceptsAcknowledgedWeaverOutput(t *testing.T) {
	src := "-- name: Q :many\n-- @policy-apply: tenant_scope\n" +
		"SELECT o.id FROM orders o WHERE o.status = :status\n"
	res := weaveOne(t, src, requiringTenantPolicy())
	noDiags(t, res)
	if diags := enforceOn(t, res.Query, requiringTenantPolicy()); len(diags) != 0 {
		t.Errorf("unexpected diagnostics: %+v", diags)
	}
}

// The acknowledgment is not a substitute for the conjunct. SQLETCH124
// re-derives presence from the template, so a hand-written query whose
// only scoping lives inside @if-present still fails — saying "this is
// scoped" must never be able to make it so.
func TestEnforce_ApplyDoesNotSatisfyTheConjunct(t *testing.T) {
	src := "-- name: Q :many\n-- @policy-apply: tenant_scope\n" +
		"SELECT o.id FROM orders o WHERE o.ok\n" +
		"@if-present(tenant_id)\nAND o.tenant_id = :tenant_id\n@endif\n"
	q := scanOne(t, src)
	diags := enforceOn(t, q, requiringTenantPolicy())
	if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
		t.Fatalf("want exactly one SQLETCH124, got %+v", diags)
	}
}

// Sanity applies whether or not the policy sets require_annotation:
// a stale acknowledgment is misleading either way.
func TestEnforce_ApplySanityIndependentOfRequireAnnotation(t *testing.T) {
	src := "-- name: Q :many\n-- @policy-apply: tenant_scope\nSELECT u.id FROM users u\n"
	q := scanOne(t, src)
	for _, p := range []Policy{tenantPolicy(), requiringTenantPolicy()} {
		diags := enforceOn(t, q, p)
		if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyBadOptOut {
			t.Errorf("require_annotation=%v: want one SQLETCH126, got %+v", p.RequireAnnotation, diags)
		}
	}
}
