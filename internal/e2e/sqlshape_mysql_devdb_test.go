//go:build devdb

package e2e_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/devdb"
)

// TestSQLShapesMySQL pins the exact SQL + binds the generated
// database/sql code sends on MySQL: question placeholders repeat per
// occurrence, @in expands to one '?' per element (arity is a shape
// dimension), and the mandatory `-- @param` annotations stay in the
// skeleton verbatim.
func TestSQLShapesMySQL(t *testing.T) {
	runShapeSuite(t, shapeSuite{
		dialect:  mysqlShapeDialect,
		schema:   mysqlShapeSchema,
		policies: tenantPolicyYAML,
		cases:    mysqlShapeCases,
	})
}

var mysqlShapeDialect = shapeDialect{
	name:          "mysql",
	serverVersion: "8.4",
	acquire: func(t *testing.T, ctx context.Context, _ string) (string, func()) {
		dsn, cleanup, err := devdb.AcquireMySQLDSN(ctx, devdb.Config{
			DSN:              os.Getenv("SQLETCH_TEST_MYSQL_DSN"),
			AllowDestructive: true,
			ServerVersion:    "8.4",
		})
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		return dsn, cleanup
	},
	imports:  []string{`"database/sql"`, `"database/sql/driver"`},
	recorder: sqlRecorder,
}

// annot renders `-- ` annotation lines exactly as they survive into the
// skeleton: the header's own line break first, then one line each.
func annot(lines ...string) string {
	var b strings.Builder
	b.WriteString("\n")
	for _, l := range lines {
		b.WriteString("-- " + l + "\n")
	}
	return b.String()
}

const mysqlShapeSchema = `
CREATE TABLE users (
    id         BIGINT AUTO_INCREMENT PRIMARY KEY,
    tenant_id  BIGINT NOT NULL,
    email      VARCHAR(255) NOT NULL,
    status     VARCHAR(32) NOT NULL,
    nickname   VARCHAR(64),
    bio        TEXT,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
);
CREATE TABLE organization_users (
    user_id         BIGINT NOT NULL,
    organization_id BIGINT NOT NULL,
    PRIMARY KEY (user_id, organization_id)
);
CREATE TABLE notes (
    id      BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT NOT NULL,
    body    VARCHAR(64) DEFAULT 'n/a',
    tag     VARCHAR(64)
);
CREATE TABLE audit_logs (
    id        BIGINT AUTO_INCREMENT PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    actor_id  BIGINT,
    action    VARCHAR(64) NOT NULL
);
`

var (
	myGuardedWhereHdr = annot("@param tenant_id: bigint", "@param status: varchar(32)",
		"@param email_prefix: varchar(255)", "@param min_id: bigint", "@param limit: bigint")
	myJoinHdr   = annot("@param organization_id: bigint", "@param tenant_id: bigint")
	mySortHdr   = annot("@param tenant_id: bigint", "@param limit: bigint")
	myBucketHdr = annot("@param since: timestamp", "@column bucket: varchar(16)")
	myFeedHdr   = annot("@param tenant_id: bigint")
	myInHdr     = annot("@param tenant_id: bigint", "@param statuses: varchar(32)", "@param min_id: bigint")
	myInTwoHdr  = annot("@param statuses: varchar(32)", "@param ids: bigint")
	myHandleHdr = annot("@param handle: varchar(255)")
	myPatchHdr  = annot("@param nickname: varchar(64)", "@param bio: text", "@param id: bigint")
	myNoteHdr   = annot("@param user_id: bigint", "@param tag: varchar(64)", "@param body: varchar(64)")
	myLexHdr    = annot("@param email: varchar(255)")
	myFilterHdr = annot("@param tenant_id: bigint", "@param scope_status: varchar(32)",
		"@param scope_prefix: varchar(255)", "@param scope_min_id: bigint", "@param limit: bigint")
	myVolHdr    = annot("@param vol_min_users: bigint")
	myAuditHdr  = annot("@param actor_id: bigint", "@param limit: bigint")
	myDeleteHdr = annot("@param user_id: bigint", "@param tag: varchar(64)")
	myTouchHdr  = annot("@param id: bigint")
	myFindHdr   = annot("@param email: varchar(255)")
)

