package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// Audit-12 M9: a policy predicate may reference ONLY the designated
// relation (via {}) and its own :params (spec §Cross-Query Policies).
// A predicate that reads ANOTHER relation through a subquery ships that
// read UNSCOPED with zero diagnostics: Weave analyzes the unwoven
// template and Enforce the woven one, so a relation introduced BY the
// predicate is invisible to both. Validation must reject it (SQLETCH303).
func TestValidate_RejectsPredicateReadingAnotherRelation(t *testing.T) {
	p := tenantPolicy()
	p.Predicate = "EXISTS (SELECT 1 FROM scopes WHERE scopes.sid = {}.sid AND scopes.tenant_id = :tenant_id)"
	diags := validateOne(t, p)
	if len(diags) == 0 {
		t.Fatal("expected SQLETCH303 for a predicate reading another relation, got none")
	}
	found := false
	for _, d := range diags {
		if d.Code != diagnostics.CodePolicyInvalid {
			t.Errorf("code = %s, want SQLETCH303", d.Code)
		}
		if strings.Contains(d.Message, "scopes") || strings.Contains(d.Message, "designated relation") {
			found = true
		}
	}
	if !found {
		t.Errorf("no diagnostic names the offending relation: %+v", diags)
	}
}

// A normal column/param predicate stays valid, and — per the spec's
// "only {} and its params" restriction implemented as "introduces no
// relation" — a FROM-less scalar subquery introduces no relation and is
// permitted, while an EXTRACT(... FROM ...) function (which lexically
// contains FROM but reads no table) must not be false-rejected.
func TestValidate_RelationRestrictionAllowsRelationFreePredicates(t *testing.T) {
	ok := []struct {
		name string
		pred string
	}{
		{"plain column and param", "{}.tenant_id = :tenant_id"},
		{"from-less scalar subquery", "{}.rank = (SELECT :tenant_id)"},
		{"extract with FROM keyword", "EXTRACT(DAY FROM {}.created_at) = :tenant_id"},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			p := tenantPolicy()
			p.Predicate = tc.pred
			if diags := validateOne(t, p); len(diags) != 0 {
				t.Errorf("unexpected diagnostics for %q: %+v", tc.pred, diags)
			}
		})
	}
}
