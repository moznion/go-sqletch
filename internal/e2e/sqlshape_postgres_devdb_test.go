//go:build devdb

package e2e_test

import (
	"context"
	"os"
	"testing"

	"github.com/moznion/go-sqletch/internal/devdb"
)

// TestSQLShapesPostgres pins the exact SQL + binds the generated pgx
// code sends for each template construct (harness: sqlshape_devdb_test.go).
func TestSQLShapesPostgres(t *testing.T) {
	runShapeSuite(t, shapeSuite{
		dialect:  postgresShapeDialect,
		schema:   pgShapeSchema,
		policies: tenantPolicyYAML,
		cases:    pgShapeCases,
	})
}

var postgresShapeDialect = shapeDialect{
	name:          "postgres",
	serverVersion: "16",
	acquire: func(t *testing.T, ctx context.Context, _ string) (string, func()) {
		dsn, cleanup, err := devdb.AcquireDSN(ctx, devdb.Config{
			DSN:              os.Getenv("SQLETCH_TEST_DSN"),
			AllowDestructive: true,
			ServerVersion:    "16",
		})
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		return dsn, cleanup
	},
	requires: []string{"github.com/jackc/pgx/v5"},
	imports:  []string{`"github.com/jackc/pgx/v5"`, `"github.com/jackc/pgx/v5/pgconn"`},
	recorder: pgxRecorder,
}

// tenantPolicyYAML scopes audit_logs by tenant in every dialect suite.
const tenantPolicyYAML = `policies:
  - name: tenant_scope
    tables: [audit_logs]
    predicate: "{}.tenant_id = :tenant_id"
    param:
      name: tenant_id
      type: bigint
`

const pgShapeSchema = `
CREATE TABLE users (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id  bigint NOT NULL,
    email      text NOT NULL,
    status     text NOT NULL,
    nickname   text,
    bio        text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE organization_users (
    user_id         bigint NOT NULL,
    organization_id bigint NOT NULL,
    PRIMARY KEY (user_id, organization_id)
);
CREATE TABLE notes (
    id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id bigint NOT NULL,
    body    text DEFAULT 'n/a',
    tag     text
);
CREATE TABLE audit_logs (
    id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id bigint NOT NULL,
    actor_id  bigint,
    action    text NOT NULL
);
`

