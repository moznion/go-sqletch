package cli

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
)

func TestOverrideHygieneDiags(t *testing.T) {
	cfg := config.Config{Path: "sqletch.yaml"}
	desc := dialect.Desc{Columns: []dialect.ColumnDesc{
		{Name: "id"}, {Name: "email"}, {Name: "email"},
	}}

	diags := overrideHygieneDiags(cfg, "Q", map[string]bool{
		"id":     true,  // fine: exactly one match, no warning
		"email":  false, // ambiguous: two result columns share the name
		"emial":  true,  // dead: typo matches nothing
		"status": true,  // dead: not projected
	}, desc)

	wantCodes := []diagnostics.Code{
		diagnostics.CodeOverrideAmbiguousColumn, // email (sorted order)
		diagnostics.CodeOverrideUnknownColumn,   // emial
		diagnostics.CodeOverrideUnknownColumn,   // status
	}
	if len(diags) != len(wantCodes) {
		t.Fatalf("diags = %+v, want %d warnings", diags, len(wantCodes))
	}
	for i, want := range wantCodes {
		if diags[i].Code != want {
			t.Errorf("diags[%d].Code = %s, want %s (%s)", i, diags[i].Code, want, diags[i].Message)
		}
		if diags[i].Severity != diagnostics.Warning {
			t.Errorf("diags[%d] severity = %v, want warning", i, diags[i].Severity)
		}
		if diags[i].Span.File != "sqletch.yaml" {
			t.Errorf("diags[%d] span = %+v, want the config file", i, diags[i].Span)
		}
	}

	if d := overrideHygieneDiags(cfg, "Q", nil, desc); d != nil {
		t.Errorf("no overrides must produce no diags, got %+v", d)
	}
}
