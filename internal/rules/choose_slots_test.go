package rules

import (
	"testing"

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/cache"
	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect"
	"github.com/moznion/sqletch/internal/dialect/postgres"
	"github.com/moznion/sqletch/internal/nullability"
	"github.com/moznion/sqletch/internal/shape"
	"github.com/moznion/sqletch/internal/template"
)

// PROJECT_INSTRUCTION Use Case 4: @choose in a projection slot; the
// alias stays in the skeleton (R8).
const bucketTemplate = `-- name: SignupsByBucket :many
SELECT
@choose(bucket)
@case(daily)
date_trunc('day', u.created_at)
@case(weekly)
date_trunc('week', u.created_at)
@case(monthly)
date_trunc('month', u.created_at)
@end
 AS bucket,
    count(*) AS signups
FROM users AS u
WHERE u.created_at >= :since
GROUP BY 1
ORDER BY 1;
`

func TestChooseProjection_CleanAndShapesParse(t *testing.T) {
	q := scanOne(t, bucketTemplate)
	var c *template.Choose
	for _, it := range q.Items {
		if ch, ok := it.(*template.Choose); ok {
			c = ch
		}
	}
	if c == nil || c.Slot != template.SlotProjExpr || len(c.Cases) != 3 || c.Default != nil {
		t.Fatalf("choose = %+v", c)
	}
	if diags := checkR1(t, bucketTemplate); len(diags) != 0 {
		t.Fatalf("R1 diagnostics: %+v", diags)
	}
	keys, _ := shape.Enumerate(q, 0)
	if len(keys) != 3 {
		t.Fatalf("shapes = %d, want 3", len(keys))
	}
	fe := postgres.Frontend{}
	for _, k := range keys {
		r, err := ast.RenderShape(postgres.Profile{}, q, k.Guards, k.Selection(), k.OrderSelection())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fe.Parse(r.SQL); err != nil {
			t.Fatalf("shape %s does not parse: %v\n%s", k, err, r.SQL)
		}
	}
}

func TestChooseGroupBy_Clean(t *testing.T) {
	src := `-- name: Q :many
SELECT count(*) AS n FROM t
WHERE TRUE
@choose(grouping)
@case(by_a)
GROUP BY t.a
@case(by_b)
GROUP BY t.b
@default
@end
;
`
	q := scanOne(t, src)
	var c *template.Choose
	for _, it := range q.Items {
		if ch, ok := it.(*template.Choose); ok {
			c = ch
		}
	}
	if c == nil || c.Slot != template.SlotGroupBy {
		t.Fatalf("choose slot = %+v, want SlotGroupBy", c)
	}
	if diags := checkR1(t, src); len(diags) != 0 {
		t.Fatalf("R1 diagnostics: %+v", diags)
	}
}

// Scanner-level slot violations (detected during classification).
func TestChoose_ScanTimeSlotViolations(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{
			name: "empty case in projection slot (R2)",
			src: `-- name: Bad :many
SELECT
@choose(x)
@case(a)
t.a
@default
@end
 AS c FROM t;
`,
		},
		{
			name: "mixed ORDER BY and GROUP BY cases",
			src: `-- name: Bad :many
SELECT 1 FROM t WHERE TRUE
@choose(x)
@case(a)
ORDER BY t.a
@case(b)
GROUP BY t.b
@end
;
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, diags := template.NewScanner(postgres.Profile{}).ScanFile("test.sql", []byte(tt.src))
			_ = f
			if !hasCode(diags, diagnostics.CodeChooseStructure) {
				t.Errorf("want SQLETCH009 from the scanner, got %+v", diags)
			}
		})
	}
}

func TestChoose_SlotViolations(t *testing.T) {
	tests := []struct {
		name string
		src  string
		code diagnostics.Code
	}{
		{
			name: "group-by case smuggling a LIMIT",
			code: diagnostics.CodeNodeIncomplete,
			src: `-- name: Bad :many
SELECT count(*) AS n FROM t WHERE TRUE
@choose(g)
@case(a)
GROUP BY t.a LIMIT 5
@end
;
`,
		},
		{
			name: "projection case smuggling an alias",
			code: diagnostics.CodeNodeIncomplete,
			src: `-- name: Bad :many
SELECT
@choose(x)
@case(a)
t.a AS sneaky
@end
 AS c FROM t;
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := scanOne(t, tt.src)
			rs, err := ast.Renderings(postgres.Profile{}, q)
			if err != nil {
				t.Fatal(err)
			}
			diags := CheckR1(postgres.Profile{}, postgres.Frontend{}, q, rs)
			if !hasCode(diags, tt.code) {
				t.Errorf("want %s, got %+v", tt.code, diags)
			}
		})
	}
}

// Review counterexample F1c: two projection cases with the same type
// but different nullability — the union must be nullable.
func TestNullabilityUnionAcrossCases(t *testing.T) {
	src := `-- name: F1c :many
SELECT
@choose(fmt)
@case(raw)
u.email
@case(blank)
nullif(u.email, '')
@end
 AS email
FROM users AS u;
`
	q := scanOne(t, src)
	rs, err := ast.Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("renderings = %d", len(rs))
	}
	cat := fixtureCatalog() // users.email NOT NULL
	// The raw case describes with a source-column ref (non-null); the
	// nullif case is an expression (nullable).
	descs := []dialect.Desc{
		{Columns: []dialect.ColumnDesc{{Name: "email", SrcRel: 1001, SrcAtt: 2}}},
		{Columns: []dialect.ColumnDesc{{Name: "email"}}},
	}
	got, err := nullability.AnalyzeAll(postgres.Frontend{}, rs, descs, cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != true {
		t.Fatalf("union nullability = %v, want [true] (nullable-most case wins)", got)
	}
}

// Guard against regression: with only the raw case, the column stays
// non-nullable — the union must not blindly force pointers.
func TestNullabilityUnion_SingleCaseStaysNarrow(t *testing.T) {
	src := `-- name: N :many
SELECT u.email AS email FROM users AS u;
`
	q := scanOne(t, src)
	rs, err := ast.Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	descs := []dialect.Desc{{Columns: []dialect.ColumnDesc{{Name: "email", SrcRel: 1001, SrcAtt: 2}}}}
	got, err := nullability.AnalyzeAll(postgres.Frontend{}, rs, descs, fixtureCatalog(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != false {
		t.Fatalf("nullability = %v, want [false]", got)
	}
}

var _ = cache.Catalog{} // keep the import stable across edits