var mysqlShapeCases = []shapeCase{
	{
		name: "guarded_where",
		query: "-- name: GuardedWhere :many" + myGuardedWhereHdr + `SELECT u.id, u.email
FROM users AS u
WHERE u.tenant_id = :tenant_id
@if-present(status)
  AND u.status = :status
@endif
@if-present(email_prefix)
  AND u.email LIKE concat(:email_prefix, '%')
@endif
@if-present(min_id)
  AND u.id >= :min_id
@endif
ORDER BY u.id
LIMIT :limit;
`,
		calls: []shapeCall{
			{
				name:     "no_guards",
				call:     `q.GuardedWhere(ctx, gen.GuardedWhereParams{TenantID: 7, Limit: 10})`,
				wantSQL:  myGuardedWhereHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n\n\n\nORDER BY u.id\nLIMIT ?;\n",
				wantArgs: []string{"int64(7)", "int64(10)"},
			},
			{
				name: "all_guards",
				call: `q.GuardedWhere(ctx, gen.GuardedWhereParams{TenantID: 7, Status: sqletch.Present("active"), EmailPrefix: sqletch.Present("a"), MinID: sqletch.Present(int64(3)), Limit: 10})`,
				wantSQL: myGuardedWhereHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n" +
					"\nAND (u.status = ?)\n" +
					"\nAND (u.email LIKE concat(?, '%'))\n" +
					"\nAND (u.id >= ?)\nORDER BY u.id\nLIMIT ?;\n",
				wantArgs: []string{"int64(7)", `string("active")`, `string("a")`, "int64(3)", "int64(10)"},
			},
			{
				name:     "middle_guard_only",
				call:     `q.GuardedWhere(ctx, gen.GuardedWhereParams{TenantID: 7, EmailPrefix: sqletch.Present("日本"), Limit: 10})`,
				wantSQL:  myGuardedWhereHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n\n\nAND (u.email LIKE concat(?, '%'))\n\nORDER BY u.id\nLIMIT ?;\n",
				wantArgs: []string{"int64(7)", `string("日本")`, "int64(10)"},
			},
		},
	},
	{
		name: "guarded_join",
		query: "-- name: UsersInOrg :many" + myJoinHdr + `SELECT u.id, u.email
FROM users AS u
@if-present(organization_id)
JOIN organization_users AS ou
  ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif
WHERE u.tenant_id = :tenant_id
ORDER BY u.id;
`,
		calls: []shapeCall{
			{
				name:     "join_off",
				call:     `q.UsersInOrg(ctx, gen.UsersInOrgParams{TenantID: 7})`,
				wantSQL:  myJoinHdr + "SELECT u.id, u.email\nFROM users AS u\n\nWHERE u.tenant_id = ?\nORDER BY u.id;\n",
				wantArgs: []string{"int64(7)"},
			},
			{
				name: "join_on_binds_before_where",
				call: `q.UsersInOrg(ctx, gen.UsersInOrgParams{TenantID: 7, OrganizationID: sqletch.Present(int64(77))})`,
				wantSQL: myJoinHdr + "SELECT u.id, u.email\nFROM users AS u\n" +
					"\nJOIN organization_users AS ou\n  ON ou.user_id = u.id\n AND ou.organization_id = ?" +
					"\nWHERE u.tenant_id = ?\nORDER BY u.id;\n",
				wantArgs: []string{"int64(77)", "int64(7)"},
			},
		},
	},
	{
		name: "choose_sort",
		query: "-- name: SortedUsers :many" + mySortHdr + `SELECT u.id, u.email
FROM users AS u
WHERE u.tenant_id = :tenant_id
@choose(sort)
@case(newest)
ORDER BY u.created_at DESC, u.id DESC
@case(email)
ORDER BY u.email ASC
@default
ORDER BY u.id ASC
@end
LIMIT :limit;
`,
		calls: []shapeCall{
			{
				name:     "zero_selects_default",
				call:     `q.SortedUsers(ctx, gen.SortedUsersParams{TenantID: 1, Limit: 5})`,
				wantSQL:  mySortHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n\nORDER BY u.id ASC\nLIMIT ?;\n",
				wantArgs: []string{"int64(1)", "int64(5)"},
			},
			{
				name:     "first_case",
				call:     `q.SortedUsers(ctx, gen.SortedUsersParams{TenantID: 1, Sort: gen.SortedUsersSortNewest, Limit: 5})`,
				wantSQL:  mySortHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n\nORDER BY u.created_at DESC, u.id DESC\nLIMIT ?;\n",
				wantArgs: []string{"int64(1)", "int64(5)"},
			},
			{
				name:    "out_of_range_rejected",
				call:    `q.SortedUsers(ctx, gen.SortedUsersParams{Sort: gen.SortedUsersSort(9)})`,
				wantErr: "out of range",
			},
		},
	},
	{
		name: "choose_required_projection",
		query: "-- name: Bucketed :many" + myBucketHdr + `SELECT
@choose(bucket)
@case(day)
DATE_FORMAT(u.created_at, '%Y-%m-%d')
@case(month)
DATE_FORMAT(u.created_at, '%Y-%m')
@end
 AS bucket, u.id
FROM users AS u
WHERE u.created_at >= :since
ORDER BY u.id;
`,
		calls: []shapeCall{
			{
				name: "month",
				call: `q.Bucketed(ctx, gen.BucketedParams{Bucket: gen.BucketedBucketMonth, Since: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)})`,
				wantSQL: myBucketHdr + "SELECT\n\nDATE_FORMAT(u.created_at, '%Y-%m')\n AS bucket, u.id\n" +
					"FROM users AS u\nWHERE u.created_at >= ?\nORDER BY u.id;\n",
				wantArgs: []string{"time.Time(2024-01-02T03:04:05Z)"},
			},
			{
				name:    "zero_rejected",
				call:    `q.Bucketed(ctx, gen.BucketedParams{})`,
				wantErr: "required @choose",
			},
		},
	},
	{
		name: "order_by",
		query: "-- name: OrderedUsers :many" + mySortHdr + `SELECT u.id, u.email
FROM users AS u
WHERE u.tenant_id = :tenant_id
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
		calls: []shapeCall{
			{
				name:     "empty_uses_default",
				call:     `q.OrderedUsers(ctx, gen.OrderedUsersParams{TenantID: 1, Limit: 5})`,
				wantSQL:  mySortHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n\nORDER BY u.id ASC\nLIMIT ?;\n",
				wantArgs: []string{"int64(1)", "int64(5)"},
			},
			{
				name:     "two_keys_caller_order_with_desc",
				call:     `q.OrderedUsers(ctx, gen.OrderedUsersParams{TenantID: 1, Sort: []gen.OrderedUsersSortKey{gen.OrderedUsersSortCreatedAtDesc, gen.OrderedUsersSortEmailAsc}, Limit: 5})`,
				wantSQL:  mySortHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n\nORDER BY u.created_at DESC, u.email\nLIMIT ?;\n",
				wantArgs: []string{"int64(1)", "int64(5)"},
			},
			{
				name:    "duplicate_key_rejected",
				call:    `q.OrderedUsers(ctx, gen.OrderedUsersParams{Sort: []gen.OrderedUsersSortKey{gen.OrderedUsersSortCreatedAtAsc, gen.OrderedUsersSortCreatedAtDesc}})`,
				wantErr: "invalid @order-by key selection",
			},
		},
	},
	{
		name: "when_literals",
		query: "-- name: UserFeed :many" + myFeedHdr + `SELECT u.id, u.email
FROM users AS u
WHERE u.tenant_id = :tenant_id
@when(include_banned = false)
  AND u.status <> 'banned'
@end
@when(mode = 'strict')
  AND u.nickname IS NOT NULL
@end
ORDER BY u.id;
`,
		calls: []shapeCall{
			{
				name:     "zero_values",
				call:     `q.UserFeed(ctx, gen.UserFeedParams{TenantID: 1})`,
				wantSQL:  myFeedHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n\nAND (u.status <> 'banned')\n\nORDER BY u.id;\n",
				wantArgs: []string{"int64(1)"},
			},
			{
				name:     "matching_literals",
				call:     `q.UserFeed(ctx, gen.UserFeedParams{TenantID: 1, IncludeBanned: true, Mode: "strict"})`,
				wantSQL:  myFeedHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n\n\nAND (u.nickname IS NOT NULL)\nORDER BY u.id;\n",
				wantArgs: []string{"int64(1)"},
			},
		},
	},
	{
		name: "in_list",
		query: "-- name: UsersInStatuses :many" + myInHdr + `SELECT u.id
FROM users AS u
WHERE u.tenant_id = :tenant_id
  AND u.status @in(:statuses)
@if-present(min_id)
  AND u.id >= :min_id
@endif
ORDER BY u.id;
`,
		calls: []shapeCall{
			{
				name:     "three_elements_expand",
				call:     `q.UsersInStatuses(ctx, gen.UsersInStatusesParams{TenantID: 1, Statuses: []string{"a", "b", "c"}})`,
				wantSQL:  myInHdr + "SELECT u.id\nFROM users AS u\nWHERE u.tenant_id = ?\n  AND u.status IN (?, ?, ?)\n\nORDER BY u.id;\n",
				wantArgs: []string{"int64(1)", `string("a")`, `string("b")`, `string("c")`},
			},
			{
				name:     "empty_matches_nothing",
				call:     `q.UsersInStatuses(ctx, gen.UsersInStatusesParams{TenantID: 1, Statuses: []string{}})`,
				wantSQL:  myInHdr + "SELECT u.id\nFROM users AS u\nWHERE u.tenant_id = ?\n  AND u.status IN (SELECT NULL FROM DUAL WHERE FALSE)\n\nORDER BY u.id;\n",
				wantArgs: []string{"int64(1)"},
			},
			{
				name:     "nil_is_empty",
				call:     `q.UsersInStatuses(ctx, gen.UsersInStatusesParams{TenantID: 1})`,
				wantSQL:  myInHdr + "SELECT u.id\nFROM users AS u\nWHERE u.tenant_id = ?\n  AND u.status IN (SELECT NULL FROM DUAL WHERE FALSE)\n\nORDER BY u.id;\n",
				wantArgs: []string{"int64(1)"},
			},
			{
				name:     "one_element_then_guard",
				call:     `q.UsersInStatuses(ctx, gen.UsersInStatusesParams{TenantID: 1, Statuses: []string{"x"}, MinID: sqletch.Present(int64(9))})`,
				wantSQL:  myInHdr + "SELECT u.id\nFROM users AS u\nWHERE u.tenant_id = ?\n  AND u.status IN (?)\n\nAND (u.id >= ?)\nORDER BY u.id;\n",
				wantArgs: []string{"int64(1)", `string("x")`, "int64(9)"},
			},
		},
	},
	{
		name: "in_list_two_blocks",
		query: "-- name: InTwo :many" + myInTwoHdr + `SELECT u.id
FROM users AS u
WHERE u.status @in(:statuses)
  AND u.id @in(:ids)
ORDER BY u.id;
`,
		calls: []shapeCall{
			{
				name:     "arities_are_independent",
				call:     `q.InTwo(ctx, gen.InTwoParams{Statuses: []string{"a", "b"}, Ids: []int64{}})`,
				wantSQL:  myInTwoHdr + "SELECT u.id\nFROM users AS u\nWHERE u.status IN (?, ?)\n  AND u.id IN (SELECT NULL FROM DUAL WHERE FALSE)\nORDER BY u.id;\n",
				wantArgs: []string{`string("a")`, `string("b")`},
			},
			{
				name:     "elements_bind_in_text_order",
				call:     `q.InTwo(ctx, gen.InTwoParams{Statuses: []string{"a"}, Ids: []int64{4, 5, 6}})`,
				wantSQL:  myInTwoHdr + "SELECT u.id\nFROM users AS u\nWHERE u.status IN (?)\n  AND u.id IN (?, ?, ?)\nORDER BY u.id;\n",
				wantArgs: []string{`string("a")`, "int64(4)", "int64(5)", "int64(6)"},
			},
		},
	},
	{
		name: "repeated_param",
		query: "-- name: FindByHandle :many" + myHandleHdr + `SELECT u.id
FROM users AS u
WHERE u.email = :handle OR u.nickname = :handle
ORDER BY u.id;
`,
		calls: []shapeCall{
			{
				name:     "bind_repeated_per_occurrence",
				call:     `q.FindByHandle(ctx, gen.FindByHandleParams{Handle: "neo"})`,
				wantSQL:  myHandleHdr + "SELECT u.id\nFROM users AS u\nWHERE u.email = ? OR u.nickname = ?\nORDER BY u.id;\n",
				wantArgs: []string{`string("neo")`, `string("neo")`},
			},
		},
	},
	{
		name: "patch_update",
		query: "-- name: PatchUser :execrows" + myPatchHdr + `UPDATE users
SET
    email = email
@if-present(nickname)
  , nickname = :nickname
@endif
@if-present(bio)
  , bio = :bio
@endif
WHERE id = :id;
`,
		calls: []shapeCall{
			{
				name:     "nothing_set",
				call:     `q.PatchUser(ctx, gen.PatchUserParams{ID: 1})`,
				wantSQL:  myPatchHdr + "UPDATE users\nSET\n    email = email\n\n\nWHERE id = ?;\n",
				wantArgs: []string{"int64(1)"},
			},
			{
				name: "set_value_and_clear_to_null",
				call: `q.PatchUser(ctx, gen.PatchUserParams{ID: 1, Nickname: sqletch.Present(optional.Some("ニック")), Bio: sqletch.Present(optional.None[string]())})`,
				wantSQL: myPatchHdr + "UPDATE users\nSET\n    email = email\n" +
					"\n, nickname = ?\n\n, bio = ?\nWHERE id = ?;\n",
				wantArgs: []string{`string("ニック")`, "NULL", "int64(1)"},
			},
		},
	},
	{
		name: "insert_optional_pair",
		query: "-- name: CreateNote :execrows" + myNoteHdr + `INSERT INTO notes (
    user_id
  , tag
@if-present(body)
  , body
@endif
) VALUES (
    :user_id
  , :tag
@if-present(body)
  , :body
@endif
);
`,
		calls: []shapeCall{
			{
				name:     "omitted_pair_none_tag",
				call:     `q.CreateNote(ctx, gen.CreateNoteParams{UserID: 5})`,
				wantSQL:  myNoteHdr + "INSERT INTO notes (\n    user_id\n  , tag\n\n) VALUES (\n    ?\n  , ?\n\n);\n",
				wantArgs: []string{"int64(5)", "NULL"},
			},
			{
				name:     "pair_present_both_sides",
				call:     `q.CreateNote(ctx, gen.CreateNoteParams{UserID: 5, Tag: optional.Some("t"), Body: sqletch.Present(optional.Some("b"))})`,
				wantSQL:  myNoteHdr + "INSERT INTO notes (\n    user_id\n  , tag\n\n, body\n) VALUES (\n    ?\n  , ?\n\n, ?\n);\n",
				wantArgs: []string{"int64(5)", `string("t")`, `string("b")`},
			},
		},
	},
	{
		name: "lexical_edges",
		query: "-- name: LexicalEdges :many" + myLexHdr + `SELECT u.id, u.` + "`status`" + `, 'it''s :not_param' AS lit, 'a\'b :x' AS esc, 'こんにちは' AS greeting
FROM users AS u
-- a comment mentioning :not_a_param
WHERE u.email = :email
  AND u.created_at > NOW() - INTERVAL 1 DAY
ORDER BY u.id;
`,
		calls: []shapeCall{
			{
				name: "only_real_params_bind",
				call: `q.LexicalEdges(ctx, gen.LexicalEdgesParams{Email: "e@example.com"})`,
				wantSQL: myLexHdr + "SELECT u.id, u.`status`, 'it''s :not_param' AS lit, 'a\\'b :x' AS esc, 'こんにちは' AS greeting\n" +
					"FROM users AS u\n-- a comment mentioning :not_a_param\nWHERE u.email = ?\n" +
					"  AND u.created_at > NOW() - INTERVAL 1 DAY\nORDER BY u.id;\n",
				wantArgs: []string{`string("e@example.com")`},
			},
		},
	},
	{
		name: "filter_tree_required",
		query: "-- name: FilterUsers :many" + myFilterHdr + `SELECT u.id
FROM users AS u
WHERE u.tenant_id = :tenant_id
  AND @filter-tree!(scope)
@predicate(status_eq)
u.status = :scope_status
@predicate(email_prefix)
u.email LIKE concat(:scope_prefix, '%')
@predicate(min_id)
u.id >= :scope_min_id
@end
ORDER BY u.id
LIMIT :limit;
`,
		calls: []shapeCall{
			{
				name:     "unscoped",
				call:     `q.FilterUsers(ctx, gen.FilterUsersUnscoped(), gen.FilterUsersParams{TenantID: 1, Limit: 10})`,
				wantSQL:  myFilterHdr + "SELECT u.id\nFROM users AS u\nWHERE u.tenant_id = ?\n  AND TRUE\nORDER BY u.id\nLIMIT ?;\n",
				wantArgs: []string{"int64(1)", "int64(10)"},
			},
			{
				name: "nested_and_or",
				call: `q.FilterUsers(ctx, gen.And(gen.FilterUsersStatusEq("active"), gen.Or(gen.FilterUsersEmailPrefix("x"), gen.FilterUsersMinID(5))), gen.FilterUsersParams{TenantID: 1, Limit: 10})`,
				wantSQL: myFilterHdr + "SELECT u.id\nFROM users AS u\nWHERE u.tenant_id = ?\n" +
					"  AND ((u.status = ?) AND ((u.email LIKE concat(?, '%')) OR (u.id >= ?)))\nORDER BY u.id\nLIMIT ?;\n",
				wantArgs: []string{"int64(1)", `string("active")`, `string("x")`, "int64(5)", "int64(10)"},
			},
			{
				name:    "zero_tree_rejected",
				call:    `q.FilterUsers(ctx, sqletchruntime.Tree{}, gen.FilterUsersParams{TenantID: 1, Limit: 10})`,
				wantErr: "required @filter-tree",
			},
		},
	},
	{
		name: "filter_tree_optional_having",
		query: "-- name: TenantVolumes :many" + myVolHdr + `SELECT u.tenant_id, count(*) AS n
FROM users AS u
GROUP BY u.tenant_id
HAVING TRUE
  AND @filter-tree(vol)
@predicate(min_users)
count(*) >= :vol_min_users
@end
ORDER BY u.tenant_id;
`,
		calls: []shapeCall{
			{
				name:     "zero_tree_is_true",
				call:     `q.TenantVolumes(ctx, gen.TenantVolumesParams{})`,
				wantSQL:  myVolHdr + "SELECT u.tenant_id, count(*) AS n\nFROM users AS u\nGROUP BY u.tenant_id\nHAVING TRUE\n  AND TRUE\nORDER BY u.tenant_id;\n",
				wantArgs: []string{},
			},
			{
				name:     "or_of_leaves",
				call:     `q.TenantVolumes(ctx, gen.TenantVolumesParams{Vol: gen.Or(gen.TenantVolumesMinUsers(3), gen.TenantVolumesMinUsers(30))})`,
				wantSQL:  myVolHdr + "SELECT u.tenant_id, count(*) AS n\nFROM users AS u\nGROUP BY u.tenant_id\nHAVING TRUE\n  AND ((count(*) >= ?) OR (count(*) >= ?))\nORDER BY u.tenant_id;\n",
				wantArgs: []string{"int64(3)", "int64(30)"},
			},
		},
	},
	{
		name: "policy_where",
		query: "-- name: RecentAudit :many" + myAuditHdr + `SELECT a.id, a.action
FROM audit_logs AS a
WHERE a.action <> 'noop'
@if-present(actor_id)
  AND a.actor_id = :actor_id
@endif
ORDER BY a.id DESC
LIMIT :limit;
`,
		calls: []shapeCall{
			{
				name: "with_guard",
				call: `q.RecentAudit(ctx, gen.TenantID(3), gen.RecentAuditParams{ActorID: sqletch.Present(int64(8)), Limit: 20})`,
				wantSQL: myAuditHdr + "SELECT a.id, a.action\nFROM audit_logs AS a\nWHERE (a.tenant_id = ?) AND a.action <> 'noop'\n" +
					"\nAND (a.actor_id = ?)\nORDER BY a.id DESC\nLIMIT ?;\n",
				wantArgs: []string{"int64(3)", "int64(8)", "int64(20)"},
			},
		},
	},
	{
		name: "policy_or_wrapped",
		query: `-- name: LoginEvents :many
SELECT a.id
FROM audit_logs AS a
WHERE a.action = 'login' OR a.action = 'logout'
ORDER BY a.id;
`,
		calls: []shapeCall{
			{
				name:     "or_parenthesized",
				call:     `q.LoginEvents(ctx, gen.TenantID(3), gen.LoginEventsParams{})`,
				wantSQL:  "\nSELECT a.id\nFROM audit_logs AS a\nWHERE (a.tenant_id = ?) AND (a.action = 'login' OR a.action = 'logout')\nORDER BY a.id;\n",
				wantArgs: []string{"int64(3)"},
			},
		},
	},
	{
		name: "policy_no_where",
		query: `-- name: AllAudit :many
SELECT a.id FROM audit_logs AS a ORDER BY a.id;
`,
		calls: []shapeCall{
			{
				name:     "where_inserted_before_tail",
				call:     `q.AllAudit(ctx, gen.TenantID(3), gen.AllAuditParams{})`,
				wantSQL:  "\nSELECT a.id FROM audit_logs AS a WHERE (a.tenant_id = ?) ORDER BY a.id;\n",
				wantArgs: []string{"int64(3)"},
			},
		},
	},
	{
		name: "exec_kinds",
		query: "-- name: DeleteNotes :execrows" + myDeleteHdr + `DELETE FROM notes
WHERE user_id = :user_id
@if-present(tag)
  AND tag = :tag
@endif
;
`,
		calls: []shapeCall{
			{
				name:     "execrows_guard_on",
				call:     `q.DeleteNotes(ctx, gen.DeleteNotesParams{UserID: 2, Tag: sqletch.Present("old")})`,
				wantSQL:  myDeleteHdr + "DELETE FROM notes\nWHERE user_id = ?\n\nAND (tag = ?)\n;\n",
				wantArgs: []string{"int64(2)", `string("old")`},
			},
		},
	},
	{
		name: "exec_plain",
		query: "-- name: TouchUser :exec" + myTouchHdr + `UPDATE users SET bio = bio WHERE id = :id;
`,
		calls: []shapeCall{
			{
				name:     "exec",
				call:     `q.TouchUser(ctx, gen.TouchUserParams{ID: 4})`,
				wantSQL:  myTouchHdr + "UPDATE users SET bio = bio WHERE id = ?;\n",
				wantArgs: []string{"int64(4)"},
			},
		},
	},
	{
		name: "maybe_one",
		query: "-- name: FindUser :maybe-one" + myFindHdr + `SELECT u.id, u.nickname FROM users AS u WHERE u.email = :email;
`,
		calls: []shapeCall{
			{
				name:     "maybe_one",
				call:     `q.FindUser(ctx, gen.FindUserParams{Email: "a@b"})`,
				wantSQL:  myFindHdr + "SELECT u.id, u.nickname FROM users AS u WHERE u.email = ?;\n",
				wantArgs: []string{`string("a@b")`},
			},
		},
	},
}
