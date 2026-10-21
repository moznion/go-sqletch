package policy

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
)

// Security finding F9 (CWE-863, owner decision 2026-10-21): a
// data-modifying CTE (PostgreSQL wCTE) can UPDATE/DELETE/upsert a
// designated table under an outer statement of a kind the policy does
// not cover. Applicability used to key only on the OUTER kind, so the
// nested modification was silently outside the policy — no weave, no
// SQLETCH125, no SQLETCH124. When the nested DML's own kind is covered
// the policy is active and the statement is refused, whatever the outer
// kind.

func postsPolicy(kinds ...dialect.StmtKind) Policy {
	return Policy{
		Name:      "tenant_scope",
		Tables:    []string{"posts"},
		Predicate: "{}.tenant_id = :tenant_id",
		ParamName: "tenant_id",
		ParamType: "bigint",
		Kinds:     kinds,
	}
}

var wcteRefusals = []struct {
	name  string
	src   string
	kinds []dialect.StmtKind
}{
	{
		name:  "DELETE wCTE under INSERT, applies_to [update, delete]",
		src:   "-- name: Q :exec\nWITH d AS (DELETE FROM posts WHERE id = :id RETURNING *) INSERT INTO posts_archive SELECT * FROM d\n",
		kinds: []dialect.StmtKind{dialect.StmtUpdate, dialect.StmtDelete},
	},
	{
		name:  "UPDATE wCTE under SELECT, applies_to [update, delete]",
		src:   "-- name: Q :many\nWITH u AS (UPDATE posts SET title = :title WHERE id = :id RETURNING id) SELECT id FROM u\n",
		kinds: []dialect.StmtKind{dialect.StmtUpdate, dialect.StmtDelete},
	},
	{
		name:  "DELETE wCTE under SELECT, applies_to [delete]",
		src:   "-- name: Q :many\nWITH d AS (DELETE FROM posts WHERE id = :id RETURNING id) SELECT id FROM d\n",
		kinds: []dialect.StmtKind{dialect.StmtDelete},
	},
	{
		name:  "upsert wCTE under SELECT, applies_to [update]",
		src:   "-- name: Q :many\nWITH i AS (INSERT INTO posts (id, title) VALUES (:id, :title) ON CONFLICT (id) DO UPDATE SET title = excluded.title RETURNING id) SELECT id FROM i\n",
		kinds: []dialect.StmtKind{dialect.StmtUpdate},
	},
	{
		name:  "upsert wCTE under INSERT, applies_to [update]",
		src:   "-- name: Q :exec\nWITH i AS (INSERT INTO posts (id, title) VALUES (:id, :title) ON CONFLICT (id) DO UPDATE SET title = excluded.title RETURNING id) INSERT INTO audit (post_id) SELECT id FROM i\n",
		kinds: []dialect.StmtKind{dialect.StmtUpdate},
	},
	{
		name:  "UPDATE wCTE under DELETE of another table, applies_to [update]",
		src:   "-- name: Q :exec\nWITH u AS (UPDATE posts SET title = :title WHERE id = :id RETURNING id) DELETE FROM drafts WHERE post_id IN (SELECT id FROM u)\n",
		kinds: []dialect.StmtKind{dialect.StmtUpdate},
	},
}

func TestWeaveWCTE_CoveredNestedDMLRefused(t *testing.T) {
	for _, tc := range wcteRefusals {
		t.Run(tc.name, func(t *testing.T) {
			q := scanOne(t, tc.src)
			res := weaveOne(t, tc.src, postsPolicy(tc.kinds...))
			if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
				t.Fatalf("want exactly one SQLETCH125, got %+v", res.Diags)
			}
			if !strings.Contains(res.Diags[0].Message, "data-modifying") {
				t.Errorf("message should name the data-modifying CTE: %q", res.Diags[0].Message)
			}
			if !strings.Contains(res.Diags[0].Message, `"posts"`) {
				t.Errorf("message should name the designated table: %q", res.Diags[0].Message)
			}
			if len(res.Woven) != 0 || renderSQL(t, res.Query) != renderSQL(t, q) {
				t.Errorf("a refused query must not be woven: %s", renderSQL(t, res.Query))
			}
		})
	}
}

// Enforce is the weaver-regression backstop: fed the (unwoven)
// template directly, it must flag the nested modification (SQLETCH124)
// rather than credit it.
func TestEnforceWCTE_CoveredNestedDMLFlagged(t *testing.T) {
	for _, tc := range wcteRefusals {
		t.Run(tc.name, func(t *testing.T) {
			diags := enforceOn(t, scanOne(t, tc.src), postsPolicy(tc.kinds...))
			if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
				t.Fatalf("want exactly one SQLETCH124, got %+v", diags)
			}
		})
	}
}

