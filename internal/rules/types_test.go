package rules

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

const typesTemplate = `-- name: Q :many
SELECT u.id FROM users AS u
WHERE TRUE
@if-present(status)
  AND u.status = :status
@endif
@choose(sort)
@case(a)
ORDER BY u.id ASC
@case(b)
ORDER BY u.id DESC
@end
LIMIT :limit;
`

func TestCheckTypeAgreement(t *testing.T) {
	q := scanOne(t, typesTemplate)
	rs, err := ast.Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("renderings = %d, want 2", len(rs))
	}

	text := dialect.TypeRef{OID: 25, Name: "text"}
	int8t := dialect.TypeRef{OID: 20, Name: "int8"}
	col := dialect.ColumnDesc{Name: "id", Type: int8t}

	agree := []dialect.Desc{
		{Params: []dialect.TypeRef{text, int8t}, Columns: []dialect.ColumnDesc{col}},
		{Params: []dialect.TypeRef{text, int8t}, Columns: []dialect.ColumnDesc{col}},
	}
	if diags := CheckTypeAgreement(q, rs, agree); len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	types, diags := ResolveParamTypes(q, rs, agree)
	if len(diags) != 0 {
		t.Fatalf("resolve diagnostics: %+v", diags)
	}
	if len(types) != 2 || types[0].Name != "status" || types[0].Type.OID != 25 ||
		types[1].Name != "limit" || types[1].Type.OID != 20 {
		t.Fatalf("resolved types = %+v", types)
	}

	// Column disagreement between cases -> SQLETCH210.
	colMismatch := []dialect.Desc{
		{Params: []dialect.TypeRef{text, int8t}, Columns: []dialect.ColumnDesc{col}},
		{Params: []dialect.TypeRef{text, int8t}, Columns: []dialect.ColumnDesc{{Name: "id", Type: text}}},
	}
	if diags := CheckTypeAgreement(q, rs, colMismatch); !hasCode(diags, diagnostics.CodeColumnAgreement) {
		t.Errorf("want SQLETCH210, got %+v", diags)
	}

	// Param type disagreement across renderings -> SQLETCH211.
	paramMismatch := []dialect.Desc{
		{Params: []dialect.TypeRef{text, int8t}, Columns: []dialect.ColumnDesc{col}},
		{Params: []dialect.TypeRef{text, text}, Columns: []dialect.ColumnDesc{col}},
	}
	if diags := CheckTypeAgreement(q, rs, paramMismatch); !hasCode(diags, diagnostics.CodeParamAgreement) {
		t.Errorf("want SQLETCH211, got %+v", diags)
	}

	// Column count disagreement -> SQLETCH210.
	countMismatch := []dialect.Desc{
		{Params: []dialect.TypeRef{text, int8t}, Columns: []dialect.ColumnDesc{col}},
		{Params: []dialect.TypeRef{text, int8t}, Columns: []dialect.ColumnDesc{col, col}},
	}
	if diags := CheckTypeAgreement(q, rs, countMismatch); !hasCode(diags, diagnostics.CodeColumnAgreement) {
		t.Errorf("want SQLETCH210 for count, got %+v", diags)
	}
}
