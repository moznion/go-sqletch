//go:build devdb

package e2e_test

import (
	"context"
	"path/filepath"
	"testing"
)

// TestSQLShapesSQLite pins the exact SQL + binds the generated
// database/sql code sends on SQLite (in-process engine, no Docker):
// question placeholders, per-element @in expansion with the SQLite
// arity-0 emission, and `-- @param` / `-- @column` annotations kept in
// the skeleton.
func TestSQLShapesSQLite(t *testing.T) {
	runShapeSuite(t, shapeSuite{
		dialect:  sqliteShapeDialect,
		schema:   sqliteShapeSchema,
		policies: tenantPolicyYAML,
		cases:    sqliteShapeCases,
	})
}

var sqliteShapeDialect = shapeDialect{
	name:          "sqlite",
	serverVersion: "3",
	acquire: func(_ *testing.T, _ context.Context, dir string) (string, func()) {
		return filepath.Join(dir, "dev.sqlite3"), func() {}
	},
	imports:  []string{`"database/sql"`, `"database/sql/driver"`},
	recorder: sqlRecorder,
}

const sqliteShapeSchema = `
CREATE TABLE users (
    id         INTEGER PRIMARY KEY,
    tenant_id  INTEGER NOT NULL,
    email      TEXT NOT NULL,
    status     TEXT NOT NULL,
    nickname   TEXT,
    bio        TEXT,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE organization_users (
    user_id         INTEGER NOT NULL,
    organization_id INTEGER NOT NULL,
    PRIMARY KEY (user_id, organization_id)
);
CREATE TABLE notes (
    id      INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL,
    body    TEXT DEFAULT 'n/a',
    tag     TEXT
);
CREATE TABLE audit_logs (
    id        INTEGER PRIMARY KEY,
    tenant_id INTEGER NOT NULL,
    actor_id  INTEGER,
    event     TEXT NOT NULL
);
`

var (
	slGuardedWhereHdr = annot("@param tenant_id: integer", "@param status: text",
		"@param email_prefix: text", "@param min_id: integer", "@param limit: integer")
	slJoinHdr   = annot("@param organization_id: integer", "@param tenant_id: integer")
	slSortHdr   = annot("@param tenant_id: integer", "@param limit: integer")
	slBucketHdr = annot("@param since: datetime", "@column bucket: text")
	slFeedHdr   = annot("@param tenant_id: integer")
	slInHdr     = annot("@param tenant_id: integer", "@param statuses: text", "@param min_id: integer")
	slHandleHdr = annot("@param handle: text")
	slPatchHdr  = annot("@param nickname: text", "@param bio: text", "@param id: integer")
	slNoteHdr   = annot("@param user_id: integer", "@param tag: text", "@param body: text")
	slLexHdr    = annot("@param email: text", "@column lit: text", "@column greeting: text")
	slFilterHdr = annot("@param tenant_id: integer", "@param scope_status: text",
		"@param scope_prefix: text", "@param scope_min_id: integer", "@param limit: integer")
	slVolHdr    = annot("@param vol_min_users: integer", "@column n: integer")
	slAuditHdr  = annot("@param actor_id: integer", "@param limit: integer")
	slSetOpHdr  = annot("@param tenant_id: integer", "@param status: text", "@param tag: text", "@column kind: text")
	slDeleteHdr = annot("@param user_id: integer", "@param tag: text")
	slFindHdr   = annot("@param email: text")
)

