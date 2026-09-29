//go:build devdb

package e2e_test

import (
	"slices"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/policy"
	"github.com/moznion/go-sqletch/internal/rules"
)

// Row-level proof of set-operation branch weaving on SQLite (design 14
// §13), against the real engine: every woven query is verified by the
// same passes the pipeline runs (R1 on the woven template, Enforce,
// oracle Describe) and then EXECUTED per tenant. Each expected row set
// differs from what an unscoped branch would return.
//
// SharedAliasUnion is the pre-§13 leak: the facade's whole-statement
// Relations() of a compound SELECT is the FIRST core's, so the weaver
// used to splice audit_logs' conjunct at the statement's first WHERE —
// the archive branch's. Both branches alias `o` and archive carries
// tenant_id, so that was valid SQL scoping archive and leaving
// audit_logs open (tenant 1 saw 'secret' and 'crossed').
const policySetOpSQLiteSchema = `
CREATE TABLE audit_archive (
    id        INTEGER PRIMARY KEY,
    tenant_id INTEGER NOT NULL,
    action    TEXT NOT NULL
);
INSERT INTO audit_logs (id, tenant_id, actor_id, action) VALUES
  (1, 1, 1, 'login'), (2, 1, NULL, 'cron'), (3, 2, 9, 'secret'), (4, 2, 1, 'crossed');
INSERT INTO audit_archive (id, tenant_id, action) VALUES
  (10, 1, 'old-1'), (20, 2, 'old-2');
INSERT INTO users (email, status, tenant_id) VALUES
  ('alice@example.com', 'active', 1), ('bob@example.com', 'active', 1);
`

func TestSQLitePolicySetOpWeaving(t *testing.T) {
	conn, ctx := acquireSQLite(t)
	if err := conn.Exec(policySetOpSQLiteSchema); err != nil {
		t.Fatalf("schema/seed: %v", err)
	}
	oracle := sqlite.NewOracle(conn)
	pols := []policy.Policy{{
		Name:      "tenant_scope",
		Tables:    []string{"audit_logs"},
		Predicate: "{}.tenant_id = :tenant_id",
		ParamName: "tenant_id",
		ParamType: "integer",
	}}
	if d := policy.Validate(sqlite.Profile{}, sqlite.Frontend{}, pols, "sqletch.yaml"); len(d) != 0 {
		t.Fatalf("policy validation: %+v", d)
	}

	cases := []struct {
		name string
		src  string
		want map[int64][]string // tenant → rows (first column as text)
	}{
		{
			name: "SharedAliasUnion",
			src: `-- name: SharedAliasUnion :many
SELECT o."action" FROM audit_logs AS o
UNION ALL
SELECT o."action" FROM audit_archive AS o WHERE o."action" <> 'x'
ORDER BY 1;
`,
			// audit_archive is not designated: both tenants' archive rows.
			want: map[int64][]string{
				1: {"cron", "login", "old-1", "old-2"},
				2: {"crossed", "old-1", "old-2", "secret"},
			},
		},
		{
			name: "EveryBranch",
			src: `-- name: EveryBranch :many
SELECT a."action" FROM audit_logs AS a WHERE a.actor_id IS NOT NULL
UNION ALL
SELECT a."action" FROM audit_logs AS a WHERE a.actor_id IS NULL
ORDER BY 1;
`,
			want: map[int64][]string{1: {"cron", "login"}, 2: {"crossed", "secret"}},
		},
		{
			name: "OrphanActors",
			src: `-- name: OrphanActors :many
SELECT a.actor_id FROM audit_logs AS a WHERE a.actor_id IS NOT NULL
EXCEPT
SELECT u.id FROM users AS u
ORDER BY 1;
`,
			// Tenant 1's answer is EMPTY; an unscoped left operand says 9.
			want: map[int64][]string{1: nil, 2: {"9"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := compileSQLite(t, tc.src)
			wres := policy.Weave(sqlite.Profile{}, sqlite.Frontend{}, pols, q)
			if len(wres.Diags) != 0 {
				t.Fatalf("weave: %+v", wres.Diags)
			}
			wq := wres.Query
			rs, err := ast.Renderings(sqlite.Profile{}, wq)
			if err != nil {
				t.Fatal(err)
			}
			if d := rules.CheckR1(sqlite.Profile{}, sqlite.Frontend{}, wq, rs); len(d) != 0 {
				t.Fatalf("R1 on woven: %+v", d)
			}
			tree, err := sqlite.Frontend{}.Parse(rs[0].SQL)
			if err != nil {
				t.Fatal(err)
			}
			if d := policy.Enforce(sqlite.Profile{}, sqlite.Frontend{}, pols, wq, tree, rs[0]); len(d) != 0 {
				t.Fatalf("enforce rejects the woven template: %+v\n%s", d, rs[0].SQL)
			}
			if _, err := oracle.Describe(ctx, rs[0].SQL); err != nil {
				t.Fatalf("describe: %v\n%s", err, rs[0].SQL)
			}

			for tenant, want := range tc.want {
				stmt, _, err := conn.Prepare(rs[0].SQL)
				if err != nil {
					t.Fatalf("prepare: %v\n%s", err, rs[0].SQL)
				}
				for i, name := range rs[0].ParamsSeq {
					if name != "tenant_id" {
						t.Fatalf("unexpected parameter %q", name)
					}
					if err := stmt.BindInt64(i+1, tenant); err != nil {
						t.Fatal(err)
					}
				}
				var got []string
				for stmt.Step() {
					got = append(got, stmt.ColumnText(0))
				}
				if err := stmt.Err(); err != nil {
					t.Fatal(err)
				}
				_ = stmt.Close()
				if !slices.Equal(got, want) {
					t.Errorf("tenant %d: rows = %q, want %q\nSQL: %s", tenant, got, want, rs[0].SQL)
				}
			}
		})
	}
}

// The pre-§13 woven shape of SharedAliasUnion — the conjunct in the
// archive branch — must be rejected by enforcement.
func TestSQLitePolicySetOpLeakedShapeRejected(t *testing.T) {
	q := compileSQLite(t, `-- name: Leaked :many
-- @param tenant_id: integer
SELECT o."action" FROM audit_logs AS o
UNION ALL
SELECT o."action" FROM audit_archive AS o WHERE (o.tenant_id = :tenant_id) AND o."action" <> 'x'
ORDER BY 1;
`)
	rs, err := ast.Renderings(sqlite.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := sqlite.Frontend{}.Parse(rs[0].SQL)
	if err != nil {
		t.Fatal(err)
	}
	pols := []policy.Policy{{Name: "tenant_scope", Tables: []string{"audit_logs"}, Predicate: "{}.tenant_id = :tenant_id", ParamName: "tenant_id", ParamType: "integer"}}
	d := policy.Enforce(sqlite.Profile{}, sqlite.Frontend{}, pols, q, tree, rs[0])
	if len(d) != 1 || d[0].Code != diagnostics.CodePolicyUnscoped {
		t.Fatalf("want one SQLETCH124, got %+v", d)
	}
}
