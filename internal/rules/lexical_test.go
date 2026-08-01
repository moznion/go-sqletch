package rules

import (
	"testing"

	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect/postgres"
	"github.com/moznion/sqletch/internal/template"
)

func scanOne(t *testing.T, src string) *template.QueryTemplate {
	t.Helper()
	f, diags := template.NewScanner(postgres.Profile{}).ScanFile("test.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scanner diagnostics (test precondition): %+v", diags)
	}
	if len(f.Queries) != 1 {
		t.Fatalf("queries = %d", len(f.Queries))
	}
	return f.Queries[0]
}

func TestCheckLexical_Anchors(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want bool // expect SQLETCH113
	}{
		{
			name: "all conjuncts optional without anchor",
			want: true,
			src: `-- name: Q :many
SELECT 1 FROM t
WHERE
@if-present(x)
  AND t.x = :x
@endif
;
`,
		},
		{
			name: "WHERE TRUE anchor",
			want: false,
			src: `-- name: Q :many
SELECT 1 FROM t
WHERE TRUE
@if-present(x)
  AND t.x = :x
@endif
;
`,
		},
		{
			name: "unconditional conjunct as anchor",
			want: false,
			src: `-- name: Q :many
SELECT 1 FROM t
WHERE t.tenant_id = :tenant_id
@if-present(x)
  AND t.x = :x
@endif
;
`,
		},
		{
			name: "anchor with comment between WHERE and fragment",
			want: true,
			src: `-- name: Q :many
SELECT 1 FROM t
WHERE -- anchor missing
@if-present(x)
  AND t.x = :x
@endif
;
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := scanOne(t, tt.src)
			diags := CheckLexical(postgres.Profile{}, q)
			if got := hasCode(diags, diagnostics.CodeUnanchoredClause); got != tt.want {
				t.Errorf("SQLETCH113 = %v, want %v (diags: %+v)", got, tt.want, diags)
			}
		})
	}
}

func TestCheckLexical_ParamDiscipline(t *testing.T) {
	tests := []struct {
		name string
		src  string
		code diagnostics.Code
	}{
		{
			name: "vacuous guard: guard param binds unguarded",
			code: diagnostics.CodeVacuousGuard,
			src: `-- name: Q :many
SELECT 1 FROM t
WHERE t.tenant_id = :tenant_id
@if-present(tenant_id)
  AND t.plan = :tenant_id
@endif
;
`,
		},
		{
			name: "guard never binds under itself",
			code: diagnostics.CodeGuardNeverBinds,
			src: `-- name: Q :many
SELECT 1 FROM t
WHERE TRUE
@if-present(flag)
  AND t.x = :other
@endif
;
`,
		},
		{
			name: "choose param used as bind param",
			code: diagnostics.CodeChooseParamBinds,
			src: `-- name: Q :many
SELECT 1 FROM t
WHERE t.a = :sort
@choose(sort)
@case(a)
ORDER BY t.a
@end
;
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := scanOne(t, tt.src)
			diags := CheckLexical(postgres.Profile{}, q)
			if !hasCode(diags, tt.code) {
				t.Errorf("want %s, got %+v", tt.code, diags)
			}
		})
	}
}

func TestCheckLexical_OptionalClassification(t *testing.T) {
	src := `-- name: Q :many
SELECT 1 FROM t
WHERE t.tenant_id = :tenant_id
@if-present(status)
  AND t.status = :status
@endif
@if-present(min_a, max_a)
  AND t.a BETWEEN :min_a AND :max_a
@endif
;
`
	q := scanOne(t, src)
	if diags := CheckLexical(postgres.Profile{}, q); len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	want := map[string]bool{
		"tenant_id": false,
		"status":    true,
		"min_a":     true,
		"max_a":     true,
	}
	for name, optional := range want {
		p := q.Params[name]
		if p == nil {
			t.Fatalf("param %q missing", name)
		}
		if p.Optional != optional {
			t.Errorf("param %q Optional = %v, want %v", name, p.Optional, optional)
		}
	}
}

// A param bound only inside a fragment guarded by OTHER params is
// required but legal (R9 third bullet).
func TestCheckLexical_OtherGuardedParamIsRequired(t *testing.T) {
	src := `-- name: Q :many
SELECT 1 FROM t
WHERE TRUE
@if-present(a)
  AND t.a = :a AND t.b = :b
@endif
;
`
	q := scanOne(t, src)
	if diags := CheckLexical(postgres.Profile{}, q); len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	if q.Params["a"].Optional != true {
		t.Error("a must be optional")
	}
	if q.Params["b"].Optional != false {
		t.Error("b must be required (guarded by another param)")
	}
}
