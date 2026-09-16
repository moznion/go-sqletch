package template

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

func TestScan_PolicyApply(t *testing.T) {
	src := "-- name: Q :many\n" +
		"-- @policy-apply: tenant_scope\n" +
		"-- @policy-apply: soft_delete (kept for the dashboard's live rows)\n" +
		"SELECT id FROM orders\n"
	f := scanClean(t, src)
	q := f.Queries[0]
	if len(q.PolicyApplies) != 2 {
		t.Fatalf("got %d acknowledgments", len(q.PolicyApplies))
	}
	// The reason is optional here: an acknowledgment claims no
	// exemption, so there is nothing to justify.
	if a := q.PolicyApplies[0]; a.Policy != "tenant_scope" || a.Reason != "" {
		t.Errorf("first acknowledgment = %+v", a)
	}
	if a := q.PolicyApplies[1]; a.Policy != "soft_delete" || a.Reason != "kept for the dashboard's live rows" {
		t.Errorf("second acknowledgment = %+v", a)
	}
	if a := q.PolicyApplies[0]; a.Span.Start == 0 || a.Span.End <= a.Span.Start {
		t.Errorf("acknowledgment span not recorded: %+v", a.Span)
	}
}

// Malformed shapes are rejected rather than ignored: a typo that
// silently recorded nothing would leave the query looking
// unannotated, and the author would see SQLETCH127 pointing at a
// query they believe they annotated.
func TestScan_PolicyApplyMalformed(t *testing.T) {
	cases := []string{
		"-- name: Q :many\n-- @policy-apply\nSELECT id FROM orders\n",
		"-- name: Q :many\n-- @policy-apply tenant_scope\nSELECT id FROM orders\n",
		"-- name: Q :many\n-- @policy-apply: Tenant-Scope\nSELECT id FROM orders\n",
		"-- name: Q :many\n-- @policy-apply: tenant_scope ()\nSELECT id FROM orders\n",
		"-- name: Q :many\n-- @policy-apply: tenant_scope trailing words\nSELECT id FROM orders\n",
	}
	for _, src := range cases {
		f, diags := scan(t, src)
		if !hasCode(diags, diagnostics.CodeConstructGrammar) {
			t.Errorf("no SQLETCH001 for %q: %+v", src, diags)
		}
		if len(f.Queries) == 1 && len(f.Queries[0].PolicyApplies) != 0 {
			t.Errorf("malformed acknowledgment was recorded: %+v", f.Queries[0].PolicyApplies)
		}
	}
}

// `@policy-apply` must not be mistaken for `@policy-optout`'s prefix
// or vice versa: they are distinct directives with distinct forms.
func TestScan_PolicyApplyAndOptOutAreDistinct(t *testing.T) {
	src := "-- name: Q :many\n" +
		"-- @policy-apply: tenant_scope\n" +
		"-- @policy-optout: soft_delete (backfill)\n" +
		"SELECT id FROM orders\n"
	q := scanClean(t, src).Queries[0]
	if len(q.PolicyApplies) != 1 || q.PolicyApplies[0].Policy != "tenant_scope" {
		t.Errorf("applies = %+v", q.PolicyApplies)
	}
	if len(q.PolicyOptOuts) != 1 || q.PolicyOptOuts[0].Policy != "soft_delete" {
		t.Errorf("opt-outs = %+v", q.PolicyOptOuts)
	}
}

// The comment stays in the skeleton verbatim, like every annotation —
// which is why adding one re-keys the query's oracle entries
// (design 14 §12.8).
func TestScan_PolicyApplyStaysInSkeleton(t *testing.T) {
	src := "-- name: Q :many\n-- @policy-apply: tenant_scope\nSELECT id FROM orders\n"
	f := scanClean(t, src)
	var text string
	for _, it := range f.Queries[0].Items {
		if s, ok := it.(*Skeleton); ok {
			text += s.Text
		}
	}
	if !strings.Contains(text, "@policy-apply: tenant_scope") {
		t.Errorf("annotation excised from skeleton:\n%s", text)
	}
}
