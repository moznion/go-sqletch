//go:build devdb

package e2e_test

import (
	"context"
	"os"
	"testing"
	"time"

	gomysqlclient "github.com/go-mysql-org/go-mysql/client"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/devdb"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/nullability"
	"github.com/moznion/go-sqletch/internal/rules"
	"github.com/moznion/go-sqletch/internal/shape"
	"github.com/moznion/go-sqletch/internal/template"
)

const mysqlSchemaSQL = `
CREATE TABLE users (
    id         BIGINT AUTO_INCREMENT PRIMARY KEY,
    email      VARCHAR(255) NOT NULL,
    status     VARCHAR(32) NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    tenant_id  BIGINT NOT NULL,
    org_id     BIGINT,
    nickname   VARCHAR(64),
    bio        TEXT
);
CREATE TABLE organization_users (
    user_id         BIGINT NOT NULL,
    organization_id BIGINT NOT NULL,
    PRIMARY KEY (user_id, organization_id)
);
CREATE TABLE audit_logs (
    id         BIGINT AUTO_INCREMENT PRIMARY KEY,
    tenant_id  BIGINT NOT NULL,
    actor_id   BIGINT,
    action     VARCHAR(64) NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
);
`

// mysqlCorpus mirrors the PostgreSQL corpus in MySQL dialect: every
// bind parameter carries its mandatory annotation, string concat uses
// concat(), and there is no RETURNING.
var mysqlCorpus = map[string]string{
	"search_users": `-- name: SearchUsers :many
-- @param organization_id: bigint
-- @param status: varchar(32)
-- @param email_prefix: varchar(255)
-- @param limit: bigint
SELECT u.id, u.email, u.status, u.created_at
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
  AND u.email LIKE concat(:email_prefix, '%')
@endif
@choose(sort)
@case(created_at_desc)
ORDER BY u.created_at DESC
@case(email_asc)
ORDER BY u.email ASC, u.id ASC
@default
ORDER BY u.id ASC
@end
LIMIT :limit;
`,
	"in_list": `-- name: UsersInStatuses :many
-- @param tenant_id: bigint
-- @param statuses: varchar(32)
-- @param min_id: bigint
-- @param limit: bigint
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
-- @param new_email: varchar(255)
-- @param nickname: varchar(64)
-- @param bio: text
-- @param id: bigint
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
-- @param email: varchar(255)
-- @param status: varchar(32)
-- @param tenant_id: bigint
-- @param nickname: varchar(64)
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
-- @param action: varchar(64)
-- @param min_actions: bigint
SELECT a.tenant_id, count(*) AS actions
FROM audit_logs AS a
WHERE TRUE
@when(include_cron = false)
  AND a.actor_id IS NOT NULL
@end
@if-present(action)
  AND a.action = :action
@endif
GROUP BY a.tenant_id
HAVING TRUE
@if-present(min_actions)
  AND count(*) >= :min_actions
@endif
ORDER BY a.tenant_id;
`,
	"order_by_users": `-- name: OrderedUsers :many
-- @param status: varchar(32)
-- @param limit: bigint
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
-- @param scope_tenant_id: bigint
-- @param scope_status: varchar(32)
-- @param scope_prefix: varchar(255)
-- @param limit: bigint
SELECT u.id, u.email
FROM users AS u
WHERE TRUE
  AND @filter-tree!(scope)
@predicate(tenant)
u.tenant_id = :scope_tenant_id
@predicate(status_eq)
u.status = :scope_status
@predicate(email_prefix)
u.email LIKE concat(:scope_prefix, '%')
@end
ORDER BY u.id
LIMIT :limit;
`,
	"filter_tree_having": `-- name: TenantVolumes :many
-- @param vol_min_users: bigint
-- @param vol_max_id: bigint
SELECT u.tenant_id, count(*) AS n
FROM users AS u
GROUP BY u.tenant_id
HAVING TRUE
  AND @filter-tree(vol)
@predicate(min_users)
count(*) >= :vol_min_users
@predicate(max_id_at_least)
max(u.id) >= :vol_max_id
@end
ORDER BY u.tenant_id;
`,
}

func acquireMySQL(t *testing.T) (*gomysqlclient.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	conn, cleanup, err := devdb.AcquireMySQL(ctx, devdb.Config{
		DSN:           os.Getenv("SQLETCH_TEST_MYSQL_DSN"),
		ServerVersion: "8.4",
		SchemaSQL:     []string{mysqlSchemaSQL},
	})
	if err != nil {
		t.Fatalf("acquire MySQL dev database: %v", err)
	}
	t.Cleanup(cleanup)
	return conn, ctx
}

