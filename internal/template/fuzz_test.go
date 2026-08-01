package template

import (
	"testing"

	"github.com/moznion/sqletch/internal/dialect/postgres"
)

// FuzzScan asserts the scanner never panics and upholds the span
// invariants of design 01 §9 on arbitrary input: per query, item raw
// spans are contiguous from the header end and within file bounds.
func FuzzScan(f *testing.F) {
	f.Add(useCase1)
	f.Add("-- name: A :one\nSELECT 1;")
	f.Add("-- name: B :many\nSELECT 1 FROM t WHERE TRUE\n@if-present(x)\n  AND t.x = :x\n@endif\n;")
	f.Add("@if-present(")
	f.Add("-- name: C :many\n@choose(s)@case(a)ORDER BY 1@end;")
	f.Add("SELECT 'unterminated")
	f.Add("-- name: D :many\nSELECT $tag$ @endif $tag$ @> :p;")
	f.Fuzz(func(t *testing.T, src string) {
		file, _ := NewScanner(postgres.Profile{}).ScanFile("fuzz.sql", []byte(src))
		for qi, q := range file.Queries {
			pos := q.HeaderSpan.End
			for ii, it := range q.Items {
				r := it.Raw()
				if r.Start != pos || r.End < r.Start || r.End > len(src) {
					t.Fatalf("query %d item %d: span %+v violates contiguity from %d (len %d)",
						qi, ii, r, pos, len(src))
				}
				pos = r.End
			}
		}
	})
}
