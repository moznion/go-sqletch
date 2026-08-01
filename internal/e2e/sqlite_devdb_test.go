//go:build devdb

package e2e_test

import (
	"context"
	"testing"
	"time"

	sqlite3 "github.com/ncruces/go-sqlite3"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/devdb"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/nullability"
	"github.com/moznion/go-sqletch/internal/rules"
	"github.com/moznion/go-sqletch/internal/shape"
	"github.com/moznion/go-sqletch/internal/template"
)

const sqliteSchemaSQL = `
CREATE TABLE users (
    id         INTEGER PRIMARY KEY,
    email      TEXT NOT NULL,
    status     TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    tenant_id  INTEGER NOT NULL,
    org_id     INTEGER,
    nickname   TEXT,
    bio        TEXT
);
CREATE TABLE organization_users (
    user_id         INTEGER NOT NULL,
    organization_id INTEGER NOT NULL,
    PRIMARY KEY (user_id, organization_id)
);
CREATE TABLE audit_logs (
    id        INTEGER PRIMARY KEY,
    tenant_id INTEGER NOT NULL,
    actor_id  INTEGER,
    action    TEXT NOT NULL
);
`

// sqliteCorpus mirrors the MySQL corpus in SQLite dialect: every bind
// parameter carries its mandatory annotation, and expression columns
// carry `-- @column` annotations.
var sqliteCorpus = map[string]string{
	"search_users": `-- name: SearchUsers :many
-- @param organization_id: integer
-- @param status: text
-- @param email_prefix: text
-- @param limit: integer
SELECT u.id, u.email, u.status
FROM users AS u
@if-present(organization_id)
JOIN organization_users AS ou
  ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif
WHERE TRUE
@if-present(status)
  AND u.status = :status
@endif
@if-present(email_prefix)
  AND u.email LIKE :email_prefix || '%'
@endif
@choose(sort)
@case(email_asc)
ORDER BY u.email ASC, u.id ASC
@default
ORDER BY u.id ASC
@end
LIMIT :limit;
`,
	"in_list": `-- name: UsersInStatuses :many
-- @param tenant_id: integer
-- @param statuses: text
-- @param min_id: integer
-- @param limit: integer
SELECT u.id, u.email, u.status
FROM users AS u
WHERE u.tenant_id = :tenant_id
  AND u.status @in(:statuses)
@if-present(min_id)
  AND u.id >= :min_id
@endif
ORDER BY u.id
LIMIT :limit;
`,
	"update_user_profile": `-- name: UpdateUserProfile :execrows
-- @param new_email: text
-- @param nickname: text
-- @param bio: text
-- @param id: integer
UPDATE users
SET
    tenant_id = tenant_id
@if-present(new_email)
  , email = :new_email
@endif
@if-present(nickname)
  , nickname = :nickname
@endif
@if-present(bio)
  , bio = :bio
@endif
WHERE id = :id;
`,
	"create_user": `-- name: CreateUser :execrows
-- @param email: text
-- @param status: text
-- @param tenant_id: integer
-- @param nickname: text
INSERT INTO users (
    email
  , status
  , tenant_id
@if-present(nickname)
  , nickname
@endif
) VALUES (
    :email
  , :status
  , :tenant_id
@if-present(nickname)
  , :nickname
@endif
);
`,
	"when_and_having": `-- name: TenantActivity :many
-- @param action: text
-- @param min_actions: integer
-- @column actions: integer
SELECT a.tenant_id, count(*) AS actions
FROM audit_logs AS a
WHERE TRUE
@when(include_cron = false)
  AND a.actor_id IS NOT NULL
@end
@if-present(action)
  AND a."action" = :action
@endif
GROUP BY a.tenant_id
HAVING TRUE
@if-present(min_actions)
  AND count(*) >= :min_actions
@endif
ORDER BY a.tenant_id;
`,
	"order_by_users": `-- name: OrderedUsers :many
-- @param status: text
-- @param limit: integer
SELECT u.id, u.email, u.created_at
FROM users AS u
WHERE TRUE
@if-present(status)
  AND u.status = :status
@endif
@order-by(sort)
@key(created_at)
u.created_at
@key(email)
u.email
@default
ORDER BY u.id ASC
@end
LIMIT :limit;
`,
	"filter_tree": `-- name: FilterUsers :many
-- @param scope_tenant_id: integer
-- @param scope_status: text
-- @param scope_prefix: text
-- @param limit: integer
SELECT u.id, u.email
FROM users AS u
WHERE TRUE
  AND @filter-tree!(scope)
@predicate(tenant)
u.tenant_id = :scope_tenant_id
@predicate(status_eq)
u.status = :scope_status
@predicate(email_prefix)
u.email LIKE :scope_prefix || '%'
@end
ORDER BY u.id
LIMIT :limit;
`,
}