// compileMySQL runs the offline front half under the MySQL dialect.
func compileMySQL(t *testing.T, src string) *template.QueryTemplate {
	t.Helper()
	f, diags := template.NewScanner(mysql.Profile{}).ScanFile("e2e.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	if d := rules.CheckLexical(mysql.Profile{}, q); len(d) != 0 {
		t.Fatalf("lexical: %+v", d)
	}
	rs, err := ast.Renderings(mysql.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	if d := rules.CheckR1(mysql.Profile{}, mysql.Frontend{}, q, rs); len(d) != 0 {
		t.Fatalf("R1: %+v", d)
	}
	return q
}

// TestMySQLPropertyAllShapesPrepareAndPlan: every enumerable shape —
// including both representative @in arities — must prepare
// (COM_STMT_PREPARE) and plan (executed EXPLAIN) on a live MySQL.
func TestMySQLPropertyAllShapesPrepareAndPlan(t *testing.T) {
	conn, ctx := acquireMySQL(t)
	oracle := mysql.NewOracle(conn)

	for name, src := range mysqlCorpus {
		t.Run(name, func(t *testing.T) {
			q := compileMySQL(t, src)
			keys, truncated := shape.EnumerateExpand(q, 4096, true)
			if truncated {
				t.Fatalf("corpus template exceeds the test cap")
			}
			for _, k := range keys {
				r, err := ast.RenderShape(mysql.Profile{}, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
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

// TestMySQLDescribe pins the oracle's column metadata: names, encoded
// type refs, and source-column identity resolved through the snapshot.
func TestMySQLDescribe(t *testing.T) {
	conn, ctx := acquireMySQL(t)
	oracle := mysql.NewOracle(conn)

	q := compileMySQL(t, mysqlCorpus["in_list"])
	rs, err := ast.Renderings(mysql.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := oracle.Describe(ctx, rs[0].SQL)
	if err != nil {
		t.Fatalf("describe maximal: %v", err)
	}
	// Parameter slots exist but stay untyped (annotation-filled).
	if len(desc.Params) != len(rs[0].ParamsSeq) {
		t.Fatalf("params = %d, want %d", len(desc.Params), len(rs[0].ParamsSeq))
	}
	for i, p := range desc.Params {
		if p.OID != 0 {
			t.Errorf("param %d typed by the protocol (%+v); expected untyped", i, p)
		}
	}
	if len(desc.Columns) != 3 {
		t.Fatalf("columns = %+v", desc.Columns)
	}
	tm := mysql.TypeMap{}
	for i, want := range []string{"int64", "string", "string"} {
		gt, ok := tm.GoType(desc.Columns[i].Type.OID)
		if !ok || gt.Name != want {
			t.Errorf("column %d (%s) type %s -> (%+v, %v), want %s",
				i, desc.Columns[i].Name, desc.Columns[i].Type.Name, gt, ok, want)
		}
	}
	// Source identity: direct references resolve to catalog positions.
	cat, err := oracle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	users := cat.Lookup("users")
	if users == nil {
		t.Fatal("snapshot must contain users")
	}
	if desc.Columns[0].SrcRel != users.OID || desc.Columns[0].SrcAtt != users.Col("id").Att {
		t.Errorf("column[0] source = (%d,%d), want users.id (%d,%d)",
			desc.Columns[0].SrcRel, desc.Columns[0].SrcAtt, users.OID, users.Col("id").Att)
	}
}

func TestMySQLSnapshotAndVersion(t *testing.T) {
	conn, ctx := acquireMySQL(t)
	oracle := mysql.NewOracle(conn)

	cat, err := oracle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	users := cat.Lookup("users")
	if users == nil {
		t.Fatal("snapshot must contain the users table")
	}
	if c := users.Col("email"); c == nil || !c.NotNull || c.TypeName != "varchar(255)" {
		t.Errorf("users.email = %+v", c)
	}
	if c := users.Col("org_id"); c == nil || c.NotNull {
		t.Errorf("users.org_id must be nullable: %+v", c)
	}
	if c := users.Col("created_at"); c == nil || !c.HasDefault {
		t.Errorf("users.created_at must have a default: %+v", c)
	}
	if c := users.Col("id"); c == nil || !c.HasDefault {
		t.Errorf("users.id (auto_increment) must count as defaulted: %+v", c)
	}

	v, err := oracle.ServerVersion(ctx)
	if err != nil || v == "" {
		t.Fatalf("server version: %q, %v", v, err)
	}
}

// TestMySQLNullability runs the analysis against real Describe output:
// catalog NOT NULL survives, nullable columns stay nullable, and a
// LEFT JOIN null-extends its right side.
func TestMySQLNullability(t *testing.T) {
	conn, ctx := acquireMySQL(t)
	oracle := mysql.NewOracle(conn)
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
	q := compileMySQL(t, src)
	rs, err := ast.Renderings(mysql.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := oracle.Describe(ctx, rs[0].SQL)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := (mysql.Frontend{}).Parse(rs[0].SQL)
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
