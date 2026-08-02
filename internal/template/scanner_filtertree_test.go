package template

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// The conjunct-anchor discipline: @filter-tree must directly follow an
// unconditional `AND` and must end its conjunct. The empty tree renders
// TRUE, which is only sound when it substitutes one whole AND-conjunct
// — under OR, NOT, or an expression continuation the guard would
// silently vanish or change meaning.
func TestScan_FilterTree_ConjunctAnchor(t *testing.T) {
	block := "@filter-tree(s)\n@predicate(x)\nt.a = :a\n@end"
	tests := []struct {
		name, src string
	}{
		{
			name: "directly after WHERE",
			src:  "-- name: Bad :many\nSELECT 1 FROM t WHERE " + block + "\n;",
		},
		{
			name: "after OR",
			src:  "-- name: Bad :many\nSELECT 1 FROM t WHERE t.b = 1\n  OR " + block + "\n;",
		},
		{
			name: "after AND NOT",
			src:  "-- name: Bad :many\nSELECT 1 FROM t WHERE TRUE\n  AND NOT " + block + "\n;",
		},
		{
			name: "directly after another construct",
			src: "-- name: Bad :many\nSELECT 1 FROM t WHERE TRUE\n" +
				"@if-present(y)\n  AND t.y = :y\n@endif\n" + block + "\n;",
		},
		{
			name: "followed by OR",
			src:  "-- name: Bad :many\nSELECT 1 FROM t WHERE TRUE\n  AND " + block + " OR t.b = 1\n;",
		},
		{
			name: "followed by an expression continuation",
			src:  "-- name: Bad :many\nSELECT 1 FROM t WHERE TRUE\n  AND " + block + " = TRUE\n;",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, diags := scan(t, tt.src)
			if !hasCode(diags, diagnostics.CodeConjunctNeedsAnd) {
				t.Errorf("want %s, got %+v", diagnostics.CodeConjunctNeedsAnd, diags)
			}
		})
	}
}

func TestScan_FilterTree_ConjunctAnchor_Valid(t *testing.T) {
	block := "@filter-tree(s)\n@predicate(x)\nt.a = :a\n@end"
	tests := []struct {
		name, src string
	}{
		{
			name: "followed by semicolon",
			src:  "-- name: Ok :many\nSELECT 1 FROM t WHERE TRUE\n  AND " + block + "\n;",
		},
		{
			name: "followed by end of input",
			src:  "-- name: Ok :many\nSELECT 1 FROM t WHERE TRUE\n  AND " + block,
		},
		{
			name: "followed by AND",
			src:  "-- name: Ok :many\nSELECT 1 FROM t WHERE TRUE\n  AND " + block + "\n  AND t.b = 1\n;",
		},
		{
			name: "followed by ORDER BY",
			src:  "-- name: Ok :many\nSELECT 1 FROM t WHERE TRUE\n  AND " + block + "\nORDER BY t.id\n;",
		},
		{
			name: "followed by another construct",
			src: "-- name: Ok :many\nSELECT 1 FROM t WHERE TRUE\n  AND " + block + "\n" +
				"@if-present(y)\n  AND t.y = :y\n@endif\n;",
		},
		{
			name: "lowercase and",
			src:  "-- name: Ok :many\nselect 1 from t where true\n  and " + block + "\n;",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, diags := scan(t, tt.src)
			if len(diags) != 0 {
				t.Errorf("want clean scan, got %+v", diags)
			}
		})
	}
}
