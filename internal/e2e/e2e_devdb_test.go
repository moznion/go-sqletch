//go:build devdb

// Package e2e_test runs the real-database soundness harness (design
// 04 §6): for every corpus template, every enumerable shape must both
// PREPARE (Describe) and PLAN (EXPLAIN GENERIC_PLAN) against a live
// PostgreSQL. Set SQLETCH_TEST_DSN to reuse a database; otherwise a
// disposable container is started.
package e2e_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/devdb"
	"github.com/moznion/sqletch/internal/dialect"
	"github.com/moznion/sqletch/internal/dialect/postgres"
	"github.com/moznion/sqletch/internal/nullability"
	"github.com/moznion/sqletch/internal/rules"
	"github.com/moznion/sqletch/internal/shape"
	"github.com/moznion/sqletch/internal/template"
)

const schemaSQL = `
CREATE TABLE users (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email       text NOT NULL,
    status      text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    tenant_id   bigint NOT NULL,
    org_id      bigint,
    nickname    text,
    bio         text
);
CREATE TABLE organization_users (
    user_id         bigint NOT NULL,
    organization_id bigint NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, organization_id)
);
CREATE TABLE audit_logs (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id  bigint NOT NULL,
    actor_id   bigint,
    action     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
`

// corpus: the design-doc use cases, exercised end to end.
var corpus = map[string]string{
	"search_users": `-- name: SearchUsers :many
SELECT
    u.id,
    u.email,
    u.status,
    u.created_at
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

@if-present(created_after)
  AND u.created_at >= :created_after
@endif

@choose(sort)
@case(created_at_desc)
ORDER BY u.created_at DESC
@case(created_at_asc)
ORDER BY u.created_at ASC
@case(email_asc)
ORDER BY u.email ASC, u.id ASC
@default
ORDER BY u.id ASC
@end

LIMIT :limit;
`,
	"list_audit_logs": `-- name: ListAuditLogs :many
SELECT a.id, a.actor_id, a.action, a.created_at
FROM audit_logs AS a
WHERE a.tenant_id = :tenant_id

@if-present(after_id)
  AND a.id < :after_id
@endif

ORDER BY a.id DESC
LIMIT :limit;
`,
	"update_user_profile": `-- name: UpdateUserProfile :one
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
WHERE id = :id
RETURNING id, email, nickname, bio;
`,
	"create_user": `-- name: CreateUser :one
INSERT INTO users (
    email
  , status
  , tenant_id
@if-present(nickname)
  , nickname
@endif
@if-present(bio)
  , bio
@endif
) VALUES (
    :email
  , :status
  , :tenant_id
@if-present(nickname)
  , :nickname
@endif
@if-present(bio)
  , :bio
@endif
)
RETURNING id, email, nickname, bio;
`,
	"signups_by_bucket": `-- name: SignupsByBucket :many
SELECT
@choose(bucket)
@case(daily)
date_trunc('day', u.created_at)
@case(weekly)
date_trunc('week', u.created_at)
@case(monthly)
date_trunc('month', u.created_at)
@end
 AS bucket,
    count(*) AS signups
FROM users AS u
WHERE u.created_at >= :since
GROUP BY 1
ORDER BY 1;
`,
	"when_and_having": `-- name: TenantActivity :many
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
	"exists_form": `-- name: SearchUsersExists :many
SELECT u.id, u.email
FROM users AS u
WHERE TRUE

@if-present(organization_id)
  AND EXISTS (
    SELECT 1 FROM organization_users AS ou
    WHERE ou.user_id = u.id AND ou.organization_id = :organization_id
  )
@endif

@if-present(status)
  AND u.status = :status
@endif

ORDER BY u.id
LIMIT :limit;
`,
}

func acquire(t *testing.T) (*pgx.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	conn, cleanup, err := devdb.Acquire(ctx, devdb.Config{
		DSN:           os.Getenv("SQLETCH_TEST_DSN"),
		ServerVersion: "16",
		SchemaSQL:     []string{schemaSQL},
	})
	if err != nil {
		t.Fatalf("acquire dev database: %v", err)
	}
	t.Cleanup(cleanup)
	return conn, ctx
}

// TestPropertyAllShapesPrepareAndPlan is the mechanical backing of the
// compositional-verification claim: enumerate every reachable shape
// and let PostgreSQL prepare AND plan each one.
func TestPropertyAllShapesPrepareAndPlan(t *testing.T) {
	conn, ctx := acquire(t)
	oracle := postgres.NewOracle(conn)

	for name, src := range corpus {
		t.Run(name, func(t *testing.T) {
			q := compile(t, src)
			keys, truncated := shape.Enumerate(q, 4096)
			if truncated {
				t.Fatalf("corpus template exceeds the test cap")
			}
			for _, k := range keys {
				r, err := ast.RenderShape(postgres.Profile{}, q, k.Guards, k.Selection(), k.OrderSelection())
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

// TestDescribeTypes pins the type-extraction behavior the design doc
// documents (e.g. a LIMIT parameter describes as int8).
func TestDescribeTypes(t *testing.T) {
	conn, ctx := acquire(t)
	oracle := postgres.NewOracle(conn)

	q := compile(t, corpus["search_users"])
	rs, err := ast.Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}

	desc, err := oracle.Describe(ctx, rs[0].SQL)
	if err != nil {
		t.Fatalf("describe maximal: %v", err)
	}
	// ParamsSeq: organization_id, status, email_prefix, created_after, limit
	wantOIDs := map[string]uint32{
		"organization_id": 20,   // int8
		"status":          25,   // text
		"email_prefix":    25,   // text
		"created_after":   1184, // timestamptz
		"limit":           20,   // int8 (the doc's example)
	}
	if len(desc.Params) != len(rs[0].ParamsSeq) {
		t.Fatalf("params = %d, want %d", len(desc.Params), len(rs[0].ParamsSeq))
	}
	for i, name := range rs[0].ParamsSeq {
		if desc.Params[i].OID != wantOIDs[name] {
			t.Errorf("param %s OID = %d (%s), want %d", name, desc.Params[i].OID, desc.Params[i].Name, wantOIDs[name])
		}
	}

	// Result columns carry their source-relation identity for the
	// nullability analysis.
	if len(desc.Columns) != 4 {
		t.Fatalf("columns = %+v", desc.Columns)
	}
	if desc.Columns[0].Name != "id" || desc.Columns[0].SrcRel == 0 || desc.Columns[0].SrcAtt != 1 {
		t.Errorf("column[0] = %+v, want direct ref to users.id", desc.Columns[0])
	}
}

func TestSnapshotAndVersion(t *testing.T) {
	conn, ctx := acquire(t)
	oracle := postgres.NewOracle(conn)

	cat, err := oracle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	users := cat.Lookup("users")
	if users == nil {
		t.Fatal("snapshot must contain the users table")
	}
	if c := users.Col("email"); c == nil || !c.NotNull || c.TypeName != "text" {
		t.Errorf("users.email = %+v", c)
	}
	if c := users.Col("org_id"); c == nil || c.NotNull {
		t.Errorf("users.org_id must be nullable: %+v", c)
	}
	if c := users.Col("created_at"); c == nil || !c.HasDefault {
		t.Errorf("users.created_at must have a default: %+v", c)
	}

	v, err := oracle.ServerVersion(ctx)
	if err != nil || v == "" {
		t.Fatalf("server version: %q, %v", v, err)
	}
}

func TestIndeterminateParamError(t *testing.T) {
	conn, ctx := acquire(t)
	oracle := postgres.NewOracle(conn)

	// Note: bare `SELECT $1` is NOT an error — modern PostgreSQL
	// resolves a fully unconstrained parameter to text. Two unknowns
	// meeting an operator cannot be resolved and need a cast.
	_, err := oracle.Describe(ctx, "SELECT $1 + $2")
	if err == nil {
		t.Fatal("expected indeterminate-type error")
	}
	var oe *dialect.OracleError
	if !errors.As(err, &oe) {
		t.Fatalf("error type = %T (%v)", err, err)
	}
	if !oe.Indeterminate {
		t.Errorf("Indeterminate = false for %+v; the CLI relies on this flag for the cast hint (SQLETCH201)", oe)
	}
}

// TestNullabilityAgainstRealCatalog runs the analysis with a real
// snapshot and real Describe output (source-column identity comes from
// the wire protocol, not fixtures).
func TestNullabilityAgainstRealCatalog(t *testing.T) {
	conn, ctx := acquire(t)
	oracle := postgres.NewOracle(conn)
	cat, err := oracle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	src := `-- name: N :many
SELECT u.id, u.email, u.org_id, u.nickname, count(*) OVER () AS total
FROM users AS u
@if-present(organization_id)
JOIN organization_users AS ou
  ON ou.user_id = u.id AND ou.organization_id = :organization_id
@endif
WHERE TRUE;
`
	q := compile(t, src)
	rs, err := ast.Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := oracle.Describe(ctx, rs[0].SQL)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := postgres.Frontend{}.Parse(rs[0].SQL)
	if err != nil {
		t.Fatal(err)
	}
	got := nullability.Analyze(tree, rs[0], desc, cat, nil)
	// id/email NOT NULL; org_id and nickname nullable; count(*) is
	// total even as a window function (returns 0, never NULL).
	want := []bool{false, false, true, true, false}
	if len(got) != len(want) {
		t.Fatalf("columns = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("column %d (%s) nullable = %v, want %v", i, desc.Columns[i].Name, got[i], want[i])
		}
	}
}

// compile runs the full front half of the pipeline (scan, lexical
// rules, renderings, R1) and fails the test on any diagnostic.
func compile(t *testing.T, src string) *template.QueryTemplate {
	t.Helper()
	f, diags := template.NewScanner(postgres.Profile{}).ScanFile("e2e.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	if d := rules.CheckLexical(postgres.Profile{}, q); len(d) != 0 {
		t.Fatalf("lexical: %+v", d)
	}
	rs, err := ast.Renderings(postgres.Profile{}, q)
	if err != nil {
		t.Fatal(err)
	}
	if d := rules.CheckR1(postgres.Profile{}, postgres.Frontend{}, q, rs); len(d) != 0 {
		t.Fatalf("R1: %+v", d)
	}
	return q
}
