package rules

import (
	"strings"
	"testing"

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/codegen"
	"github.com/moznion/sqletch/internal/dialect/postgres"
	"github.com/moznion/sqletch/internal/shape"
	"github.com/moznion/sqletch/runtime"
)

const inTemplate = `-- name: UsersByStatus :many
SELECT u.id FROM users AS u
WHERE u.tenant_id = :tenant_id
  AND u.status @in(:statuses)
@if-present(min_id)
  AND u.id >= :min_id
@endif
ORDER BY u.id;
`

func TestInExpr_RenderAndConformance(t *testing.T) {
	q := scanOne(t, inTemplate)
	if diags := CheckLexical(postgres.Profile{}, q); len(diags) != 0 {
		t.Fatalf("lexical: %+v", diags)
	}
	if diags := checkR1(t, inTemplate); len(diags) != 0 {
		t.Fatalf("R1: %+v", diags)
	}
	rs, err := ast.Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rs[0].SQL, "AND u.status = ANY($2)") {
		t.Fatalf("@in rendering:\n%s", rs[0].SQL)
	}
	// ParamsSeq includes the @in parameter at its emission position.
	if rs[0].ParamsSeq[1] != "statuses" {
		t.Fatalf("ParamsSeq = %v", rs[0].ParamsSeq)
	}

	// Compose conformance across all shapes (guard on/off).
	frags := codegen.BuildFrags(postgres.Profile{}, q)
	keys, _ := shape.Enumerate(q, 0)
	if len(keys) != 2 {
		t.Fatalf("shapes = %d", len(keys))
	}
	fe := postgres.Frontend{}
	for _, k := range keys {
		want, err := ast.RenderShape(postgres.Profile{}, q, k.Guards, k.Selection(), k.OrderSelection())
		if err != nil {
			t.Fatal(err)
		}
		got, argIdx := runtime.Compose(frags, runtime.ShapeKey{Guards: k.Guards, Choices: k.Choices, Orders: k.Orders})
		if got != want.SQL {
			t.Fatalf("shape %s:\nruntime %q\nrender  %q", k, got, want.SQL)
		}
		for i, idx := range argIdx {
			if q.ParamOrder[idx] != want.ParamsSeq[i] {
				t.Fatalf("shape %s bind %d: %q vs %q", k, i, q.ParamOrder[idx], want.ParamsSeq[i])
			}
		}
		if _, err := fe.Parse(got); err != nil {
			t.Fatalf("shape %s does not parse: %v\n%s", k, err, got)
		}
	}
}

func TestTypeByName(t *testing.T) {
	tm := postgres.TypeMap{}
	tests := map[string]uint32{
		"text":                     25,
		"varchar(16)":              1043,
		"BIGINT":                   20,
		"timestamp with time zone": 1184,
		"text[]":                   1009,
		"bigint[]":                 1016,
	}
	for in, oid := range tests {
		tr, ok := tm.TypeByName(in)
		if !ok || tr.OID != oid {
			t.Errorf("TypeByName(%q) = (%+v, %v), want OID %d", in, tr, ok, oid)
		}
	}
	if _, ok := tm.TypeByName("weirdtype"); ok {
		t.Error("unknown type must not resolve")
	}
}
