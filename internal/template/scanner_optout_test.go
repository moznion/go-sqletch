package template

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

func TestScan_PolicyOptOut(t *testing.T) {
	src := "-- name: Q :many\n" +
		"-- @policy-optout: tenant_scope (batch job; runs outside any tenant)\n" +
		"-- @policy-optout: soft_delete (backfill sees deleted rows)\n" +
		"SELECT id FROM orders\n"
	f := scanClean(t, src)
	q := f.Queries[0]
	if len(q.PolicyOptOuts) != 2 {
		t.Fatalf("got %d opt-outs", len(q.PolicyOptOuts))
	}
	if o := q.PolicyOptOuts[0]; o.Policy != "tenant_scope" || o.Reason != "batch job; runs outside any tenant" {
		t.Errorf("first opt-out = %+v", o)
	}
	if o := q.PolicyOptOuts[1]; o.Policy != "soft_delete" || o.Reason != "backfill sees deleted rows" {
		t.Errorf("second opt-out = %+v", o)
	}
	if o := q.PolicyOptOuts[0]; o.Span.Start == 0 || o.Span.End <= o.Span.Start {
		t.Errorf("opt-out span not recorded: %+v", o.Span)
	}
}

// The reason is mandatory: it is what makes the annotation
// self-documenting in review and in the explain report.
func TestScan_PolicyOptOutMalformed(t *testing.T) {
	cases := []string{
		"-- name: Q :many\n-- @policy-optout: tenant_scope\nSELECT id FROM orders\n",
		"-- name: Q :many\n-- @policy-optout tenant_scope (x)\nSELECT id FROM orders\n",
		"-- name: Q :many\n-- @policy-optout: tenant_scope ()\nSELECT id FROM orders\n",
		"-- name: Q :many\n-- @policy-optout: Tenant-Scope (reason)\nSELECT id FROM orders\n",
	}
	for _, src := range cases {
		f, diags := scan(t, src)
		if !hasCode(diags, diagnostics.CodeConstructGrammar) {
			t.Errorf("no SQLETCH001 for %q: %+v", src, diags)
		}
		if len(f.Queries) == 1 && len(f.Queries[0].PolicyOptOuts) != 0 {
			t.Errorf("malformed opt-out was recorded: %+v", f.Queries[0].PolicyOptOuts)
		}
	}
}

// The comment stays in the skeleton verbatim, like every annotation.
func TestScan_PolicyOptOutStaysInSkeleton(t *testing.T) {
	src := "-- name: Q :many\n-- @policy-optout: tenant_scope (why not)\nSELECT id FROM orders\n"
	f := scanClean(t, src)
	var text string
	for _, it := range f.Queries[0].Items {
		if s, ok := it.(*Skeleton); ok {
			text += s.Text
		}
	}
	if !strings.Contains(text, "@policy-optout: tenant_scope") {
		t.Errorf("annotation excised from skeleton:\n%s", text)
	}
}
