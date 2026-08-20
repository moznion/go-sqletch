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

// The cap must hold for EVERY code, not only SQLETCH011. Several
// emission sites append a hinted diagnostic directly rather than through
// errorf; those sites (SQLETCH008 optional-conjunct-needs-AND here, and
// SQLETCH012 construct-nesting below) are reachable in O(input) counts,
// so if they bypassed the cap they would reopen the same unbounded-slab
// DoS under a different code. countCode returns how many of `code` a
// scan produced.
func countCode(t *testing.T, src string, code diagnostics.Code) int {
	t.Helper()
	_, diags := scan(t, src)
	var got int
	for _, d := range diags {
		if d.Code == code {
			got++
		}
	}
	return got
}

func TestConjunctNeedsAndDiagnosticsAreCapped(t *testing.T) {
	var b strings.Builder
	b.WriteString("-- name: Q :many\nSELECT 1 FROM t WHERE 1 = 1\n")
	const n = 5000
	// Each optional conjunct body omits the leading AND → one
	// SQLETCH008 apiece; unbounded before the cap covered this site.
	for range n {
		b.WriteString("@if-present(a) x = 1 @endif\n")
	}

	got := countCode(t, b.String(), diagnostics.CodeConjunctNeedsAnd)
	if got > maxDiagsPerCode+1 {
		t.Fatalf("emitted %d SQLETCH008 diagnostics, want <= %d (cap + summary)", got, maxDiagsPerCode+1)
	}
	if got < maxDiagsPerCode {
		t.Fatalf("emitted only %d SQLETCH008 diagnostics, expected the cap to be reached", got)
	}
}

func TestConstructNestingDiagnosticsAreCapped(t *testing.T) {
	var b strings.Builder
	b.WriteString("-- name: Q :many\nSELECT 1 FROM t WHERE @if-present(a) 1 = 1\n")
	const n = 5000
	// A nested construct inside the guarded body → one SQLETCH012
	// apiece; this site appends a hinted diagnostic directly.
	for range n {
		b.WriteString("AND @if-present(b) x = 1 @endif\n")
	}
	b.WriteString("@endif\n")

	got := countCode(t, b.String(), diagnostics.CodeConstructNesting)
	if got > maxDiagsPerCode+1 {
		t.Fatalf("emitted %d SQLETCH012 diagnostics, want <= %d (cap + summary)", got, maxDiagsPerCode+1)
	}
	if got < maxDiagsPerCode {
		t.Fatalf("emitted only %d SQLETCH012 diagnostics, expected the cap to be reached", got)
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