var sqliteShapeCases = []shapeCase{
	{
		name: "guarded_where",
		query: "-- name: GuardedWhere :many" + slGuardedWhereHdr + `SELECT u.id, u.email
FROM users AS u
WHERE u.tenant_id = :tenant_id
@if-present(status)
  AND u.status = :status
@endif
@if-present(email_prefix)
  AND u.email LIKE :email_prefix || '%'
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
				wantSQL:  slGuardedWhereHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n\n\n\nORDER BY u.id\nLIMIT ?;\n",
				wantArgs: []string{"int64(7)", "int64(10)"},
			},
			{
				name: "all_guards",
				call: `q.GuardedWhere(ctx, gen.GuardedWhereParams{TenantID: 7, Status: sqletch.Present("active"), EmailPrefix: sqletch.Present("a"), MinID: sqletch.Present(int64(3)), Limit: 10})`,
				wantSQL: slGuardedWhereHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n" +
					"\nAND (u.status = ?)\n" +
					"\nAND (u.email LIKE ? || '%')\n" +
					"\nAND (u.id >= ?)\nORDER BY u.id\nLIMIT ?;\n",
				wantArgs: []string{"int64(7)", `string("active")`, `string("a")`, "int64(3)", "int64(10)"},
			},
			{
				name:     "first_guard_only",
				call:     `q.GuardedWhere(ctx, gen.GuardedWhereParams{TenantID: 7, Status: sqletch.Present("active"), Limit: 10})`,
				wantSQL:  slGuardedWhereHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n\nAND (u.status = ?)\n\n\nORDER BY u.id\nLIMIT ?;\n",
				wantArgs: []string{"int64(7)", `string("active")`, "int64(10)"},
			},
		},
	},
	{
		name: "guarded_join",
		query: "-- name: UsersInOrg :many" + slJoinHdr + `SELECT u.id, u.email
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
				wantSQL:  slJoinHdr + "SELECT u.id, u.email\nFROM users AS u\n\nWHERE u.tenant_id = ?\nORDER BY u.id;\n",
				wantArgs: []string{"int64(7)"},
			},
			{
				name: "join_on_binds_before_where",
				call: `q.UsersInOrg(ctx, gen.UsersInOrgParams{TenantID: 7, OrganizationID: sqletch.Present(int64(77))})`,
				wantSQL: slJoinHdr + "SELECT u.id, u.email\nFROM users AS u\n" +
					"\nJOIN organization_users AS ou\n  ON ou.user_id = u.id\n AND ou.organization_id = ?" +
					"\nWHERE u.tenant_id = ?\nORDER BY u.id;\n",
				wantArgs: []string{"int64(77)", "int64(7)"},
			},
		},
	},
	{
		name: "choose_sort",
		query: "-- name: SortedUsers :many" + slSortHdr + `SELECT u.id, u.email
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
				wantSQL:  slSortHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n\nORDER BY u.id ASC\nLIMIT ?;\n",
				wantArgs: []string{"int64(1)", "int64(5)"},
			},
			{
				name:     "second_case",
				call:     `q.SortedUsers(ctx, gen.SortedUsersParams{TenantID: 1, Sort: gen.SortedUsersSortEmail, Limit: 5})`,
				wantSQL:  slSortHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n\nORDER BY u.email ASC\nLIMIT ?;\n",
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
		query: "-- name: Bucketed :many" + slBucketHdr + `SELECT
@choose(bucket)
@case(day)
strftime('%Y-%m-%d', u.created_at)
@case(month)
strftime('%Y-%m', u.created_at)
@end
 AS bucket, u.id
FROM users AS u
WHERE u.created_at >= :since
ORDER BY u.id;
`,
		calls: []shapeCall{
			{
				name: "day",
				call: `q.Bucketed(ctx, gen.BucketedParams{Bucket: gen.BucketedBucketDay, Since: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)})`,
				wantSQL: slBucketHdr + "SELECT\n\nstrftime('%Y-%m-%d', u.created_at)\n AS bucket, u.id\n" +
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
		query: "-- name: OrderedUsers :many" + slSortHdr + `SELECT u.id, u.email
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
				wantSQL:  slSortHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n\nORDER BY u.id ASC\nLIMIT ?;\n",
				wantArgs: []string{"int64(1)", "int64(5)"},
			},
			{
				name:     "two_keys_both_desc",
				call:     `q.OrderedUsers(ctx, gen.OrderedUsersParams{TenantID: 1, Sort: []gen.OrderedUsersSortKey{gen.OrderedUsersSortEmailDesc, gen.OrderedUsersSortCreatedAtDesc}, Limit: 5})`,
				wantSQL:  slSortHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n\nORDER BY u.email DESC, u.created_at DESC\nLIMIT ?;\n",
				wantArgs: []string{"int64(1)", "int64(5)"},
			},
			{
				name:    "duplicate_key_rejected",
				call:    `q.OrderedUsers(ctx, gen.OrderedUsersParams{Sort: []gen.OrderedUsersSortKey{gen.OrderedUsersSortEmailAsc, gen.OrderedUsersSortEmailAsc}})`,
				wantErr: "invalid @order-by key selection",
			},
		},
	},
	{
		name: "when_literals",
		query: "-- name: UserFeed :many" + slFeedHdr + `SELECT u.id, u.email
FROM users AS u
WHERE u.tenant_id = :tenant_id
@when(level = 2)
  AND u.bio IS NOT NULL
@end
@when(level != 2)
  AND u.bio IS NULL
@end
ORDER BY u.id;
`,
		calls: []shapeCall{
			{
				name:     "not_equal_branch",
				call:     `q.UserFeed(ctx, gen.UserFeedParams{TenantID: 1})`,
				wantSQL:  slFeedHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n\n\nAND (u.bio IS NULL)\nORDER BY u.id;\n",
				wantArgs: []string{"int64(1)"},
			},
			{
				name:     "equal_branch",
				call:     `q.UserFeed(ctx, gen.UserFeedParams{TenantID: 1, Level: 2})`,
				wantSQL:  slFeedHdr + "SELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = ?\n\nAND (u.bio IS NOT NULL)\n\nORDER BY u.id;\n",
				wantArgs: []string{"int64(1)"},
			},
		},
	},
	{
		name: "in_list",
		query: "-- name: UsersInStatuses :many" + slInHdr + `SELECT u.id
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
				name:     "two_elements_expand",
				call:     `q.UsersInStatuses(ctx, gen.UsersInStatusesParams{TenantID: 1, Statuses: []string{"a", "b"}})`,
				wantSQL:  slInHdr + "SELECT u.id\nFROM users AS u\nWHERE u.tenant_id = ?\n  AND u.status IN (?, ?)\n\nORDER BY u.id;\n",
				wantArgs: []string{"int64(1)", `string("a")`, `string("b")`},
			},
			{
				name:     "empty_matches_nothing",
				call:     `q.UsersInStatuses(ctx, gen.UsersInStatusesParams{TenantID: 1, Statuses: []string{}})`,
				wantSQL:  slInHdr + "SELECT u.id\nFROM users AS u\nWHERE u.tenant_id = ?\n  AND u.status IN (SELECT NULL WHERE 0)\n\nORDER BY u.id;\n",
				wantArgs: []string{"int64(1)"},
			},
			{
				name:     "one_element_then_guard",
				call:     `q.UsersInStatuses(ctx, gen.UsersInStatusesParams{TenantID: 1, Statuses: []string{"x"}, MinID: sqletch.Present(int64(9))})`,
				wantSQL:  slInHdr + "SELECT u.id\nFROM users AS u\nWHERE u.tenant_id = ?\n  AND u.status IN (?)\n\nAND (u.id >= ?)\nORDER BY u.id;\n",
				wantArgs: []string{"int64(1)", `string("x")`, "int64(9)"},
			},
		},
	},
	{
		name: "repeated_param",
		query: "-- name: FindByHandle :many" + slHandleHdr + `SELECT u.id
FROM users AS u
WHERE u.email = :handle OR u.nickname = :handle
ORDER BY u.id;
`,
		calls: []shapeCall{
			{
				name:     "bind_repeated_per_occurrence",
				call:     `q.FindByHandle(ctx, gen.FindByHandleParams{Handle: "neo"})`,
				wantSQL:  slHandleHdr + "SELECT u.id\nFROM users AS u\nWHERE u.email = ? OR u.nickname = ?\nORDER BY u.id;\n",
				wantArgs: []string{`string("neo")`, `string("neo")`},
			},
		},
	},
	{
		name: "patch_update",
		query: "-- name: PatchUser :execrows" + slPatchHdr + `UPDATE users
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
				wantSQL:  slPatchHdr + "UPDATE users\nSET\n    email = email\n\n\nWHERE id = ?;\n",
				wantArgs: []string{"int64(1)"},
			},
			{
				name: "set_value_and_clear_to_null",
				call: `q.PatchUser(ctx, gen.PatchUserParams{ID: 1, Nickname: sqletch.Present(optional.None[string]()), Bio: sqletch.Present(optional.Some("こんにちは"))})`,
				wantSQL: slPatchHdr + "UPDATE users\nSET\n    email = email\n" +
					"\n, nickname = ?\n\n, bio = ?\nWHERE id = ?;\n",
				wantArgs: []string{"NULL", `string("こんにちは")`, "int64(1)"},
			},
		},
	},
	{
		name: "insert_optional_pair",
		query: "-- name: CreateNote :execrows" + slNoteHdr + `INSERT INTO notes (
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
				wantSQL:  slNoteHdr + "INSERT INTO notes (\n    user_id\n  , tag\n\n) VALUES (\n    ?\n  , ?\n\n);\n",
				wantArgs: []string{"int64(5)", "NULL"},
			},
			{
				name:     "present_none_writes_null",
				call:     `q.CreateNote(ctx, gen.CreateNoteParams{UserID: 5, Tag: optional.Some("t"), Body: sqletch.Present(optional.None[string]())})`,
				wantSQL:  slNoteHdr + "INSERT INTO notes (\n    user_id\n  , tag\n\n, body\n) VALUES (\n    ?\n  , ?\n\n, ?\n);\n",
				wantArgs: []string{"int64(5)", `string("t")`, "NULL"},
			},
		},
	},
	{
		name: "lexical_edges",
		query: "-- name: LexicalEdges :many" + slLexHdr + `SELECT u.id, u."status", 'it''s :not_param' AS lit, 'こんにちは' AS greeting
FROM users AS u
-- a comment mentioning :not_a_param
WHERE u.email = :email
  /* block comment :also_not */
ORDER BY u.id;
`,
		calls: []shapeCall{
			{
				name: "only_real_params_bind",
				call: `q.LexicalEdges(ctx, gen.LexicalEdgesParams{Email: "e@example.com"})`,
				wantSQL: slLexHdr + "SELECT u.id, u.\"status\", 'it''s :not_param' AS lit, 'こんにちは' AS greeting\n" +
					"FROM users AS u\n-- a comment mentioning :not_a_param\nWHERE u.email = ?\n" +
					"  /* block comment :also_not */\nORDER BY u.id;\n",
				wantArgs: []string{`string("e@example.com")`},
			},
		},
	},
	{
		name: "filter_tree_required",
		query: "-- name: FilterUsers :many" + slFilterHdr + `SELECT u.id
FROM users AS u
WHERE u.tenant_id = :tenant_id
  AND @filter-tree!(scope)
@predicate(status_eq)
u.status = :scope_status
@predicate(email_prefix)
u.email LIKE :scope_prefix || '%'
@predicate(min_id)
u.id >= :scope_min_id
@end
ORDER BY u.id
LIMIT :limit;
`,
		calls: []shapeCall{
			{
				name:     "single_leaf",
				call:     `q.FilterUsers(ctx, gen.FilterUsersEmailPrefix("x"), gen.FilterUsersParams{TenantID: 1, Limit: 10})`,
				wantSQL:  slFilterHdr + "SELECT u.id\nFROM users AS u\nWHERE u.tenant_id = ?\n  AND (u.email LIKE ? || '%')\nORDER BY u.id\nLIMIT ?;\n",
				wantArgs: []string{"int64(1)", `string("x")`, "int64(10)"},
			},
			{
				name: "or_of_and",
				call: `q.FilterUsers(ctx, gen.Or(gen.And(gen.FilterUsersStatusEq("a"), gen.FilterUsersMinID(2)), gen.FilterUsersStatusEq("b")), gen.FilterUsersParams{TenantID: 1, Limit: 10})`,
				wantSQL: slFilterHdr + "SELECT u.id\nFROM users AS u\nWHERE u.tenant_id = ?\n" +
					"  AND (((u.status = ?) AND (u.id >= ?)) OR (u.status = ?))\nORDER BY u.id\nLIMIT ?;\n",
				wantArgs: []string{"int64(1)", `string("a")`, "int64(2)", `string("b")`, "int64(10)"},
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
		query: "-- name: TenantVolumes :many" + slVolHdr + `SELECT u.tenant_id, count(*) AS n
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
				wantSQL:  slVolHdr + "SELECT u.tenant_id, count(*) AS n\nFROM users AS u\nGROUP BY u.tenant_id\nHAVING TRUE\n  AND TRUE\nORDER BY u.tenant_id;\n",
				wantArgs: []string{},
			},
			{
				name:     "leaf",
				call:     `q.TenantVolumes(ctx, gen.TenantVolumesParams{Vol: gen.TenantVolumesMinUsers(3)})`,
				wantSQL:  slVolHdr + "SELECT u.tenant_id, count(*) AS n\nFROM users AS u\nGROUP BY u.tenant_id\nHAVING TRUE\n  AND (count(*) >= ?)\nORDER BY u.tenant_id;\n",
				wantArgs: []string{"int64(3)"},
			},
		},
	},
	{
		name: "policy_where",
		query: "-- name: RecentAudit :many" + slAuditHdr + `SELECT a.id, a.event
FROM audit_logs AS a
WHERE a.event <> 'noop'
@if-present(actor_id)
  AND a.actor_id = :actor_id
@endif
ORDER BY a.id DESC
LIMIT :limit;
`,
		calls: []shapeCall{
			{
				name:     "no_guard",
				call:     `q.RecentAudit(ctx, gen.TenantID(3), gen.RecentAuditParams{Limit: 20})`,
				wantSQL:  slAuditHdr + "SELECT a.id, a.event\nFROM audit_logs AS a\nWHERE (a.tenant_id = ?) AND a.event <> 'noop'\n\nORDER BY a.id DESC\nLIMIT ?;\n",
				wantArgs: []string{"int64(3)", "int64(20)"},
			},
		},
	},
	{
		name: "policy_or_wrapped",
		query: `-- name: LoginEvents :many
SELECT a.id
FROM audit_logs AS a
WHERE a.event = 'login' OR a.event = 'logout'
ORDER BY a.id;
`,
		calls: []shapeCall{
			{
				name:     "or_parenthesized",
				call:     `q.LoginEvents(ctx, gen.TenantID(3), gen.LoginEventsParams{})`,
				wantSQL:  "\nSELECT a.id\nFROM audit_logs AS a\nWHERE (a.tenant_id = ?) AND (a.event = 'login' OR a.event = 'logout')\nORDER BY a.id;\n",
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
		name: "setop_branch_conjuncts",
		query: "-- name: Mentions :many" + slSetOpHdr + `SELECT u.id, 'user' AS kind FROM users AS u
WHERE u.tenant_id = :tenant_id
@if-present(status)
  AND u.status = :status
@endif
UNION ALL
SELECT n.user_id, 'note' AS kind FROM notes AS n
WHERE n.user_id > 0
@if-present(tag)
  AND n.tag = :tag
@endif
ORDER BY 1;
`,
		calls: []shapeCall{
			{
				name: "first_branch_only",
				call: `q.Mentions(ctx, gen.MentionsParams{TenantID: 1, Status: sqletch.Present("active")})`,
				wantSQL: slSetOpHdr + "SELECT u.id, 'user' AS kind FROM users AS u\nWHERE u.tenant_id = ?\n" +
					"\nAND (u.status = ?)" +
					"\nUNION ALL\nSELECT n.user_id, 'note' AS kind FROM notes AS n\nWHERE n.user_id > 0\n\nORDER BY 1;\n",
				wantArgs: []string{"int64(1)", `string("active")`},
			},
			{
				name: "both_branches",
				call: `q.Mentions(ctx, gen.MentionsParams{TenantID: 1, Status: sqletch.Present("active"), Tag: sqletch.Present("x")})`,
				wantSQL: slSetOpHdr + "SELECT u.id, 'user' AS kind FROM users AS u\nWHERE u.tenant_id = ?\n" +
					"\nAND (u.status = ?)" +
					"\nUNION ALL\nSELECT n.user_id, 'note' AS kind FROM notes AS n\nWHERE n.user_id > 0\n" +
					"\nAND (n.tag = ?)\nORDER BY 1;\n",
				wantArgs: []string{"int64(1)", `string("active")`, `string("x")`},
			},
		},
	},
	{
		name: "exec_kinds",
		query: "-- name: DeleteNotes :execrows" + slDeleteHdr + `DELETE FROM notes
WHERE user_id = :user_id
@if-present(tag)
  AND tag = :tag
@endif
;
`,
		calls: []shapeCall{
			{
				name:     "execrows_guard_off",
				call:     `q.DeleteNotes(ctx, gen.DeleteNotesParams{UserID: 2})`,
				wantSQL:  slDeleteHdr + "DELETE FROM notes\nWHERE user_id = ?\n\n;\n",
				wantArgs: []string{"int64(2)"},
			},
		},
	},
	{
		name: "maybe_one",
		query: "-- name: FindUser :maybe-one" + slFindHdr + `SELECT u.id, u.nickname FROM users AS u WHERE u.email = :email;
`,
		calls: []shapeCall{
			{
				name:     "maybe_one",
				call:     `q.FindUser(ctx, gen.FindUserParams{Email: "a@b"})`,
				wantSQL:  slFindHdr + "SELECT u.id, u.nickname FROM users AS u WHERE u.email = ?;\n",
				wantArgs: []string{`string("a@b")`},
			},
		},
	},
}