func acquireSQLite(t *testing.T) (*sqlite3.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	conn, cleanup, err := devdb.AcquireSQLite(ctx, devdb.Config{
		ServerVersion: "3",
		SchemaSQL:     []string{sqliteSchemaSQL},
	})
	if err != nil {
		t.Fatalf("acquire SQLite dev database: %v", err)
	}
	t.Cleanup(cleanup)
	return conn, ctx
}

// compileSQLite runs the offline front half under the SQLite dialect.
func compileSQLite(t *testing.T, src string) *template.QueryTemplate {
	t.Helper()
	f, diags := template.NewScanner(sqlite.Profile{}).ScanFile("e2e.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	if d := rules.CheckLexical(sqlite.Profile{}, q); len(d) != 0 {
		t.Fatalf("lexical: %+v", d)
	}
	rs, err := ast.Renderings(sqlite.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	if d := rules.CheckR1(sqlite.Profile{}, sqlite.Frontend{}, q, rs); len(d) != 0 {
		t.Fatalf("R1: %+v", d)
	}
	return q
}

// TestSQLitePropertyAllShapesPrepareAndPlan: every enumerable shape —
// including both representative @in arities — must prepare (through
// SQLite's planner) and EXPLAIN QUERY PLAN on the in-process engine.
func TestSQLitePropertyAllShapesPrepareAndPlan(t *testing.T) {
	conn, ctx := acquireSQLite(t)
	oracle := sqlite.NewOracle(conn)

	for name, src := range sqliteCorpus {
		t.Run(name, func(t *testing.T) {
			q := compileSQLite(t, src)
			keys, truncated := shape.EnumerateExpand(q, 4096, true)
			if truncated {
				t.Fatalf("corpus template exceeds the test cap")
			}
			for _, k := range keys {
				r, err := ast.RenderShape(sqlite.Profile{}, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
				if err != nil {
					t.Fatalf("shape %s: render: %v", k, err)
				}
				if _, err := oracle.Describe(ctx, r.SQL); err != nil {
					t.Fatalf("shape %s fails to prepare: %v\nSQL:\n%s", k, err, r.SQL)
				}
				if err := oracle.Plan(ctx, r.SQL); err != nil {
					t.Fatalf("shape %s fails to plan: %v\nSQL:\n%s", k, err, r.SQL)
				}
			}
			t.Logf("%d shapes prepared and planned", len(keys))
		})
	}
}

// TestSQLiteNullability runs the analysis against real Describe
// output: catalog NOT NULL survives, nullable columns stay nullable,
// and a LEFT JOIN null-extends its right side.
func TestSQLiteNullability(t *testing.T) {
	conn, ctx := acquireSQLite(t)
	oracle := sqlite.NewOracle(conn)
	cat, err := oracle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	src := `-- name: N :many
SELECT u.id, u.email, u.org_id, u.nickname, ou.organization_id
FROM users AS u
LEFT JOIN organization_users AS ou ON ou.user_id = u.id
WHERE TRUE;
`
	q := compileSQLite(t, src)
	rs, err := ast.Renderings(sqlite.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := oracle.Describe(ctx, rs[0].SQL)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := (sqlite.Frontend{}).Parse(rs[0].SQL)
	if err != nil {
		t.Fatal(err)
	}
	got := nullability.Analyze(tree, rs[0], desc, cat, nil)
	// id/email NOT NULL; org_id/nickname nullable; ou.organization_id
	// NOT NULL in the catalog but null-extended by the LEFT JOIN.
	want := []bool{false, false, true, true, true}
	if len(got) != len(want) {
		t.Fatalf("columns = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("column %d (%s) nullable = %v, want %v", i, desc.Columns[i].Name, got[i], want[i])
		}
	}
}