var pgShapeCases = []shapeCase{
	{
		name: "guarded_where",
		query: `-- name: GuardedWhere :many
SELECT u.id, u.email
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
				wantSQL:  "\nSELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = $1\n\n\n\nORDER BY u.id\nLIMIT $2;\n",
				wantArgs: []string{"int64(7)", "int64(10)"},
			},
			{
				name: "all_guards_number_in_text_order",
				call: `q.GuardedWhere(ctx, gen.GuardedWhereParams{TenantID: 7, Status: sqletch.Present("active"), EmailPrefix: sqletch.Present("a"), MinID: sqletch.Present(int64(3)), Limit: 10})`,
				wantSQL: "\nSELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = $1\n" +
					"\nAND (u.status = $2)\n" +
					"\nAND (u.email LIKE $3 || '%')\n" +
					"\nAND (u.id >= $4)\nORDER BY u.id\nLIMIT $5;\n",
				wantArgs: []string{"int64(7)", `string("active")`, `string("a")`, "int64(3)", "int64(10)"},
			},
			{
				name:     "last_guard_only_compacts_numbering",
				call:     `q.GuardedWhere(ctx, gen.GuardedWhereParams{TenantID: 7, MinID: sqletch.Present(int64(3)), Limit: 10})`,
				wantSQL:  "\nSELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = $1\n\n\n\nAND (u.id >= $2)\nORDER BY u.id\nLIMIT $3;\n",
				wantArgs: []string{"int64(7)", "int64(3)", "int64(10)"},
			},
			{
				name:     "present_zero_value_is_present",
				call:     `q.GuardedWhere(ctx, gen.GuardedWhereParams{Status: sqletch.Present("")})`,
				wantSQL:  "\nSELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = $1\n\nAND (u.status = $2)\n\n\nORDER BY u.id\nLIMIT $3;\n",
				wantArgs: []string{"int64(0)", `string("")`, "int64(0)"},
			},
		},
	},
	{
		name: "guarded_join",
		query: `-- name: UsersInOrg :many
SELECT u.id, u.email
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
				wantSQL:  "\nSELECT u.id, u.email\nFROM users AS u\n\nWHERE u.tenant_id = $1\nORDER BY u.id;\n",
				wantArgs: []string{"int64(7)"},
			},
			{
				name: "join_on_binds_before_where",
				call: `q.UsersInOrg(ctx, gen.UsersInOrgParams{TenantID: 7, OrganizationID: sqletch.Present(int64(77))})`,
				wantSQL: "\nSELECT u.id, u.email\nFROM users AS u\n" +
					"\nJOIN organization_users AS ou\n  ON ou.user_id = u.id\n AND ou.organization_id = $1" +
					"\nWHERE u.tenant_id = $2\nORDER BY u.id;\n",
				wantArgs: []string{"int64(77)", "int64(7)"},
			},
		},
	},
	{
		name: "choose_sort",
		query: `-- name: SortedUsers :many
SELECT u.id, u.email
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
				wantSQL:  "\nSELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = $1\n\nORDER BY u.id ASC\nLIMIT $2;\n",
				wantArgs: []string{"int64(1)", "int64(5)"},
			},
			{
				name:     "first_case",
				call:     `q.SortedUsers(ctx, gen.SortedUsersParams{TenantID: 1, Sort: gen.SortedUsersSortNewest, Limit: 5})`,
				wantSQL:  "\nSELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = $1\n\nORDER BY u.created_at DESC, u.id DESC\nLIMIT $2;\n",
				wantArgs: []string{"int64(1)", "int64(5)"},
			},
			{
				name:     "second_case",
				call:     `q.SortedUsers(ctx, gen.SortedUsersParams{TenantID: 1, Sort: gen.SortedUsersSortEmail, Limit: 5})`,
				wantSQL:  "\nSELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = $1\n\nORDER BY u.email ASC\nLIMIT $2;\n",
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
		query: `-- name: Bucketed :many
SELECT
@choose(bucket)
@case(day)
date_trunc('day', u.created_at)
@case(month)
date_trunc('month', u.created_at)
@end
 AS bucket, count(*) AS n
FROM users AS u
WHERE u.created_at >= :since
GROUP BY 1
ORDER BY 1;
`,
		calls: []shapeCall{
			{
				name: "day",
				call: `q.Bucketed(ctx, gen.BucketedParams{Bucket: gen.BucketedBucketDay, Since: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)})`,
				wantSQL: "\nSELECT\n\ndate_trunc('day', u.created_at)\n AS bucket, count(*) AS n\n" +
					"FROM users AS u\nWHERE u.created_at >= $1\nGROUP BY 1\nORDER BY 1;\n",
				wantArgs: []string{"time.Time(2024-01-02T03:04:05Z)"},
			},
			{
				name: "month",
				call: `q.Bucketed(ctx, gen.BucketedParams{Bucket: gen.BucketedBucketMonth, Since: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)})`,
				wantSQL: "\nSELECT\n\ndate_trunc('month', u.created_at)\n AS bucket, count(*) AS n\n" +
					"FROM users AS u\nWHERE u.created_at >= $1\nGROUP BY 1\nORDER BY 1;\n",
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
		query: `-- name: OrderedUsers :many
