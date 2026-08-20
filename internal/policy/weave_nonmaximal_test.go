package policy

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

// A designated table read ONLY inside a non-first @choose alternative is
// invisible to the maximal rendering the weaver scopes from. Before the
// fix it shipped completely unscoped (no weave, no diagnostic, a silent
// tenant-scoping leak). It must now be refused with SQLETCH125.
//
// @choose in this position is an ORDER BY / GROUP BY slot, so the
// designated read hides in an ORDER BY scalar subquery of a non-first
// case body.
func TestWeave_NonMaximalChooseAlternative_Refused(t *testing.T) {
	src := "-- name: Q :many\n" +
		"SELECT u.id FROM users u WHERE u.active\n" +
		"@choose(sort)\n" +
		"@case(by_id)\n" +
		"ORDER BY u.id\n" +
		"@case(by_orders)\n" +
		"ORDER BY (SELECT count(*) FROM orders o2 WHERE o2.user_id = u.id) DESC\n" +
		"@end\n"
	res := weaveOne(t, src, tenantPolicy())
	if !hasCode(res.Diags, diagnostics.CodePolicyUnweavable) {
		t.Fatalf("expected SQLETCH125 for orders read only in a non-first @choose case; diags=%+v", res.Diags)
	}
}

// The same leak via the @choose @default body (also a non-maximal
// rendering) must be refused.
func TestWeave_NonMaximalChooseDefault_Refused(t *testing.T) {
	src := "-- name: Q :many\n" +
		"SELECT u.id FROM users u WHERE u.active\n" +
		"@choose(sort)\n" +
		"@case(by_id)\n" +
		"ORDER BY u.id\n" +
		"@default\n" +
		"ORDER BY (SELECT count(*) FROM orders o2 WHERE o2.user_id = u.id) DESC\n" +
		"@end\n"
	res := weaveOne(t, src, tenantPolicy())
	if !hasCode(res.Diags, diagnostics.CodePolicyUnweavable) {
		t.Fatalf("expected SQLETCH125 for orders read only in the @choose @default; diags=%+v", res.Diags)
	}
}

// An explicit opt-out silences the refusal, exactly as it silences the
// ordinary unweavable diagnostics.
func TestWeave_NonMaximalChoose_OptOutHonored(t *testing.T) {
	src := "-- name: Q :many\n" +
		"-- @policy-optout: tenant_scope (analytics counter, tenant-agnostic)\n" +
		"SELECT u.id FROM users u WHERE u.active\n" +
		"@choose(sort)\n" +
		"@case(by_id)\n" +
		"ORDER BY u.id\n" +
		"@case(by_orders)\n" +
		"ORDER BY (SELECT count(*) FROM orders o2 WHERE o2.user_id = u.id) DESC\n" +
		"@end\n"
	res := weaveOne(t, src, tenantPolicy())
	if hasCode(res.Diags, diagnostics.CodePolicyUnweavable) {
		t.Fatalf("opt-out must silence the non-maximal refusal; diags=%+v", res.Diags)
	}
}

// A designated table already present (and scoped) in the maximal
// rendering, read identically in every @choose alternative, must NOT be
// falsely refused — the shared woven WHERE conjunct scopes it in all
// renderings.
func TestWeave_MaximalDesignatedInEveryAlternative_NotRefused(t *testing.T) {
	src := "-- name: Q :many\n" +
		"SELECT o.id FROM orders o WHERE o.status = :status\n" +
		"@choose(sort)\n" +
		"@case(a)\n" +
		"ORDER BY o.a\n" +
		"@case(b)\n" +
		"ORDER BY o.b\n" +
		"@end\n"
	res := weaveOne(t, src, tenantPolicy())
	if hasCode(res.Diags, diagnostics.CodePolicyUnweavable) {
		t.Fatalf("orders is a scoped top-level relation in every rendering; must not be refused; diags=%+v", res.Diags)
	}
	if len(res.Woven) != 1 || len(res.Woven[0].Conjuncts) != 1 {
		t.Fatalf("expected orders woven once; Woven=%+v", res.Woven)
	}
}

func hasCode(diags []diagnostics.Diagnostic, code diagnostics.Code) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

// Keep the postgres import referenced even if the helpers move.
var _ = postgres.Profile{}
