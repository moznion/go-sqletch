package codegen

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/template"
	"github.com/moznion/go-sqletch/runtime"
)

// TestGenerate_ObserverSites pins the doc-18 observation sites in the
// generated code: the db-file surface (SetObserver, Cache, the
// helpers), the reject site on a validation branch, the guarded exec
// clock, and the exec event on every annotation path.
func TestGenerate_ObserverSites(t *testing.T) {
	q := scanOne(t, useCase1)
	files, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
		Q: q, Frags: BuildFrags(postgres.Profile{}, q),
		Columns: []dialect.ColumnDesc{
			{Name: "id", Type: dialect.TypeRef{OID: 20}},
			{Name: "email", Type: dialect.TypeRef{OID: 25}},
			{Name: "status", Type: dialect.TypeRef{OID: 25}},
			{Name: "created_at", Type: dialect.TypeRef{OID: 1114}},
		},
		Nullable: []bool{false, false, false, false},
		ParamTypes: map[string]dialect.TypeRef{
			"organization_id": {OID: 20}, "status": {OID: 25},
			"email_prefix": {OID: 25}, "limit": {OID: 20},
		},
	}})
	if len(diags) != 0 {
		t.Fatalf("generate: %+v", diags)
	}

	db := string(files["db.gen.go"])
	for _, want := range []string{
		"func (q *Queries) SetObserver(o runtime.Observer)",
		"q.cache.SetObserver(o)",
		"func (q *Queries) Cache() *runtime.ComposedCache",
		"func (q *Queries) observeExec(ctx context.Context, query string, key runtime.ShapeKey, start time.Time, rows int64, err error)",
		"func (q *Queries) observeReject(ctx context.Context, query string, err error)",
		// The observer/hook are atomic pointers so a SetObserver/OnQuery
		// that races in-flight reads cannot tear a two-word value.
		"obs     atomic.Pointer[runtime.Observer]",
		"onQuery atomic.Pointer[func(shapeKey, sql string)]",
		"nq.obs.Store(p)", // WithTx carries the observer
	} {
		if !strings.Contains(db, want) {
			t.Errorf("db.gen.go missing %q\n----\n%s", want, db)
		}
	}

	src := string(files["search_users.sql.gen.go"])
	for _, want := range []string{
		// ChooseOrdinal's failure is a reject, reported before return.
		"q.observeReject(ctx, \"SearchUsers\", err)",
		// The exec clock runs only for observed calls.
		"var execStart time.Time\n\tif q.obs.Load() != nil {\n\t\texecStart = time.Now()\n\t}",
		// :many reports the scanned row count and the terminal error.
		"q.observeExec(ctx, \"SearchUsers\", key, execStart, int64(len(items)), rows.Err())",
		// Failure paths report rows -1 with the driver error.
		"q.observeExec(ctx, \"SearchUsers\", key, execStart, -1, err)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("query file missing %q\n----\n%s", want, src)
		}
	}
}

// TestGenerate_ObserverTreeQuery pins the @filter-tree variant: exec
// events route through observeExecTree (which folds the `;t=` segment
// under the observer guard), and the required-tree refusal is a
// reject event.
func TestGenerate_ObserverTreeQuery(t *testing.T) {
	q := scanOne(t, `-- name: Pick :many
SELECT t.id FROM t
WHERE TRUE
  AND @filter-tree!(scope)
@predicate(tenant)
t.tenant_id = :scope_tenant_id
@end;
`)
	files, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
		Q: q, Frags: BuildFrags(postgres.Profile{}, q),
		Columns:    []dialect.ColumnDesc{{Name: "id", Type: dialect.TypeRef{OID: 20}}},
		Nullable:   []bool{false},
		ParamTypes: map[string]dialect.TypeRef{"scope_tenant_id": {OID: 20}},
	}})
	if len(diags) != 0 {
		t.Fatalf("generate: %+v", diags)
	}

	src := string(files["pick.sql.gen.go"])
	for _, want := range []string{
		"q.observeReject(ctx, \"Pick\", runtime.ErrFilterRequired)",
		"q.observeExecTree(ctx, \"Pick\", key, scope, execStart, int64(len(items)), rows.Err())",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("query file missing %q\n----\n%s", want, src)
		}
	}
	if !strings.Contains(string(files["db.gen.go"]), "func (q *Queries) observeExecTree(") {
		t.Error("db.gen.go lacks observeExecTree for a package with a filter tree")
	}
}

