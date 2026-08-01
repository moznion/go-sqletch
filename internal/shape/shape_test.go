package shape

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/template"
)

const useCase1 = `-- name: SearchUsers :many
SELECT u.id, u.email
FROM users AS u
@if-present(organization_id)
JOIN organization_users AS ou ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif
WHERE TRUE
@if-present(status)
  AND u.status = :status
@endif
@if-present(email_prefix)
  AND u.email LIKE :email_prefix || '%'
@endif
@if-present(created_after)
  AND u.created_at >= :created_after
@endif
@choose(sort)
@case(created_at_desc)
ORDER BY u.created_at DESC
@case(created_at_asc)
ORDER BY u.created_at ASC
@case(email_asc)
ORDER BY u.email ASC
@default
ORDER BY u.id ASC
@end
LIMIT :limit;
`

func scanOne(t *testing.T, src string) *template.QueryTemplate {
	t.Helper()
	f, diags := template.NewScanner(postgres.Profile{}).ScanFile("test.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	return f.Queries[0]
}

func TestCountAndEnumerate(t *testing.T) {
	q := scanOne(t, useCase1)
	if got := Count(q).Int64(); got != 64 {
		t.Fatalf("Count = %d, want 64 (2^4 guards x 4 cases)", got)
	}
	keys, truncated := Enumerate(q, 0)
	if truncated || len(keys) != 64 {
		t.Fatalf("Enumerate = %d keys (truncated=%v), want 64", len(keys), truncated)
	}
	seen := map[string]bool{}
	for _, k := range keys {
		s := k.String()
		if seen[s] {
			t.Fatalf("duplicate shape key %s", s)
		}
		seen[s] = true
	}
}

func TestEnumerate_Cap(t *testing.T) {
	q := scanOne(t, useCase1)
	keys, truncated := Enumerate(q, 10)
	if !truncated || len(keys) != 10 {
		t.Fatalf("capped enumerate = %d (truncated=%v), want 10/true", len(keys), truncated)
	}
}

// The parse-level half of the soundness property test: every reachable
// shape of Use Case 1 must render to parseable SQL. (The full
// prepare+EXPLAIN version runs against a live database in P4.)
func TestAllShapesParse(t *testing.T) {
	q := scanOne(t, useCase1)
	keys, _ := Enumerate(q, 0)
	fe := postgres.Frontend{}
	for _, k := range keys {
		r, err := ast.RenderShape(postgres.Profile{}, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
		if err != nil {
			t.Fatalf("shape %s: render: %v", k, err)
		}
		if _, err := fe.Parse(r.SQL); err != nil {
			t.Fatalf("shape %s does not parse: %v\nSQL:\n%s", k, err, r.SQL)
		}
	}
}

// Minimal shape: all guards off, default case — the WHERE TRUE anchor
// keeps it valid, and inactive params vanish from the placeholder set.
func TestRenderShape_Minimal(t *testing.T) {
	q := scanOne(t, useCase1)
	r, err := ast.RenderShape(postgres.Profile{}, q, 0, ast.CaseSelection{0: 3}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ParamsSeq) != 1 || r.ParamsSeq[0] != "limit" {
		t.Fatalf("minimal ParamsSeq = %v, want [limit]", r.ParamsSeq)
	}
	if _, err := (postgres.Frontend{}).Parse(r.SQL); err != nil {
		t.Fatalf("minimal shape does not parse: %v\n%s", err, r.SQL)
	}
}
