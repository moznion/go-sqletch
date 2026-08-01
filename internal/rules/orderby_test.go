package rules

import (
	"strings"
	"testing"

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect/postgres"
	"github.com/moznion/sqletch/internal/shape"
	"github.com/moznion/sqletch/internal/template"
)

const orderByTemplate = `-- name: ListUsers :many
SELECT u.id, u.email, u.created_at FROM users AS u
WHERE TRUE
@if-present(status)
  AND u.status = :status
@endif
@order-by(sort)
@key(created_at)
u.created_at
@key(email)
u.email
@default
ORDER BY u.id ASC
@end
LIMIT :limit;
`

func TestOrderBy_ScanAndShapes(t *testing.T) {
	q := scanOne(t, orderByTemplate)
	var o *template.OrderBy
	for _, it := range q.Items {
		if v, ok := it.(*template.OrderBy); ok {
			o = v
		}
	}
	if o == nil || o.Param != "sort" || len(o.Keys) != 2 || o.Default == nil {
		t.Fatalf("order-by = %+v", o)
	}
	if o.Keys[0].Name != "created_at" || o.Keys[0].Body != "u.created_at" {
		t.Errorf("key[0] = %+v", o.Keys[0])
	}
	if diags := CheckLexical(postgres.Profile{}, q); len(diags) != 0 {
		t.Fatalf("lexical: %+v", diags)
	}
	if diags := checkR1(t, orderByTemplate); len(diags) != 0 {
		t.Fatalf("R1: %+v", diags)
	}

	// Renderings: maximal + the @default body (verified like a case).
	rs, err := ast.Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[1].Kind != ast.RenderOrderDefault {
		t.Fatalf("renderings = %d (kinds %v %v), want maximal + order-default", len(rs), rs[0].Kind, rs[1].Kind)
	}
	if !strings.Contains(rs[0].SQL, "ORDER BY u.created_at, u.email") {
		t.Errorf("maximal must list all keys in declaration order:\n%s", rs[0].SQL)
	}
	if !strings.Contains(rs[1].SQL, "ORDER BY u.id ASC") || strings.Contains(rs[1].SQL, "u.created_at,") {
		t.Errorf("default rendering must substitute the default body:\n%s", rs[1].SQL)
	}

	// Shape space: 2 guards? no — 1 guard x orderCount(2)=13 → 26.
	if got := shape.Count(q).Int64(); got != 26 {
		t.Fatalf("Count = %d, want 26 (2 guard states x 13 order selections)", got)
	}
	keys, truncated := shape.Enumerate(q, 0)
	if truncated || len(keys) != 26 {
		t.Fatalf("Enumerate = %d (truncated=%v), want 26", len(keys), truncated)
	}
	// Every enumerated shape parses (incl. permutations and DESC mixes).
	fe := postgres.Frontend{}
	seen := map[string]bool{}
	for _, k := range keys {
		s := k.String()
		if seen[s] {
			t.Fatalf("duplicate key %s", s)
		}
		seen[s] = true
		r, err := ast.RenderShape(postgres.Profile{}, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fe.Parse(r.SQL); err != nil {
			t.Fatalf("shape %s does not parse: %v\n%s", k, err, r.SQL)
		}
	}
}

func TestOrderBy_PermutationAndDirectionEmission(t *testing.T) {
	q := scanOne(t, orderByTemplate)
	// email DESC, created_at ASC — reversed declaration order.
	r, err := ast.RenderShape(postgres.Profile{}, q, 0, nil,
		ast.OrderSelection{{1<<1 | 1, 0 << 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.SQL, "ORDER BY u.email DESC, u.created_at") {
		t.Errorf("permutation emission:\n%s", r.SQL)
	}
	// Empty selection → default body.
	r, err = ast.RenderShape(postgres.Profile{}, q, 0, nil, ast.OrderSelection{{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.SQL, "ORDER BY u.id ASC") {
		t.Errorf("empty selection must emit the default:\n%s", r.SQL)
	}
}

func TestOrderBy_Violations(t *testing.T) {
	distinctOn := `-- name: Bad :many
SELECT DISTINCT ON (u.email) u.email, u.id FROM users AS u
WHERE TRUE
@order-by(sort)
@key(email)
u.email
@end
;
`
	if diags := checkR1(t, distinctOn); !hasCode(diags, diagnostics.CodeOrderByDistinct) {
		t.Fatalf("want SQLETCH122, got %+v", diags)
	}

	withTies := `-- name: Bad :many
SELECT u.id FROM users AS u
WHERE TRUE
@order-by(sort)
@key(id)
u.id
@end
FETCH FIRST 10 ROWS WITH TIES;
`
	if diags := checkR1(t, withTies); !hasCode(diags, diagnostics.CodeOrderByNeedsDflt) {
		t.Fatalf("want SQLETCH123, got %+v", diags)
	}

	// With a @default, WITH TIES is fine.
	withDefault := strings.Replace(withTies, "@end", "@default\nORDER BY u.id\n@end", 1)
	if diags := checkR1(t, withDefault); hasCode(diags, diagnostics.CodeOrderByNeedsDflt) {
		t.Fatalf("@default must satisfy WITH TIES: %+v", diags)
	}

	keySmuggle := `-- name: Bad :many
SELECT u.id FROM users AS u
WHERE TRUE
@order-by(sort)
@key(id)
u.id LIMIT 5
@end
;
`
	if diags := checkR1(t, keySmuggle); !hasCode(diags, diagnostics.CodeNodeIncomplete) {
		t.Fatalf("want SQLETCH102 for key smuggling, got %+v", diags)
	}
}

func TestOrderBy_ControlParamMustNotBind(t *testing.T) {
	src := `-- name: Bad :many
SELECT u.id FROM users AS u
WHERE u.x = :sort
@order-by(sort)
@key(id)
u.id
@end
;
`
	q := scanOne(t, src)
	if diags := CheckLexical(postgres.Profile{}, q); !hasCode(diags, diagnostics.CodeChooseParamBinds) {
		t.Fatalf("want SQLETCH112 for bound @order-by param, got %+v", diags)
	}
}