SELECT u.id, u.email
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
				wantSQL:  "\nSELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = $1\n\nORDER BY u.id ASC\nLIMIT $2;\n",
				wantArgs: []string{"int64(1)", "int64(5)"},
			},
			{
				name:     "single_key_asc",
				call:     `q.OrderedUsers(ctx, gen.OrderedUsersParams{TenantID: 1, Sort: []gen.OrderedUsersSortKey{gen.OrderedUsersSortEmailAsc}, Limit: 5})`,
				wantSQL:  "\nSELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = $1\n\nORDER BY u.email\nLIMIT $2;\n",
				wantArgs: []string{"int64(1)", "int64(5)"},
			},
			{
				name:     "two_keys_caller_order_with_desc",
				call:     `q.OrderedUsers(ctx, gen.OrderedUsersParams{TenantID: 1, Sort: []gen.OrderedUsersSortKey{gen.OrderedUsersSortEmailDesc, gen.OrderedUsersSortCreatedAtAsc}, Limit: 5})`,
				wantSQL:  "\nSELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = $1\n\nORDER BY u.email DESC, u.created_at\nLIMIT $2;\n",
				wantArgs: []string{"int64(1)", "int64(5)"},
			},
			{
				name:    "duplicate_key_rejected",
				call:    `q.OrderedUsers(ctx, gen.OrderedUsersParams{Sort: []gen.OrderedUsersSortKey{gen.OrderedUsersSortEmailAsc, gen.OrderedUsersSortEmailDesc}})`,
				wantErr: "invalid @order-by key selection",
			},
		},
	},
	{
		name: "when_literals",
		query: `-- name: UserFeed :many
SELECT u.id, u.email
FROM users AS u
WHERE u.tenant_id = :tenant_id
@when(include_banned = false)
  AND u.status <> 'banned'
@end
@when(level = 2)
  AND u.bio IS NOT NULL
@end
@when(mode = 'strict')
  AND u.nickname IS NOT NULL
@end
@when(mode != 'strict')
  AND u.nickname IS NULL
@end
ORDER BY u.id;
`,
		calls: []shapeCall{
			{
				name: "zero_values",
				call: `q.UserFeed(ctx, gen.UserFeedParams{TenantID: 1})`,
				wantSQL: "\nSELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = $1\n" +
					"\nAND (u.status <> 'banned')\n\n\n" +
					"\nAND (u.nickname IS NULL)\nORDER BY u.id;\n",
				wantArgs: []string{"int64(1)"},
			},
			{
				name: "matching_literals",
				call: `q.UserFeed(ctx, gen.UserFeedParams{TenantID: 1, IncludeBanned: true, Level: 2, Mode: "strict"})`,
				wantSQL: "\nSELECT u.id, u.email\nFROM users AS u\nWHERE u.tenant_id = $1\n\n" +
					"\nAND (u.bio IS NOT NULL)\n" +
					"\nAND (u.nickname IS NOT NULL)\n\nORDER BY u.id;\n",
				wantArgs: []string{"int64(1)"},
			},
		},
	},
	{
		name: "in_any",
		query: `-- name: UsersInStatuses :many
-- @param statuses: text[]
SELECT u.id
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
				name: "two_elements_one_array_bind",
				call: `q.UsersInStatuses(ctx, gen.UsersInStatusesParams{TenantID: 1, Statuses: []string{"active", "banned"}})`,
				wantSQL: "\n-- @param statuses: text[]\nSELECT u.id\nFROM users AS u\nWHERE u.tenant_id = $1\n" +
					"  AND u.status = ANY($2)\n\nORDER BY u.id;\n",
				wantArgs: []string{"int64(1)", `[]string{"active", "banned"}`},
			},
			{
				name: "empty_slice_same_shape",
				call: `q.UsersInStatuses(ctx, gen.UsersInStatusesParams{TenantID: 1, Statuses: []string{}})`,
				wantSQL: "\n-- @param statuses: text[]\nSELECT u.id\nFROM users AS u\nWHERE u.tenant_id = $1\n" +
					"  AND u.status = ANY($2)\n\nORDER BY u.id;\n",
				wantArgs: []string{"int64(1)", `[]string{}`},
			},
			{
				name: "with_trailing_guard",
				call: `q.UsersInStatuses(ctx, gen.UsersInStatusesParams{TenantID: 1, Statuses: []string{"x"}, MinID: sqletch.Present(int64(9))})`,
				wantSQL: "\n-- @param statuses: text[]\nSELECT u.id\nFROM users AS u\nWHERE u.tenant_id = $1\n" +
					"  AND u.status = ANY($2)\n\nAND (u.id >= $3)\nORDER BY u.id;\n",
				wantArgs: []string{"int64(1)", `[]string{"x"}`, "int64(9)"},
			},
		},
	},
	{
		name: "repeated_param",
		query: `-- name: FindByHandle :many
SELECT u.id
FROM users AS u
WHERE u.email = :handle OR u.nickname = :handle
ORDER BY u.id;
`,
		calls: []shapeCall{
			{
				name:     "one_placeholder_reused",
				call:     `q.FindByHandle(ctx, gen.FindByHandleParams{Handle: "neo"})`,
				wantSQL:  "\nSELECT u.id\nFROM users AS u\nWHERE u.email = $1 OR u.nickname = $1\nORDER BY u.id;\n",
				wantArgs: []string{`string("neo")`},
			},
		},
	},
	{
		name: "patch_update",
		query: `-- name: PatchUser :one
UPDATE users
SET
    email = email
@if-present(nickname)
  , nickname = :nickname
@endif
@if-present(bio)
  , bio = :bio
@endif
WHERE id = :id
RETURNING id, nickname, bio;
`,
		calls: []shapeCall{
			{
				name:     "nothing_set",
				call:     `q.PatchUser(ctx, gen.PatchUserParams{ID: 1})`,
				wantSQL:  "\nUPDATE users\nSET\n    email = email\n\n\nWHERE id = $1\nRETURNING id, nickname, bio;\n",
				wantArgs: []string{"int64(1)"},
			},
			{
				name: "set_value_and_clear_to_null",
				call: `q.PatchUser(ctx, gen.PatchUserParams{ID: 1, Nickname: sqletch.Present(optional.Some("ニック")), Bio: sqletch.Present(optional.None[string]())})`,
				wantSQL: "\nUPDATE users\nSET\n    email = email\n" +
					"\n, nickname = $1\n" +
					"\n, bio = $2\nWHERE id = $3\nRETURNING id, nickname, bio;\n",
				wantArgs: []string{`string("ニック")`, "NULL", "int64(1)"},
			},
			{
				name:     "second_only",
				call:     `q.PatchUser(ctx, gen.PatchUserParams{ID: 1, Bio: sqletch.Present(optional.Some("hi"))})`,
				wantSQL:  "\nUPDATE users\nSET\n    email = email\n\n\n, bio = $1\nWHERE id = $2\nRETURNING id, nickname, bio;\n",
				wantArgs: []string{`string("hi")`, "int64(1)"},
			},
		},
	},
	{
		name: "insert_optional_pair",
		query: `-- name: CreateNote :one
INSERT INTO notes (
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
)
RETURNING id;
`,
		calls: []shapeCall{
			{
				name: "omitted_pair_none_tag",
				call: `q.CreateNote(ctx, gen.CreateNoteParams{UserID: 5})`,
				wantSQL: "\nINSERT INTO notes (\n    user_id\n  , tag\n" +
					"\n) VALUES (\n    $1\n  , $2\n\n)\nRETURNING id;\n",
				wantArgs: []string{"int64(5)", "NULL"},
			},
			{
				name: "pair_present_both_sides",
				call: `q.CreateNote(ctx, gen.CreateNoteParams{UserID: 5, Tag: optional.Some("t"), Body: sqletch.Present(optional.Some("b"))})`,
				wantSQL: "\nINSERT INTO notes (\n    user_id\n  , tag\n\n, body" +
					"\n) VALUES (\n    $1\n  , $2\n\n, $3\n)\nRETURNING id;\n",
				wantArgs: []string{"int64(5)", `string("t")`, `string("b")`},
			},
			{
				name: "present_none_writes_null",
				call: `q.CreateNote(ctx, gen.CreateNoteParams{UserID: 5, Body: sqletch.Present(optional.None[string]())})`,
				wantSQL: "\nINSERT INTO notes (\n    user_id\n  , tag\n\n, body" +
					"\n) VALUES (\n    $1\n  , $2\n\n, $3\n)\nRETURNING id;\n",
				wantArgs: []string{"int64(5)", "NULL", "NULL"},
			},
		},
	},
	{
		name: "lexical_edges",
		query: `-- name: LexicalEdges :many
SELECT u.id, u.bio::text AS bio_text, 'a:b @if-present(x) :not_param' AS lit, $tag$:still_not$tag$ AS dq, 'こんにちは' AS greeting
FROM users AS u
-- a comment mentioning :not_a_param
WHERE u.email = :email::text
  AND u.created_at > now() - interval '1 day'
ORDER BY u.id;
`,
		calls: []shapeCall{
			{
				name: "only_real_params_bind",
				call: `q.LexicalEdges(ctx, gen.LexicalEdgesParams{Email: "e@example.com"})`,
				wantSQL: "\nSELECT u.id, u.bio::text AS bio_text, 'a:b @if-present(x) :not_param' AS lit, $tag$:still_not$tag$ AS dq, 'こんにちは' AS greeting\n" +
					"FROM users AS u\n-- a comment mentioning :not_a_param\nWHERE u.email = $1::text\n" +
					"  AND u.created_at > now() - interval '1 day'\nORDER BY u.id;\n",
				wantArgs: []string{`string("e@example.com")`},
			},
		},
	},
	{
		name: "filter_tree_required",
		query: `-- name: FilterUsers :many
SELECT u.id
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
				name:     "unscoped",
				call:     `q.FilterUsers(ctx, gen.FilterUsersUnscoped(), gen.FilterUsersParams{TenantID: 1, Limit: 10})`,
				wantSQL:  "\nSELECT u.id\nFROM users AS u\nWHERE u.tenant_id = $1\n  AND TRUE\nORDER BY u.id\nLIMIT $2;\n",
				wantArgs: []string{"int64(1)", "int64(10)"},
			},
			{
				name:     "single_leaf",
				call:     `q.FilterUsers(ctx, gen.FilterUsersStatusEq("active"), gen.FilterUsersParams{TenantID: 1, Limit: 10})`,
				wantSQL:  "\nSELECT u.id\nFROM users AS u\nWHERE u.tenant_id = $1\n  AND (u.status = $2)\nORDER BY u.id\nLIMIT $3;\n",
				wantArgs: []string{"int64(1)", `string("active")`, "int64(10)"},
			},
			{
				name: "nested_and_or",
				call: `q.FilterUsers(ctx, gen.And(gen.FilterUsersStatusEq("active"), gen.Or(gen.FilterUsersEmailPrefix("x"), gen.FilterUsersMinID(5))), gen.FilterUsersParams{TenantID: 1, Limit: 10})`,
				wantSQL: "\nSELECT u.id\nFROM users AS u\nWHERE u.tenant_id = $1\n" +
					"  AND ((u.status = $2) AND ((u.email LIKE $3 || '%') OR (u.id >= $4)))\nORDER BY u.id\nLIMIT $5;\n",
				wantArgs: []string{"int64(1)", `string("active")`, `string("x")`, "int64(5)", "int64(10)"},
			},
			{
				name:     "repeated_leaf_binds_independently",
				call:     `q.FilterUsers(ctx, gen.Or(gen.FilterUsersStatusEq("a"), gen.FilterUsersStatusEq("b")), gen.FilterUsersParams{TenantID: 1, Limit: 10})`,
				wantSQL:  "\nSELECT u.id\nFROM users AS u\nWHERE u.tenant_id = $1\n  AND ((u.status = $2) OR (u.status = $3))\nORDER BY u.id\nLIMIT $4;\n",
				wantArgs: []string{"int64(1)", `string("a")`, `string("b")`, "int64(10)"},
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
		query: `-- name: TenantVolumes :many
SELECT u.tenant_id, count(*) AS n
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
				wantSQL:  "\nSELECT u.tenant_id, count(*) AS n\nFROM users AS u\nGROUP BY u.tenant_id\nHAVING TRUE\n  AND TRUE\nORDER BY u.tenant_id;\n",
				wantArgs: []string{},
			},
			{
				name:     "leaf",
				call:     `q.TenantVolumes(ctx, gen.TenantVolumesParams{Vol: gen.TenantVolumesMinUsers(3)})`,
				wantSQL:  "\nSELECT u.tenant_id, count(*) AS n\nFROM users AS u\nGROUP BY u.tenant_id\nHAVING TRUE\n  AND (count(*) >= $1)\nORDER BY u.tenant_id;\n",
				wantArgs: []string{"int64(3)"},
			},
		},
	},
	{
		name: "policy_where",
		query: `-- name: RecentAudit :many
SELECT a.id, a.action
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
				name:     "tenant_conjunct_woven_first",
				call:     `q.RecentAudit(ctx, gen.TenantID(3), gen.RecentAuditParams{Limit: 20})`,
				wantSQL:  "\nSELECT a.id, a.action\nFROM audit_logs AS a\nWHERE (a.tenant_id = $1) AND a.action <> 'noop'\n\nORDER BY a.id DESC\nLIMIT $2;\n",
				wantArgs: []string{"int64(3)", "int64(20)"},
			},
			{
				name: "with_guard",
				call: `q.RecentAudit(ctx, gen.TenantID(3), gen.RecentAuditParams{ActorID: sqletch.Present(int64(8)), Limit: 20})`,
				wantSQL: "\nSELECT a.id, a.action\nFROM audit_logs AS a\nWHERE (a.tenant_id = $1) AND a.action <> 'noop'\n" +
					"\nAND (a.actor_id = $2)\nORDER BY a.id DESC\nLIMIT $3;\n",
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
				wantSQL:  "\nSELECT a.id\nFROM audit_logs AS a\nWHERE (a.tenant_id = $1) AND (a.action = 'login' OR a.action = 'logout')\nORDER BY a.id;\n",
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
				wantSQL:  "\nSELECT a.id FROM audit_logs AS a WHERE (a.tenant_id = $1) ORDER BY a.id;\n",
				wantArgs: []string{"int64(3)"},
			},
		},
	},
	{
		name: "setop_branch_conjuncts",
		query: `-- name: Mentions :many
SELECT u.id, 'user' AS kind FROM users AS u
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
				name: "no_guards",
				call: `q.Mentions(ctx, gen.MentionsParams{TenantID: 1})`,
				wantSQL: "\nSELECT u.id, 'user' AS kind FROM users AS u\nWHERE u.tenant_id = $1\n" +
					"\nUNION ALL\nSELECT n.user_id, 'note' AS kind FROM notes AS n\nWHERE n.user_id > 0\n\nORDER BY 1;\n",
				wantArgs: []string{"int64(1)"},
			},
			{
				name: "second_branch_only",
				call: `q.Mentions(ctx, gen.MentionsParams{TenantID: 1, Tag: sqletch.Present("x")})`,
				wantSQL: "\nSELECT u.id, 'user' AS kind FROM users AS u\nWHERE u.tenant_id = $1\n" +
					"\nUNION ALL\nSELECT n.user_id, 'note' AS kind FROM notes AS n\nWHERE n.user_id > 0\n" +
					"\nAND (n.tag = $2)\nORDER BY 1;\n",
				wantArgs: []string{"int64(1)", `string("x")`},
			},
			{
				name: "both_branches",
				call: `q.Mentions(ctx, gen.MentionsParams{TenantID: 1, Status: sqletch.Present("active"), Tag: sqletch.Present("x")})`,
				wantSQL: "\nSELECT u.id, 'user' AS kind FROM users AS u\nWHERE u.tenant_id = $1\n" +
					"\nAND (u.status = $2)" +
					"\nUNION ALL\nSELECT n.user_id, 'note' AS kind FROM notes AS n\nWHERE n.user_id > 0\n" +
					"\nAND (n.tag = $3)\nORDER BY 1;\n",
				wantArgs: []string{"int64(1)", `string("active")`, `string("x")`},
			},
		},
	},
	{
		name: "exec_kinds",
		query: `-- name: DeleteNotes :execrows
DELETE FROM notes
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
				wantSQL:  "\nDELETE FROM notes\nWHERE user_id = $1\n\n;\n",
				wantArgs: []string{"int64(2)"},
			},
			{
				name:     "execrows_guard_on",
				call:     `q.DeleteNotes(ctx, gen.DeleteNotesParams{UserID: 2, Tag: sqletch.Present("old")})`,
				wantSQL:  "\nDELETE FROM notes\nWHERE user_id = $1\n\nAND (tag = $2)\n;\n",
				wantArgs: []string{"int64(2)", `string("old")`},
			},
		},
	},
	{
		name: "exec_plain",
		query: `-- name: TouchUser :exec
UPDATE users SET bio = bio WHERE id = :id;
`,
		calls: []shapeCall{
			{
				name:     "exec",
				call:     `q.TouchUser(ctx, gen.TouchUserParams{ID: 4})`,
				wantSQL:  "\nUPDATE users SET bio = bio WHERE id = $1;\n",
				wantArgs: []string{"int64(4)"},
			},
		},
	},
	{
		name: "maybe_one",
		query: `-- name: FindUser :maybe-one
SELECT u.id, u.nickname FROM users AS u WHERE u.email = :email;
`,
		calls: []shapeCall{
			{
				name:     "maybe_one",
				call:     `q.FindUser(ctx, gen.FindUserParams{Email: "a@b"})`,
				wantSQL:  "\nSELECT u.id, u.nickname FROM users AS u WHERE u.email = $1;\n",
				wantArgs: []string{`string("a@b")`},
			},
		},
	},
}
