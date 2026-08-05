package codegen

import (
	"regexp"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/shape"
	"github.com/moznion/go-sqletch/internal/template"
	"github.com/moznion/go-sqletch/runtime"
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
// expanding path for every question-style dialect: each enumerable
// shape (guards × representative arities) renders and composes
// byte-identically, parses under the dialect frontend, and runtime
// arities beyond the representative one stay parseable.
func TestInList_QuestionConformance(t *testing.T) {
	for _, d := range []struct {
		name     string
		profile  dialect.LexerProfile
		frontend dialect.Frontend
		empty    string
	}{
		{"mysql", mysql.Profile{}, mysql.Frontend{}, "IN (SELECT NULL FROM DUAL WHERE FALSE)"},
		{"sqlite", sqlite.Profile{}, sqlite.Frontend{}, "IN (SELECT NULL WHERE 0)"},
	} {
		t.Run(d.name, func(t *testing.T) {
			inListConformance(t, d.profile, d.frontend, d.empty)
		})
	}
}

func inListConformance(t *testing.T, profile dialect.LexerProfile, fe dialect.Frontend, emptySQL string) {
	t.Helper()
	f, diags := template.NewScanner(profile).ScanFile("t.sql", []byte(inListTemplate))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	frags := BuildFrags(profile, q)

	keys, _ := shape.EnumerateExpand(q, 0, true)
	if len(keys) != 4 { // 2 guards × 2 representative arities
		t.Fatalf("shapes = %d: %v", len(keys), keys)
	}
	sawEmpty := false
	for _, k := range keys {
		want, err := ast.RenderShape(profile, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
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
		if strings.Contains(got, emptySQL) {
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
	if _, err := fe.Parse(sql); err != nil {
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
	rs, err := ast.Renderings(profile, q)
	if err != nil {
		t.Fatal(err)
	}
	empties := 0
	for _, r := range rs {
		if r.Kind == ast.RenderInEmpty {
			empties++
			if !strings.Contains(r.SQL, emptySQL) {
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

// TestGenerate_QuestionStyle pins the generated code of a MySQL query:
// database/sql driver surface, slice params for @in, arity key, and
// the binds-based composition path.
func TestGenerate_QuestionStyle(t *testing.T) {
	f, diags := template.NewScanner(mysql.Profile{}).ScanFile("t.sql", []byte(inListTemplate))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	q.Params["min_id"].Optional = true
	types := map[string]dialect.TypeRef{}
	tm := mysql.TypeMap{}
	for name, sqlType := range map[string]string{
		"tenant_id": "bigint", "statuses": "varchar(16)", "min_id": "bigint", "limit": "bigint",
	} {
		tr, ok := tm.TypeByName(sqlType)
		if !ok {
			t.Fatal(sqlType)
		}
		types[name] = tr
	}
	idRef, _ := tm.TypeByName("bigint")
	emailRef, _ := tm.TypeByName("varchar")
	files, ds := Generate(Options{Package: "gen", Style: runtime.StyleQuestion}, tm, []QueryInput{{
		Q:          q,
		Frags:      BuildFrags(mysql.Profile{}, q),
		ParamTypes: types,
		Columns: []dialect.ColumnDesc{
			{Name: "id", Type: idRef},
			{Name: "email", Type: emailRef},
		},
		Nullable: []bool{false, false},
	}})
	if len(ds) != 0 {
		t.Fatalf("generate: %+v", ds)
	}
	src := string(files["users_in_statuses.sql.gen.go"])
	for _, want := range []string{
		`Statuses\s+\[\]string`,
		`MinID\s+\*int64`,
		`key\.Arities = \[\]int32\{int32\(len\(arg\.Statuses\)\)\}`,
		`q\.cache\.GetBindsStyle\(runtime\.StyleQuestion, "UsersInStatuses", usersInStatusesFrags, key\)`,
		`runtime\.ResolveArgs\(binds, \[\]any\{arg\.TenantID, arg\.Statuses, arg\.MinID, arg\.Limit\}, nil\)`,
		`\{Kind: runtime\.InList, Text: "IN \(SELECT NULL FROM DUAL WHERE FALSE\)", ParamIdx: \[\]int16\{1\}\}`,
		`q\.db\.QueryContext\(ctx, sqlText, args\.\.\.\)`,
	} {
		if !regexp.MustCompile(want).MatchString(src) {
			t.Errorf("generated code missing pattern %q\n----\n%s", want, src)
		}
	}
	db := string(files["db.gen.go"])
	if !strings.Contains(db, `"database/sql"`) || strings.Contains(db, "pgx") {
		t.Errorf("db.go must be the database/sql flavor:\n%s", db)
	}
	if !strings.Contains(db, "WithTx(tx *sql.Tx)") {
		t.Errorf("db.go WithTx flavor:\n%s", db)
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