// TestGenerate_ShapeSpace pins the registry: enumerable counts from
// shape.Count, the unbounded flag for @filter-tree everywhere and for
// @in on expanding dialects only, and sorted-order emission.
func TestGenerate_ShapeSpace(t *testing.T) {
	// useCase1: 3 guards × 3 @choose ordinals = 24 enumerable shapes.
	q := scanOne(t, useCase1)
	files, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
		Q: q, Frags: BuildFrags(postgres.Profile{}, q),
		Columns: []dialect.ColumnDesc{
			{Name: "id", Type: dialect.TypeRef{OID: 20}},
			{Name: "email", Type: dialect.TypeRef{OID: 25}},
			{Name: "status", Type: dialect.TypeRef{OID: 25}},
			{Name: "created_at", Type: dialect.TypeRef{OID: 1114}},
		},
		Nullable: []bool{false, false, false, false},
		ParamTypes: map[string]dialect.TypeRef{
			"organization_id": {OID: 20}, "status": {OID: 25},
			"email_prefix": {OID: 25}, "limit": {OID: 20},
		},
	}})
	if len(diags) != 0 {
		t.Fatalf("generate: %+v", diags)
	}
	db := string(files["db.gen.go"])
	if !strings.Contains(db, "var ShapeSpace = map[string]runtime.ShapeSpaceInfo{") {
		t.Fatalf("db.gen.go lacks the ShapeSpace registry\n----\n%s", db)
	}
	if !strings.Contains(db, `"SearchUsers": {Enumerable: 24, Exact: true, Unbounded: false}`) {
		t.Errorf("SearchUsers entry wrong\n----\n%s", db)
	}
}

func TestShapeSpaceEntry(t *testing.T) {
	// A @filter-tree query is unbounded on every dialect; its
	// enumerable dimensions still count (here: none → 1).
	tree := scanOne(t, `-- name: Pick :many
SELECT t.id FROM t
WHERE TRUE
  AND @filter-tree!(scope)
@predicate(tenant)
t.tenant_id = :scope_tenant_id
@end;
`)
	if got := shapeSpaceEntry(tree, runtime.StyleDollar); got != "\t\"Pick\": {Enumerable: 1, Exact: true, Unbounded: true}," {
		t.Errorf("tree entry: %s", got)
	}

	// @in is a shape dimension only on expanding dialects: unbounded
	// under StyleQuestion, invisible under StyleDollar.
	f, diags := template.NewScanner(mysql.Profile{}).ScanFile("t.sql", []byte(inListTemplate))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	in := f.Queries[0]
	if got := shapeSpaceEntry(in, runtime.StyleQuestion); !strings.Contains(got, "Enumerable: 2, Exact: true, Unbounded: true") {
		t.Errorf("expanding @in entry: %s", got)
	}
	if got := shapeSpaceEntry(in, runtime.StyleDollar); !strings.Contains(got, "Enumerable: 2, Exact: true, Unbounded: false") {
		t.Errorf("dollar @in entry: %s", got)
	}

	// 17 @order-by keys push shape.Count past uint64: the count must
	// saturate with Exact: false, never truncate silently.
	big := &template.QueryTemplate{Name: "Big", Items: []template.Item{
		&template.OrderBy{Param: "sort", Keys: make([]template.OrderKey, 17)},
	}}
	if got := shapeSpaceEntry(big, runtime.StyleDollar); got != "\t\"Big\": {Enumerable: 18446744073709551615, Exact: false, Unbounded: false}," {
		t.Errorf("saturated entry: %s", got)
	}
}
