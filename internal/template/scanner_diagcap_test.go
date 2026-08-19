package template

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// A file that is nothing but positional params must not emit one
// SQLETCH011 per token (millions of structs / gigabytes before
// rendering was DoS-able). The per-code cap bounds the count and a
// single summary diagnostic reports the suppressed remainder.
func TestPositionalParamDiagnosticsAreCapped(t *testing.T) {
	var b strings.Builder
	b.WriteString("-- name: Q :many\nSELECT 1 WHERE 1 IN (")
	const n = 5000
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("$1")
	}
	b.WriteString(")\n")

	_, diags := scan(t, b.String())

	var got int
	for _, d := range diags {
		if d.Code == diagnostics.CodePositionalParam {
			got++
		}
	}
	// maxDiagsPerCode individual diagnostics + one summary.
	if got > maxDiagsPerCode+1 {
		t.Fatalf("emitted %d SQLETCH011 diagnostics, want <= %d (cap + summary)", got, maxDiagsPerCode+1)
	}
	if got < maxDiagsPerCode {
		t.Fatalf("emitted only %d SQLETCH011 diagnostics, expected the cap to be reached", got)
	}
}

// The cap must not perturb ordinary output: a handful of positional
// params below the cap are reported one-for-one, with no summary.
func TestPositionalParamBelowCapUnchanged(t *testing.T) {
	_, diags := scan(t, "-- name: Q :many\nSELECT $1, $2, $3\n")
	var got int
	for _, d := range diags {
		if d.Code == diagnostics.CodePositionalParam {
			got++
		}
	}
	if got != 3 {
		t.Fatalf("emitted %d SQLETCH011 diagnostics, want exactly 3 (no cap, no summary)", got)
	}
}
