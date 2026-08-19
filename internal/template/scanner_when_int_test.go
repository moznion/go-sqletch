package template

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// whenIntSrc wraps a bare @when integer literal in a minimal query.
func whenIntSrc(literal string) string {
	return "-- name: Q :many\nSELECT 1 FROM t WHERE TRUE\n" +
		"@when(status = " + literal + ")\n  AND t.flag\n@end\n;\n"
}

func TestScan_When_IntLiteral_Rejected(t *testing.T) {
	cases := []struct {
		name    string
		literal string
	}{
		{"leading_zero", "010"},
		{"leading_zero_invalid_octal", "08"},
		{"all_zeros", "00"},
		{"overflow_int64", "99999999999999999999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := scan(t, whenIntSrc(tc.literal))
			if !hasCode(diags, diagnostics.CodeWhenIntLiteral) {
				t.Fatalf("want %s for @when literal %q, got %+v",
					diagnostics.CodeWhenIntLiteral, tc.literal, diags)
			}
			// The diagnostic must carry a compliant-rewrite hint.
			var withHint bool
			for _, d := range diags {
				if d.Code == diagnostics.CodeWhenIntLiteral && strings.TrimSpace(d.Hint) != "" {
					withHint = true
				}
			}
			if !withHint {
				t.Errorf("%s diagnostic must carry a hint, got %+v", diagnostics.CodeWhenIntLiteral, diags)
			}
		})
	}
}

func TestScan_When_IntLiteral_Accepted(t *testing.T) {
	for _, literal := range []string{"0", "10", "42", "9223372036854775807"} {
		t.Run(literal, func(t *testing.T) {
			f := scanClean(t, whenIntSrc(literal))
			q := f.Queries[0]
			var atom *GuardAtom
			for _, it := range q.Items {
				if ip, ok := it.(*IfPresent); ok && len(ip.Guards) == 1 {
					atom = &ip.Guards[0]
				}
			}
			if atom == nil {
				t.Fatalf("no @when guard atom parsed for %q", literal)
			}
			if atom.Kind != ValueInt || atom.Value != literal {
				t.Errorf("atom = %+v, want ValueInt %q", atom, literal)
			}
		})
	}
}
