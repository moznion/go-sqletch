package rules

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/codegen"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/template"
	"github.com/moznion/go-sqletch/runtime"
)

const filterTemplate = `-- name: ListOrders :many
SELECT o.id, o.total FROM orders AS o
WHERE TRUE
  AND @filter-tree!(scope)
@predicate(tenant)
o.tenant_id = :tenant_id
@predicate(org)
o.org_id = :org_id
@predicate(created_in)
o.created_at >= :from AND o.created_at < :to
@end
@if-present(min_total)
  AND o.total >= :min_total
@endif
ORDER BY o.id DESC;
`

func TestFilterTree_ScanAndRules(t *testing.T) {
	q := scanOne(t, filterTemplate)
	var ft *template.FilterTree
	for _, it := range q.Items {
		if v, ok := it.(*template.FilterTree); ok {
			ft = v
		}
	}
	if ft == nil || !ft.Required || ft.Param != "scope" || len(ft.Predicates) != 3 {
		t.Fatalf("filter-tree = %+v", ft)
	}
	if got := ft.Predicates[2].Params; len(got) != 2 || got[0] != "from" || got[1] != "to" {
		t.Fatalf("predicate params = %v", got)
	}
	if diags := CheckLexical(postgres.Profile{}, q); len(diags) != 0 {
		t.Fatalf("lexical: %+v", diags)
	}
	if diags := checkR1(t, filterTemplate); len(diags) != 0 {
		t.Fatalf("R1: %+v", diags)
	}
	// Maximal rendering conjoins every predicate, parenthesized.
	rs, err := ast.Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rs[0].SQL,
		"AND ((o.tenant_id = $1) AND (o.org_id = $2) AND (o.created_at >= $3 AND o.created_at < $4))") {
		t.Fatalf("maximal tree emission:\n%s", rs[0].SQL)
	}
}

// The tree conformance obligation: the runtime composition of
// And(all predicates in order) must be byte-identical to the verified
// maximal rendering, with equivalent bind naming.
func TestFilterTree_ComposeConformance(t *testing.T) {
	q := scanOne(t, filterTemplate)
	rs, err := ast.Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	frags := codegen.BuildFrags(postgres.Profile{}, q)

	tree := runtime.And(
		runtime.NewLeaf(0, int64(1)),
		runtime.NewLeaf(1, int64(2)),
		runtime.NewLeaf(2, "a", "b"),
	)
	key := runtime.ShapeKey{Guards: 1} // min_total active (maximal)
	sql, binds, err := runtime.ComposeTree(frags, key, tree, runtime.DefaultTreeCaps)
	if err != nil {
		t.Fatal(err)
	}
	if sql != rs[0].SQL {
		t.Fatalf("runtime tree != verified maximal:\nruntime:\n%q\nrenderer:\n%q", sql, rs[0].SQL)
	}
	// Bind naming equivalence: renderer ParamsSeq names vs bind plan.
	treeNames := []string{"tenant_id", "org_id", "from", "to"}
	if len(binds) != len(rs[0].ParamsSeq) {
		t.Fatalf("binds = %d, ParamsSeq = %d", len(binds), len(rs[0].ParamsSeq))
	}
	for i, b := range binds {
		var name string
		if b.FromTree {
			name = treeNames[b.Idx]
		} else {
			name = q.ParamOrder[b.Idx]
		}
		if name != rs[0].ParamsSeq[i] {
			t.Fatalf("bind %d = %q, renderer expects %q", i, name, rs[0].ParamsSeq[i])
		}
	}
}

func TestFilterTree_RuntimeShapes(t *testing.T) {
	q := scanOne(t, filterTemplate)
	frags := codegen.BuildFrags(postgres.Profile{}, q)
	fe := postgres.Frontend{}

	cases := []*runtime.Tree{
		nil, // non-required path renders TRUE (required-ness is enforced in generated code)
		runtime.Unscoped(),
		runtime.NewLeaf(0, int64(1)),
		runtime.Or(runtime.NewLeaf(0, int64(1)), runtime.NewLeaf(1, int64(2))),
		runtime.And(
			runtime.NewLeaf(2, "x", "y"),
			runtime.Or(runtime.NewLeaf(0, int64(1)), runtime.NewLeaf(0, int64(9))), // repeated predicate
		),
	}
	for _, tree := range cases {
		sql, binds, err := runtime.ComposeTree(frags, runtime.ShapeKey{}, tree, runtime.DefaultTreeCaps)
		if err != nil {
			t.Fatalf("tree %s: %v", tree.Encode(), err)
		}
		if _, err := fe.Parse(sql); err != nil {
			t.Fatalf("tree %s does not parse: %v\n%s", tree.Encode(), err, sql)
		}
		// Repeated predicates bind independently.
		if tree != nil && tree.Encode() == "&(p2,|(p0,p0))" && len(binds) != 4 {
			t.Fatalf("repeated predicate binds = %d, want 4", len(binds))
		}
	}

	// Caps: a runaway tree errors before any SQL is produced.
	deep := runtime.NewLeaf(0, int64(1))
	for range 10 {
		deep = runtime.And(deep, runtime.NewLeaf(1, int64(2)))
	}
	if _, _, err := runtime.ComposeTree(frags, runtime.ShapeKey{}, deep, runtime.DefaultTreeCaps); err == nil {
		t.Fatal("expected ErrTreeTooLarge for a deep tree")
	}
}

func TestFilterTree_Violations(t *testing.T) {
	mixedBind := `-- name: Bad :many
SELECT o.id FROM orders AS o
WHERE o.tenant_id = :tenant_id
  AND @filter-tree(scope)
@predicate(tenant)
o.tenant_id = :tenant_id
@end
;
`
	q := scanOne(t, mixedBind)
	if diags := CheckLexical(postgres.Profile{}, q); !hasCode(diags, diagnostics.CodeChooseParamBinds) {
		t.Fatalf("want SQLETCH112 for mixed in/out binding, got %+v", diags)
	}

	twoBlocks := `-- name: Bad :many
SELECT o.id FROM orders AS o
WHERE TRUE
  AND @filter-tree(a)
@predicate(x)
o.a = :a
@end
  AND @filter-tree(b)
@predicate(y)
o.b = :b
@end
;
`
	_, diags := template.NewScanner(postgres.Profile{}).ScanFile("t.sql", []byte(twoBlocks))
	if !hasCode(diags, diagnostics.CodeChooseStructure) {
		t.Fatalf("want SQLETCH009 for two blocks, got %+v", diags)
	}

	smuggle := `-- name: Bad :many
SELECT o.id FROM orders AS o
WHERE TRUE
  AND @filter-tree(scope)
@predicate(x)
o.a = :a; DROP TABLE orders
@end
;
`
	if diags := checkR1(t, smuggle); !hasCode(diags, diagnostics.CodeNodeIncomplete) {
		t.Fatalf("want SQLETCH102 for predicate smuggling, got %+v", diags)
	}

	joinRef := `-- name: Bad :many
SELECT u.id FROM users AS u
@if-present(oid)
JOIN organization_users AS ou ON ou.user_id = u.id AND ou.organization_id = :oid
@endif
WHERE TRUE
  AND @filter-tree(scope)
@predicate(org)
ou.created_at > :since
@end
;
`
	if diags := checkResolved(t, joinRef); !hasCode(diags, diagnostics.CodeScopeViolation) {
		t.Fatalf("want SQLETCH115 for predicate referencing optional join, got %+v", diags)
	}
}
