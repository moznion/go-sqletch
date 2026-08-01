package codegen

import (
	"strings"
	"testing"

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/dialect/mysql"
	"github.com/moznion/sqletch/internal/shape"
	"github.com/moznion/sqletch/internal/template"
	"github.com/moznion/sqletch/runtime"
)

const inListTemplate = `-- name: UsersInStatuses :many
-- @param statuses: varchar(16)
SELECT u.id, u.email FROM users AS u
WHERE u.tenant_id = :tenant_id
  AND u.status @in(:statuses)
@if-present(min_id)
  AND u.id >= :min_id
@endif
ORDER BY u.id
LIMIT :limit;
`

// TestInList_QuestionConformance pins the @in arity machinery on the
// expanding path: every enumerable shape (guards × representative
// arities) renders and composes byte-identically, parses under the
// MySQL frontend, and runtime arities beyond the representative one
// stay parseable.
func TestInList_QuestionConformance(t *testing.T) {
	f, diags := template.NewScanner(mysql.Profile{}).ScanFile("t.sql", []byte(inListTemplate))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	frags := BuildFrags(mysql.Profile{}, q)

	keys, _ := shape.EnumerateExpand(q, 0, true)
	if len(keys) != 4 { // 2 guards × 2 representative arities
		t.Fatalf("shapes = %d: %v", len(keys), keys)
	}
	fe := mysql.Frontend{}
	sawEmpty := false
	for _, k := range keys {
		want, err := ast.RenderShape(mysql.Profile{}, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
		if err != nil {
			t.Fatal(err)
		}
		got, binds, err := runtime.ComposeTreeStyle(runtime.StyleQuestion, frags,
			runtime.ShapeKey{Guards: k.Guards, Choices: k.Choices, Orders: k.Orders, Arities: k.Arities()},
			nil, runtime.DefaultTreeCaps)
		if err != nil {
			t.Fatal(err)
		}
		if got != want.SQL {
			t.Fatalf("shape %s:\nruntime %q\nrender  %q", k, got, want.SQL)
		}
		if len(binds) != len(want.ParamsSeq) {
			t.Fatalf("shape %s: binds %d, ParamsSeq %d", k, len(binds), len(want.ParamsSeq))
		}
		for i, bd := range binds {
			if q.ParamOrder[bd.Idx] != want.ParamsSeq[i] {
				t.Fatalf("shape %s bind %d: %q vs %q", k, i, q.ParamOrder[bd.Idx], want.ParamsSeq[i])
			}
		}
		if _, err := fe.Parse(got); err != nil {
			t.Fatalf("shape %s does not parse: %v\n%s", k, err, got)
		}
		if strings.Contains(got, "SELECT NULL FROM DUAL WHERE FALSE") {
			sawEmpty = true
		}
	}
	if !sawEmpty {
		t.Error("no arity-0 shape enumerated")
	}

	// Runtime arities beyond the representative: still parseable, one
	// bind per element.
	sql, binds, err := runtime.ComposeTreeStyle(runtime.StyleQuestion, frags,
		runtime.ShapeKey{Guards: 1, Arities: []int32{3}}, nil, runtime.DefaultTreeCaps)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "u.status IN (?, ?, ?)") {
		t.Fatalf("arity-3:\n%s", sql)
	}
	if _, err := (mysql.Frontend{}).Parse(sql); err != nil {
		t.Fatalf("arity-3 does not parse: %v\n%s", err, sql)
	}
	elems := 0
	for _, bd := range binds {
		if bd.Elem > 0 {
			elems++
		}
	}
	if elems != 3 {
		t.Errorf("binds = %+v, want 3 element binds", binds)
	}

	// The renderings set includes the arity-0 verification rendering.
	rs, err := ast.Renderings(mysql.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	empties := 0
	for _, r := range rs {
		if r.Kind == ast.RenderInEmpty {
			empties++
			if !strings.Contains(r.SQL, "IN (SELECT NULL FROM DUAL WHERE FALSE)") {
				t.Errorf("arity-0 rendering:\n%s", r.SQL)
			}
			for _, name := range r.ParamsSeq {
				if name == "statuses" {
					t.Error("arity-0 rendering must not bind the slice")
				}
			}
		}
	}
	if empties != 1 {
		t.Errorf("RenderInEmpty renderings = %d, want 1", empties)
	}
}

// TestShapeCountExpand pins the arity dimension in the count formula.
func TestShapeCountExpand(t *testing.T) {
	f, diags := template.NewScanner(mysql.Profile{}).ScanFile("t.sql", []byte(inListTemplate))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	if got := shape.Count(q).Int64(); got != 2 {
		t.Errorf("Count = %d, want 2 (PostgreSQL view)", got)
	}
	if got := shape.CountExpand(q, true).Int64(); got != 4 {
		t.Errorf("CountExpand = %d, want 4", got)
	}
}
