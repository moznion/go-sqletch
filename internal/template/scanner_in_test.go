package template

import (
	"strings"
	"testing"

	"github.com/moznion/sqletch/internal/diagnostics"
)

const inTemplate = `-- name: UsersByStatus :many
-- @param statuses: text[]
SELECT u.id FROM users AS u
WHERE u.tenant_id = :tenant_id
  AND u.status @in(:statuses)
ORDER BY u.id;
`

func TestScan_InExpr(t *testing.T) {
	f := scanClean(t, inTemplate)
	q := f.Queries[0]
	var in *InExpr
	for _, it := range q.Items {
		if v, ok := it.(*InExpr); ok {
			in = v
		}
	}
	if in == nil || in.Param != "statuses" {
		t.Fatalf("in = %+v", in)
	}
	// The construct splits the skeleton mid-expression; the parameter
	// is recorded like any skeleton bind.
	if p := q.Params["statuses"]; p == nil || len(p.Occurrences) != 1 || p.Occurrences[0].Guards != nil {
		t.Fatalf("statuses param = %+v", q.Params["statuses"])
	}
	// The type hint directive was captured (and stays in the skeleton).
	hint, ok := q.TypeHints["statuses"]
	if !ok || hint.SQLType != "text[]" {
		t.Fatalf("type hints = %+v", q.TypeHints)
	}
	var skel strings.Builder
	for _, it := range q.Items {
		if s, ok := it.(*Skeleton); ok {
			skel.WriteString(s.Text)
		}
	}
	if !strings.Contains(skel.String(), "-- @param statuses: text[]") {
		t.Error("directive comment must remain skeleton text")
	}
}

func TestScan_InExpr_Rejected(t *testing.T) {
	tests := []struct {
		name, src string
		code      diagnostics.Code
	}{
		{
			name: "inside guarded body unsupported",
			code: diagnostics.CodeConstructBadSlot,
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE TRUE
@if-present(ids)
  AND t.id @in(:ids)
@endif
;
`,
		},
		{
			name: "bad argument",
			code: diagnostics.CodeConstructGrammar,
			src:  "-- name: Bad :many\nSELECT 1 FROM t WHERE t.id @in(ids);\n",
		},
		{
			name: "projection position",
			code: diagnostics.CodeConstructBadSlot,
			src:  "-- name: Bad :many\nSELECT t.id @in(:ids) FROM t;\n",
		},
		{
			name: "inside parens",
			code: diagnostics.CodeConstructNested,
			src:  "-- name: Bad :many\nSELECT 1 FROM t WHERE (t.id @in(:ids));\n",
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

func TestScan_ParamHint_OnlyInsideQueries(t *testing.T) {
	src := "-- @param outside: text\n" + inTemplate
	f, diags := scan(t, src)
	if len(diags) != 0 {
		t.Fatalf("diags: %+v", diags)
	}
	if _, ok := f.Queries[0].TypeHints["outside"]; ok {
		t.Error("directives before any header must be ignored")
	}
}
