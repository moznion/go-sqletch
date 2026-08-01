package template

import (
	"strings"
	"testing"

	"github.com/moznion/sqletch/internal/diagnostics"
)

const whenTemplate = `-- name: ListItems :many
SELECT t.id FROM t
WHERE TRUE
@when(include_deleted = false)
  AND t.deleted_at IS NULL
@end
@when(mode = 'archived')
  AND t.archived
@end
@when(min_rank != 0)
  AND t.rank >= :min_rank
@end
;
`

func TestScan_When_Atoms(t *testing.T) {
	f := scanClean(t, whenTemplate)
	q := f.Queries[0]
	var frags []*IfPresent
	for _, it := range q.Items {
		if ip, ok := it.(*IfPresent); ok {
			frags = append(frags, ip)
		}
	}
	if len(frags) != 3 {
		t.Fatalf("fragments = %d, want 3", len(frags))
	}
	want := []GuardAtom{
		{Param: "include_deleted", Op: "=", Value: "false", Kind: ValueBool, RawValue: "false"},
		{Param: "mode", Op: "=", Value: "archived", Kind: ValueString, RawValue: "'archived'"},
		{Param: "min_rank", Op: "!=", Value: "0", Kind: ValueInt, RawValue: "0"},
	}
	for i, w := range want {
		if len(frags[i].Guards) != 1 || frags[i].Guards[0] != w {
			t.Errorf("frag %d atom = %+v, want %+v", i, frags[i].Guards, w)
		}
		if frags[i].Slot != SlotWhereConjunct || frags[i].Sep != SepAnd {
			t.Errorf("frag %d slot/sep = %v/%v", i, frags[i].Slot, frags[i].Sep)
		}
	}
	// Guard bits assigned per atom; params registered even when unbound.
	if len(q.GuardAtoms) != 3 {
		t.Fatalf("guard atoms = %+v", q.GuardAtoms)
	}
	if q.Params["include_deleted"] == nil || q.Params["mode"] == nil {
		t.Fatal("control params must be registered")
	}
	// min_rank also binds in SQL.
	if len(q.Params["min_rank"].Occurrences) != 1 {
		t.Fatalf("min_rank occurrences = %+v", q.Params["min_rank"].Occurrences)
	}
}

func TestScan_When_StringEscapes(t *testing.T) {
	src := `-- name: Q :many
SELECT 1 FROM t WHERE TRUE
@when(kind = 'it''s')
  AND t.kind IS NOT NULL
@end
;
`
	f := scanClean(t, src)
	g := f.Queries[0].GuardAtoms[0]
	if g.Value != "it's" || g.RawValue != "'it''s'" {
		t.Errorf("atom = %+v", g)
	}
}

func TestScan_When_Rejected(t *testing.T) {
	tests := []struct {
		name, src string
		code      diagnostics.Code
	}{
		{
			name: "bad operator",
			code: diagnostics.CodeConstructGrammar,
			src:  "-- name: Q :many\nSELECT 1 FROM t WHERE TRUE\n@when(a > 5)\n  AND t.a\n@end\n;\n",
		},
		{
			name: "float literal",
			code: diagnostics.CodeConstructGrammar,
			src:  "-- name: Q :many\nSELECT 1 FROM t WHERE TRUE\n@when(a = 1.5)\n  AND t.a\n@end\n;\n",
		},
		{
			name: "unterminated",
			code: diagnostics.CodeConstructGrammar,
			src:  "-- name: Q :many\nSELECT 1 FROM t WHERE TRUE\n@when(a = 1)\n  AND t.a = :b\n;\n",
		},
		{
			name: "nested when",
			code: diagnostics.CodeConstructNesting,
			src:  "-- name: Q :many\nSELECT 1 FROM t WHERE TRUE\n@when(a = 1)\n@when(b = 2)\n  AND t.a\n@end\n@end\n;\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, diags := scan(t, tt.src)
			if !hasCode(diags, tt.code) {
				t.Errorf("want %s, got %+v", tt.code, diags)
			}
		})
	}
}

const havingTemplate = `-- name: BigSpenders :many
SELECT t.user_id, sum(t.amount) AS total
FROM t
WHERE TRUE
GROUP BY t.user_id
HAVING TRUE
@if-present(min_total)
  AND sum(t.amount) >= :min_total
@endif
;
`

func TestScan_HavingSlot(t *testing.T) {
	f := scanClean(t, havingTemplate)
	q := f.Queries[0]
	var frags []*IfPresent
	for _, it := range q.Items {
		if ip, ok := it.(*IfPresent); ok {
			frags = append(frags, ip)
		}
	}
	if len(frags) != 1 || frags[0].Slot != SlotHavingConjunct || frags[0].Sep != SepAnd {
		t.Fatalf("fragment = %+v", frags)
	}
	if !strings.HasPrefix(frags[0].Body, "sum(t.amount)") {
		t.Errorf("body = %q", frags[0].Body)
	}
}
