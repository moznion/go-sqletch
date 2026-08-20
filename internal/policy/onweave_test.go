package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

// D2(a): a designated table on the null-extended side of an outer
// join is woven into that join's ON clause — the outer row set is
// preserved and only the joined rows are scoped.
func TestWeaveON_Golden(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "LEFT JOIN weaves into ON",
			src:  "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE u.ok\n",
			want: "SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND o.tenant_id = $1 WHERE u.ok",
		},
		{
			name: "no WHERE at all: ON weave only",
			src:  "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id\n",
			want: "SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND o.tenant_id = $1",
		},
		{
			name: "RIGHT JOIN null-extends its left side",
			src:  "-- name: Q :many\nSELECT u.id FROM orders o RIGHT JOIN users u ON o.user_id = u.id WHERE u.ok\n",
			want: "SELECT u.id FROM orders o RIGHT JOIN users u ON o.user_id = u.id AND o.tenant_id = $1 WHERE u.ok",
		},
		// NOTE: a FULL JOIN preserves both operands, so no ON clause can
		// scope the designated table's own rows — that case is REFUSED
		// (SQLETCH125), asserted in TestWeaveON_WrongJoinRefuses.
		{
			name: "mixed: WHERE for the inner table, ON for the outer one",
			src:  "-- name: Q :many\nSELECT o.id FROM orders o LEFT JOIN order_items i ON i.order_id = o.id WHERE o.ok\n",
			want: "SELECT o.id FROM orders o LEFT JOIN order_items i ON i.order_id = o.id AND i.tenant_id = $1 WHERE o.tenant_id = $1 AND o.ok",
		},
		{
			name: "ON expression with a top-level OR is wrapped first",
			src:  "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id OR o.owner_id = u.id WHERE u.ok\n",
			want: "SELECT u.id FROM users u LEFT JOIN orders o ON (o.user_id = u.id OR o.owner_id = u.id) AND o.tenant_id = $1 WHERE u.ok",
		},
		{
			name: "join chained after the outer join",
			src:  "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id JOIN teams t ON t.id = u.team_id WHERE u.ok\n",
			want: "SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND o.tenant_id = $1 JOIN teams t ON t.id = u.team_id WHERE u.ok",
		},
		{
			name: "parenthesized ON expression",
			src:  "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON (o.user_id = u.id) WHERE u.ok\n",
			want: "SELECT u.id FROM users u LEFT JOIN orders o ON (o.user_id = u.id) AND o.tenant_id = $1 WHERE u.ok",
		},
		{
			name: "hand-scoped ON conjunct is not double-woven",
			src:  "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND o.tenant_id = :tenant_id WHERE u.ok\n",
			want: "SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND o.tenant_id = $1 WHERE u.ok",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := weaveOne(t, tc.src, tenantPolicy())
			noDiags(t, res)
			if got := renderSQL(t, res.Query); got != tc.want {
				t.Errorf("woven rendering:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// Two policies on the same outer-joined table extend the same ON
// clause in declaration order.
func TestWeaveON_MultiplePolicies(t *testing.T) {
	soft := Policy{Name: "soft_delete", Tables: []string{"orders"}, Predicate: "{}.deleted_at IS NULL"}
	src := "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE u.ok\n"
	res := weaveOne(t, src, tenantPolicy(), soft)
	noDiags(t, res)
	want := "SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id AND o.tenant_id = $1 AND o.deleted_at IS NULL WHERE u.ok"
	if got := renderSQL(t, res.Query); got != want {
		t.Errorf("got: %s\nwant: %s", got, want)
	}
}

// The phase-1 precedence gap, fixed here: a WHERE clause with a
// top-level OR is parenthesized before the conjunct is prepended, so
// the scoping binds above the OR in every disjunct.
func TestWeave_WhereWithTopLevelORIsWrapped(t *testing.T) {
	src := "-- name: Q :many\nSELECT id FROM orders WHERE status = 'a' OR status = 'b'\n"
	res := weaveOne(t, src, tenantPolicy())
	noDiags(t, res)
	want := "SELECT id FROM orders WHERE orders.tenant_id = $1 AND (status = 'a' OR status = 'b')"
	if got := renderSQL(t, res.Query); got != want {
		t.Errorf("got: %s\nwant: %s", got, want)
	}
	// And the wrapped result satisfies enforcement.
	if diags := enforceOn(t, res.Query, tenantPolicy()); len(diags) != 0 {
		t.Errorf("enforcement rejects the wrapped weave: %+v", diags)
	}
}

// Enforcement checks the ON clause for nullable-side occurrences: the
// weaver's ON output passes, an unwoven template fails, and a WHERE
// copy of the conjunct does NOT satisfy an ON requirement.
func TestEnforceON(t *testing.T) {
	woven := weaveOne(t, "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE u.ok\n", tenantPolicy())
	noDiags(t, woven)
	if diags := enforceOn(t, woven.Query, tenantPolicy()); len(diags) != 0 {
		t.Errorf("enforcement rejects the ON weave: %+v", diags)
	}

	unwoven := scanOne(t, "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE u.ok\n")
	diags := enforceOn(t, unwoven, tenantPolicy())
	if len(diags) != 1 || !strings.Contains(diags[0].Hint, "ON clause") {
		t.Errorf("unwoven outer join must fail with an ON-clause hint: %+v", diags)
	}

	// A WHERE-placed copy would inner-join the outer table; it must
	// not satisfy the ON requirement.
	misplaced := scanOne(t, "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE o.tenant_id = :tenant_id AND u.ok\n")
	diags = enforceOn(t, misplaced, tenantPolicy())
	if len(diags) != 1 {
		t.Errorf("WHERE-misplaced conjunct must not satisfy the ON requirement: %+v", diags)
	}
}

// ON weaving composes with everything downstream: renderings of the
// woven template stay parseable and the conjunct is unconditional
// (present in the minimal shape too).
func TestWeaveON_MinimalShapeCarriesConjunct(t *testing.T) {
	src := "-- name: Q :many\nSELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE u.ok\n@if-present(status)\nAND u.status = :status\n@endif\n"
	res := weaveOne(t, src, tenantPolicy())
	noDiags(t, res)
	minimal, err := ast.RenderShape(postgres.Profile{}, res.Query, 0, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(minimal.SQL, "AND o.tenant_id = $1") {
		t.Errorf("minimal shape lost the ON conjunct:\n%s", minimal.SQL)
	}
	if _, err := (postgres.Frontend{}).Parse(minimal.SQL); err != nil {
		t.Errorf("minimal woven shape does not parse: %v\n%s", err, minimal.SQL)
	}
}
