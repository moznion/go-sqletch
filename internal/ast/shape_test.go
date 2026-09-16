package ast

import (
	"regexp"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/template"
)

// shapeNames renders q with profile and returns the shape name of every
// verified rendering, in enumeration order.
func shapeNames(t *testing.T, profile dialect.LexerProfile, src string) []string {
	t.Helper()
	f, diags := template.NewScanner(profile).ScanFile("test.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan diagnostics: %+v", diags)
	}
	if len(f.Queries) != 1 {
		t.Fatalf("queries = %d, want 1", len(f.Queries))
	}
	rs, err := Renderings(profile, f.Queries[0])
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Shape
	}
	return out
}

func eqNames(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("shapes = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("shapes = %v, want %v", got, want)
		}
	}
}

func TestShape_Choose(t *testing.T) {
	// The maximal rendering takes ordinal 0 of every @choose, so the
	// named shapes are the remaining ordinals plus @default.
	eqNames(t, shapeNames(t, postgres.Profile{}, useCase1),
		[]string{"maximal", "case-sort-email_asc", "case-sort-default"})
}

const orderBySrc = `-- name: ListUsersSorted :many
SELECT u.id, u.email, u.created_at
FROM users AS u
WHERE TRUE
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

func TestShape_OrderByDefault(t *testing.T) {
	eqNames(t, shapeNames(t, postgres.Profile{}, orderBySrc),
		[]string{"maximal", "order-default-sort"})
}

const filterTreeSrc = `-- name: FilterUsers :many
SELECT u.id, u.email
FROM users AS u
WHERE TRUE
  AND @filter-tree!(scope)
@predicate(tenant)
u.tenant_id = :scope_tenant_id
@predicate(status_eq)
u.status = :scope_status
@end
ORDER BY u.id;
`

func TestShape_FilterTreeEmpty(t *testing.T) {
	eqNames(t, shapeNames(t, postgres.Profile{}, filterTreeSrc),
		[]string{"maximal", "tree-empty-scope"})
}

const inSrc = `-- name: UsersInStatuses :many
-- @param tenant_id: bigint
-- @param statuses: varchar(32)
SELECT u.id, u.email
FROM users AS u
WHERE u.tenant_id = :tenant_id
  AND u.status @in(:statuses)
ORDER BY u.id;
`

func TestShape_InEmpty(t *testing.T) {
	// The arity-0 form is its own rendering on expanding dialects only.
	eqNames(t, shapeNames(t, mysql.Profile{}, inSrc), []string{"maximal", "in-empty-statuses"})
	eqNames(t, shapeNames(t, postgres.Profile{}, inSrc), []string{"maximal"})
}

// TestShape_CollidingParams pins the disambiguation rule: two blocks
// may carry the same parameter name, and the resulting names must stay
// distinct — a collision would make two renderings share one cache
// path, and each run would evict the other's entry.
func TestShape_CollidingParams(t *testing.T) {
	rs := []Rendering{
		{Kind: RenderMaximal},
		{Kind: RenderCase, ChooseIdx: 0, CaseIdx: 1},
		{Kind: RenderCase, ChooseIdx: 1, CaseIdx: 1},
	}
	q := &template.QueryTemplate{Items: []template.Item{
		&template.Choose{Param: "sort", Cases: []template.ChooseCase{{Name: "a"}, {Name: "b"}}},
		&template.Choose{Param: "sort", Cases: []template.ChooseCase{{Name: "a"}, {Name: "b"}}},
	}}
	assignShapes(q, rs)
	got := []string{rs[0].Shape, rs[1].Shape, rs[2].Shape}
	eqNames(t, got, []string{"maximal", "case-sort-b-0", "case-sort-b-1"})
}

// TestShape_UnnamedFallback covers the defensive accessors: a shape
// name is a file name, and a malformed or out-of-range block index
// must degrade to a usable name rather than panic.
func TestShape_UnnamedFallback(t *testing.T) {
	rs := []Rendering{
		{Kind: RenderCase, ChooseIdx: 7, CaseIdx: 3},
		{Kind: RenderOrderDefault, OrderIdx: 7},
		{Kind: RenderInEmpty, InIdx: 7},
		{Kind: RenderTreeEmpty, TreeIdx: 7},
	}
	assignShapes(&template.QueryTemplate{}, rs)
	eqNames(t, []string{rs[0].Shape, rs[1].Shape, rs[2].Shape, rs[3].Shape},
		[]string{"case-_-3", "order-default-_", "in-empty-_", "tree-empty-_"})
}

var shapeSegment = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// TestShape_PathSafeAndUnique is the invariant the cache layout rests
// on: every rendering of a query has a distinct, path-safe name.
func TestShape_PathSafeAndUnique(t *testing.T) {
	cases := []struct {
		profile dialect.LexerProfile
		src     string
	}{
		{postgres.Profile{}, useCase1},
		{postgres.Profile{}, orderBySrc},
		{postgres.Profile{}, filterTreeSrc},
		{postgres.Profile{}, inSrc},
		{mysql.Profile{}, inSrc},
	}
	for _, c := range cases {
		seen := map[string]bool{}
		for _, name := range shapeNames(t, c.profile, c.src) {
			if !shapeSegment.MatchString(name) {
				t.Errorf("shape %q is not a safe path segment", name)
			}
			if seen[name] {
				t.Errorf("duplicate shape name %q", name)
			}
			seen[name] = true
		}
	}
}