// The policy being active, an opt-out is honored (no SQLETCH125/124) and
// is not a stale opt-out (SQLETCH126).
func TestWCTE_OptOutHonored(t *testing.T) {
	src := "-- name: Q :many\n-- @policy-optout: tenant_scope (admin purge)\n" +
		"WITH d AS (DELETE FROM posts WHERE id = :id RETURNING id) SELECT id FROM d\n"
	pol := postsPolicy(dialect.StmtDelete)
	noDiags(t, weaveOne(t, src, pol))
	if diags := enforceOn(t, scanOne(t, src), pol); len(diags) != 0 {
		t.Errorf("opt-out on an active wCTE policy flagged: %+v", diags)
	}
}

// Must-stay: the refusal keys on the nested DML's kind AND target.
// A wCTE that modifies a non-designated table, a nested kind the policy
// does not cover, a DO NOTHING upsert, or a plain nested INSERT stays
// outside the policy exactly as before.
func TestWCTE_UncoveredOrUndesignatedUnaffected(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		kinds []dialect.StmtKind
	}{
		{
			name:  "wCTE deletes a non-designated table",
			src:   "-- name: Q :exec\nWITH d AS (DELETE FROM drafts WHERE id = :id RETURNING *) INSERT INTO drafts_archive SELECT * FROM d\n",
			kinds: []dialect.StmtKind{dialect.StmtUpdate, dialect.StmtDelete},
		},
		{
			name:  "nested DELETE kind not covered",
			src:   "-- name: Q :exec\nWITH d AS (DELETE FROM posts WHERE id = :id RETURNING *) INSERT INTO posts_archive SELECT * FROM d\n",
			kinds: []dialect.StmtKind{dialect.StmtUpdate},
		},
		{
			name:  "nested DO NOTHING upsert",
			src:   "-- name: Q :many\nWITH i AS (INSERT INTO posts (id) VALUES (:id) ON CONFLICT (id) DO NOTHING RETURNING id) SELECT id FROM i\n",
			kinds: []dialect.StmtKind{dialect.StmtUpdate},
		},
		{
			name:  "nested plain INSERT",
			src:   "-- name: Q :many\nWITH i AS (INSERT INTO posts (id) VALUES (:id) RETURNING id) SELECT id FROM i\n",
			kinds: []dialect.StmtKind{dialect.StmtUpdate, dialect.StmtDelete},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := scanOne(t, tc.src)
			res := weaveOne(t, tc.src, postsPolicy(tc.kinds...))
			noDiags(t, res)
			if len(res.Woven) != 0 || renderSQL(t, res.Query) != renderSQL(t, q) {
				t.Errorf("query altered: %s", renderSQL(t, res.Query))
			}
			if diags := enforceOn(t, q, postsPolicy(tc.kinds...)); len(diags) != 0 {
				t.Errorf("enforcement flagged: %+v", diags)
			}
		})
	}
}

// A wCTE living only in a non-first @choose alternative is invisible to
// the maximal rendering; the nested-DML refusal must still see it (the
// overread scan carries every non-maximal rendering's NestedDML). The
// outer kind (SELECT) is uncovered, so the plain overread refusal does
// not fire — only the nested-DML rule can.
func TestWCTE_NonMaximalChooseAlternativeRefused(t *testing.T) {
	src := "-- name: Q :many\n" +
		"SELECT id FROM drafts\n" +
		"@choose(sort)\n" +
		"@case(by_id)\n" +
		"ORDER BY id\n" +
		"@case(purge)\n" +
		"ORDER BY (WITH d AS (DELETE FROM posts RETURNING id) SELECT max(id) FROM d)\n" +
		"@end\n"
	pol := postsPolicy(dialect.StmtDelete)
	res := weaveOne(t, src, pol)
	if len(res.Diags) != 1 || res.Diags[0].Code != diagnostics.CodePolicyUnweavable {
		t.Fatalf("want exactly one SQLETCH125, got %+v", res.Diags)
	}
	diags := enforceOn(t, scanOne(t, src), pol)
	if len(diags) != 1 || diags[0].Code != diagnostics.CodePolicyUnscoped {
		t.Fatalf("want exactly one SQLETCH124, got %+v", diags)
	}
}
