package rules

import (
	"testing"

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect"
	"github.com/moznion/sqletch/internal/dialect/postgres"
	"github.com/moznion/sqletch/internal/shape"
)

const whenTemplate = `-- name: ListItems :many
SELECT u.id FROM users AS u
WHERE TRUE
@when(include_deleted = false)
  AND u.status != 'deleted'
@end
@when(min_id != 0)
  AND u.id >= :min_id
@end
;
`

func TestWhen_LexicalAndShapes(t *testing.T) {
	q := scanOne(t, whenTemplate)
	if diags := CheckLexical(postgres.Profile{}, q); len(diags) != 0 {
		t.Fatalf("lexical: %+v", diags)
	}
	// Control params are required, never optional.
	if q.Params["include_deleted"].Optional || q.Params["min_id"].Optional {
		t.Error("@when params must be required")
	}
	if diags := checkR1(t, whenTemplate); len(diags) != 0 {
		t.Fatalf("R1: %+v", diags)
	}
	keys, _ := shape.Enumerate(q, 0)
	if len(keys) != 4 {
		t.Fatalf("shapes = %d, want 4 (2 value atoms)", len(keys))
	}
	fe := postgres.Frontend{}
	for _, k := range keys {
		r, err := ast.RenderShape(postgres.Profile{}, q, k.Guards, k.Selection())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fe.Parse(r.SQL); err != nil {
			t.Fatalf("shape %s does not parse: %v\n%s", k, err, r.SQL)
		}
	}
}

func TestWhen_MixedPresenceValueConflict(t *testing.T) {
	src := `-- name: Bad :many
SELECT 1 FROM t
WHERE TRUE
@if-present(mode)
  AND t.mode = :mode
@endif
@when(mode = 'x')
  AND t.extra
@end
;
`
	q := scanOne(t, src)
	diags := CheckLexical(postgres.Profile{}, q)
	if !hasCode(diags, diagnostics.CodeVacuousGuard) {
		t.Fatalf("want SQLETCH110 for mixed presence/value guard, got %+v", diags)
	}
}

// A @when param that also binds in SQL must agree with the literal's
// type (SQLETCH211 on mismatch).
func TestWhen_LiteralTypeAgreement(t *testing.T) {
	q := scanOne(t, whenTemplate)
	rs, err := ast.Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	int8t := dialect.TypeRef{OID: 20, Name: "int8"}
	textT := dialect.TypeRef{OID: 25, Name: "text"}
	col := dialect.ColumnDesc{Name: "id", Type: int8t}

	ok := []dialect.Desc{{Params: []dialect.TypeRef{int8t}, Columns: []dialect.ColumnDesc{col}}}
	if diags := CheckTypeAgreement(q, rs, ok); len(diags) != 0 {
		t.Fatalf("compatible literal flagged: %+v", diags)
	}
	bad := []dialect.Desc{{Params: []dialect.TypeRef{textT}, Columns: []dialect.ColumnDesc{col}}}
	if diags := CheckTypeAgreement(q, rs, bad); !hasCode(diags, diagnostics.CodeParamAgreement) {
		t.Fatalf("want SQLETCH211 for int literal vs text param, got %+v", diags)
	}
}

func TestHaving_R1AndAnchor(t *testing.T) {
	clean := `-- name: H :many
SELECT t.user_id, sum(t.amount) AS total FROM t
GROUP BY t.user_id
HAVING TRUE
@if-present(min_total)
  AND sum(t.amount) >= :min_total
@endif
;
`
	if diags := checkR1(t, clean); len(diags) != 0 {
		t.Fatalf("R1: %+v", diags)
	}
	q := scanOne(t, clean)
	if diags := CheckLexical(postgres.Profile{}, q); len(diags) != 0 {
		t.Fatalf("lexical: %+v", diags)
	}

	unanchored := `-- name: H :many
SELECT t.user_id, sum(t.amount) AS total FROM t
GROUP BY t.user_id
HAVING
@if-present(min_total)
  AND sum(t.amount) >= :min_total
@endif
;
`
	q2 := scanOne(t, unanchored)
	if diags := CheckLexical(postgres.Profile{}, q2); !hasCode(diags, diagnostics.CodeUnanchoredClause) {
		t.Fatalf("want SQLETCH113 for unanchored HAVING, got %+v", diags)
	}

	smuggle := `-- name: H :many
SELECT t.user_id FROM t
GROUP BY t.user_id
HAVING TRUE
@if-present(x)
  AND count(*) > :x; DROP TABLE t
@endif
;
`
	if diags := checkR1(t, smuggle); !hasCode(diags, diagnostics.CodeNodeIncomplete) {
		t.Fatalf("want SQLETCH102 for HAVING smuggling, got %+v", diags)
	}
}

// All shapes of the HAVING template parse, including the minimal one.
func TestHavingShapesParse(t *testing.T) {
	src := `-- name: H :many
SELECT t.user_id, sum(t.amount) AS total FROM t
GROUP BY t.user_id
HAVING TRUE
@if-present(min_total)
  AND sum(t.amount) >= :min_total
@endif
;
`
	q := scanOne(t, src)
	keys, _ := shape.Enumerate(q, 0)
	fe := postgres.Frontend{}
	for _, k := range keys {
		r, err := ast.RenderShape(postgres.Profile{}, q, k.Guards, k.Selection())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fe.Parse(r.SQL); err != nil {
			t.Fatalf("shape %s: %v\n%s", k, err, r.SQL)
		}
	}
}
