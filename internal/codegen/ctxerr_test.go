package codegen

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/template"
	"github.com/moznion/go-sqletch/runtime"
)

// genCtxErr generates one single-column question-style query with the
// SQLite context-error normalization on (or off).
func genCtxErr(t *testing.T, norm bool, src string) string {
	t.Helper()
	f, diags := template.NewScanner(mysql.Profile{}).ScanFile("t.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	in := QueryInput{Q: q, Frags: BuildFrags(mysql.Profile{}, q),
		ParamTypes: map[string]dialect.TypeRef{"id": {OID: 8}}}
	switch q.Annotation {
	case template.AnnotationExec, template.AnnotationExecRows:
	default:
		in.Columns = []dialect.ColumnDesc{{Name: "id", Type: dialect.TypeRef{OID: 8}}}
		in.Nullable = []bool{false}
	}
	files, diags := Generate(Options{Package: "gen", Style: runtime.StyleQuestion, NormalizeCtxErr: norm},
		mysql.TypeMap{}, []QueryInput{in})
	if len(diags) != 0 {
		t.Fatalf("generate: %+v", diags)
	}
	return string(files["q.sql.gen.go"])
}

// SQLite (doc 23 §5, owner decision 2026-10-02): every driver error a
// generated method returns — and hands the observer — goes through
// runtime.CtxErr, whether or not the query has a @timeout (a caller's
// deadline hits the same SQLITE_INTERRUPT). Each case lists the exact
// normalization sites its annotation must carry; the count pins that
// there are no others.
func TestGenerate_SQLiteCtxErrFunnel(t *testing.T) {
	cases := map[string]struct {
		src  string
		want []string
	}{
		"many": {"-- name: Q :many\nSELECT id FROM t WHERE id = :id;\n", []string{
			"rows, err := q.db.QueryContext(ctx, sqlText, args...)\n\tif err != nil {\n\t\terr = runtime.CtxErr(ctx, err)\n\t\tq.observeExec(ctx, \"Q\", key, execStart, -1, err)\n\t\treturn nil, err\n",
			"if err := rows.Scan(&i.ID); err != nil {\n\t\t\terr = runtime.CtxErr(ctx, err)\n\t\t\tq.observeExec(ctx, \"Q\", key, execStart, -1, err)\n\t\t\treturn nil, err\n",
			"err = runtime.CtxErr(ctx, rows.Err())\n\tq.observeExec(ctx, \"Q\", key, execStart, int64(len(items)), err)\n\treturn items, err\n",
		}},
		"one": {"-- name: Q :one\nSELECT id FROM t WHERE id = :id;\n", []string{
			"if err := row.Scan(&i.ID); err != nil {\n\t\terr = runtime.CtxErr(ctx, err)\n\t\tq.observeExec(ctx, \"Q\", key, execStart, -1, err)\n\t\treturn zero, err\n",
		}},
		// The no-rows branch comes FIRST and stays (None, nil).
		"maybeone": {"-- name: Q :maybe-one\nSELECT id FROM t WHERE id = :id;\n", []string{
			"if errors.Is(err, sql.ErrNoRows) {\n\t\t\tq.observeExec(ctx, \"Q\", key, execStart, 0, nil)\n\t\t\treturn zero, nil\n\t\t}\n\t\terr = runtime.CtxErr(ctx, err)\n\t\tq.observeExec(ctx, \"Q\", key, execStart, -1, err)\n\t\treturn zero, err\n",
		}},
		"exec": {"-- name: Q :exec\nDELETE FROM t WHERE id = :id;\n", []string{
			"res, err := q.db.ExecContext(ctx, sqlText, args...)\n\tif err != nil {\n\t\terr = runtime.CtxErr(ctx, err)\n\t\tq.observeExec(ctx, \"Q\", key, execStart, -1, err)\n\t\treturn err\n",
		}},
		"execrows": {"-- name: Q :execrows\nDELETE FROM t WHERE id = :id;\n", []string{
			"res, err := q.db.ExecContext(ctx, sqlText, args...)\n\tif err != nil {\n\t\terr = runtime.CtxErr(ctx, err)\n\t\tq.observeExec(ctx, \"Q\", key, execStart, -1, err)\n\t\treturn 0, err\n",
			"if rerr != nil {\n\t\trerr = runtime.CtxErr(ctx, rerr)\n\t\tq.observeExec(ctx, \"Q\", key, execStart, -1, rerr)\n\t\treturn 0, rerr\n",
		}},
	}
	for name, c := range cases {
		got := genCtxErr(t, true, c.src)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: missing\n%s\n----\n%s", name, w, got)
			}
		}
		if cnt := strings.Count(got, "runtime.CtxErr(ctx, "); cnt != len(c.want) {
			t.Errorf("%s: %d normalizations, want %d\n----\n%s", name, cnt, len(c.want), got)
		}
		// Off (MySQL): no normalization anywhere.
		if off := genCtxErr(t, false, c.src); strings.Contains(off, "CtxErr") {
			t.Errorf("%s: normalization emitted without the option\n----\n%s", name, off)
		}
	}
}

// The filter-tree path normalizes too, with the tree observer.
func TestGenerate_SQLiteCtxErrFilterTree(t *testing.T) {
	src := `-- name: Pick :many
SELECT t.id FROM t
WHERE TRUE
  AND @filter-tree!(scope)
@predicate(tenant)
t.tenant_id = :scope_tenant_id
@end;
`
	f, diags := template.NewScanner(mysql.Profile{}).ScanFile("t.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	files, ds := Generate(Options{Package: "gen", Style: runtime.StyleQuestion, NormalizeCtxErr: true}, mysql.TypeMap{},
		[]QueryInput{{Q: q, Frags: BuildFrags(mysql.Profile{}, q),
			Columns:    []dialect.ColumnDesc{{Name: "id", Type: dialect.TypeRef{OID: 8}}},
			Nullable:   []bool{false},
			ParamTypes: map[string]dialect.TypeRef{"scope_tenant_id": {OID: 8}}}})
	if len(ds) != 0 {
		t.Fatalf("generate: %+v", ds)
	}
	got := string(files["pick.sql.gen.go"])
	if !strings.Contains(got, "err = runtime.CtxErr(ctx, rows.Err())\n\tq.observeExecTree(ctx, \"Pick\", key, scope, execStart, int64(len(items)), err)\n") {
		t.Errorf("tree terminal error not normalized\n----\n%s", got)
	}
	if cnt := strings.Count(got, "runtime.CtxErr(ctx, "); cnt != 3 {
		t.Errorf("%d normalizations, want 3\n----\n%s", cnt, got)
	}
	// The reject branch (composition error) is not a driver error.
	if strings.Contains(got, "CtxErr(ctx, runtime.ErrFilterRequired") {
		t.Errorf("reject path normalized\n----\n%s", got)
	}
}
