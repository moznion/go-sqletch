package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/template"
)

// manyCaseTemplate scans to one query whose @choose block expands to
// 1 + cases verification renderings.
func manyCaseTemplate(t *testing.T, cases int) *template.QueryTemplate {
	t.Helper()
	var b strings.Builder
	b.WriteString("-- name: Big :many\nSELECT u.id FROM users AS u\nWHERE TRUE\n@choose(sort)\n")
	for j := 0; j < cases; j++ {
		fmt.Fprintf(&b, "@case(c%d)\nORDER BY u.id, %d\n", j, j)
	}
	b.WriteString("@default\nORDER BY u.id ASC\n@end\nLIMIT :limit;\n")
	drv := driverFor(config.Config{Dialect: "postgres"})
	file, diags := template.NewScanner(drv.profile).ScanFile("t.sql", []byte(b.String()))
	if diagnostics.HasErrors(diags) || len(file.Queries) != 1 {
		t.Fatalf("template must scan cleanly: %v", diags)
	}
	return file.Queries[0]
}

// A template whose verification rendering set exceeds the budget is
// refused with SQLETCH302 and its renderings are NEVER materialised, so
// a crafted many-block template cannot OOM `sqletch check`/the LSP.
func TestScanChecks_RenderingBudgetRefused(t *testing.T) {
	drv := driverFor(config.Config{Dialect: "postgres"})
	q := manyCaseTemplate(t, 50) // 1 + 50 = 51 renderings

	_, rs, diags, err := scanChecks(drv, nil, q, 10)
	if err != nil {
		t.Fatalf("scanChecks returned a hard error, want a diagnostic: %v", err)
	}
	if rs != nil {
		t.Fatalf("renderings were materialised (%d) despite exceeding the budget", len(rs))
	}
	if !hasCode(diags, diagnostics.CodeExpansionLarge) {
		t.Fatalf("want %s, got %+v", diagnostics.CodeExpansionLarge, diags)
	}
}

// A template within the budget renders normally — the cap never trips a
// legitimate query.
func TestScanChecks_RenderingBudgetAdmitted(t *testing.T) {
	drv := driverFor(config.Config{Dialect: "postgres"})
	q := manyCaseTemplate(t, 50) // 51 renderings

	_, rs, diags, err := scanChecks(drv, nil, q, 4096)
	if err != nil {
		t.Fatalf("scanChecks: %v", err)
	}
	if len(rs) != 51 {
		t.Fatalf("renderings = %d, want 51", len(rs))
	}
	if hasCode(diags, diagnostics.CodeExpansionLarge) {
		t.Fatalf("budget tripped for an in-budget template: %+v", diags)
	}
}

// A non-positive budget disables the check (e.g. a zero-value config).
func TestScanChecks_RenderingBudgetDisabled(t *testing.T) {
	drv := driverFor(config.Config{Dialect: "postgres"})
	q := manyCaseTemplate(t, 50)

	_, rs, diags, err := scanChecks(drv, nil, q, 0)
	if err != nil {
		t.Fatalf("scanChecks: %v", err)
	}
	if len(rs) != 51 {
		t.Fatalf("renderings = %d, want 51", len(rs))
	}
	if hasCode(diags, diagnostics.CodeExpansionLarge) {
		t.Fatalf("budget tripped when disabled: %+v", diags)
	}
}
