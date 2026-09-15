package rules

import (
	"reflect"
	"sort"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/template"
)

// nullableParamsCatalog: users.email/status NOT NULL, users.nickname and
// users.bio nullable (bio has a non-NULL default), plus a view over it.
func nullableParamsCatalog(schema string) *cache.Catalog {
	users := cache.Table{Schema: schema, Name: "users", OID: 2001, Cols: []cache.Column{
		{Name: "id", Att: 1, NotNull: true, HasDefault: true},
		{Name: "email", Att: 2, NotNull: true},
		{Name: "status", Att: 3, NotNull: true},
		{Name: "nickname", Att: 4},
		{Name: "bio", Att: 5, HasDefault: true},
		{Name: "updated_at", Att: 6},
	}}
	view := cache.Table{Schema: schema, Name: "users_v", OID: 2002, IsView: true, Cols: []cache.Column{
		{Name: "email", Att: 1},
		{Name: "nickname", Att: 2},
	}}
	return &cache.Catalog{SchemaFP: "nullable-params", Tables: []cache.Table{users, view}}
}

type deriveDialect struct {
	name     string
	profile  dialect.LexerProfile
	frontend dialect.Frontend
	schema   string
}

var (
	derivePG     = deriveDialect{"postgres", postgres.Profile{}, postgres.Frontend{}, "public"}
	deriveMySQL  = deriveDialect{"mysql", mysql.Profile{}, mysql.Frontend{}, "app"}
	deriveSQLite = deriveDialect{"sqlite", sqlite.Profile{}, sqlite.Frontend{}, "main"}
)

func scanFor(t *testing.T, d deriveDialect, src string) *template.QueryTemplate {
	t.Helper()
	f, diags := template.NewScanner(d.profile).ScanFile("t.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	if diags := CheckLexical(d.profile, q); len(diags) != 0 {
		t.Fatalf("lexical (test precondition): %+v", diags)
	}
	return q
}

func deriveFor(t *testing.T, d deriveDialect, q *template.QueryTemplate, cat *cache.Catalog) []string {
	t.Helper()
	rs, err := ast.Renderings(d.profile, q)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := d.frontend.Parse(rs[0].SQL)
	if err != nil {
		t.Fatalf("parse maximal rendering: %v\n%s", err, rs[0].SQL)
	}
	var names []string
	for name, ok := range DeriveNullableParams(d.profile, q, rs[0], tree, cat) {
		if !ok {
			t.Errorf("DeriveNullableParams must only hold true entries; %q is false", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func assertNullable(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nullable params = %v, want %v", got, want)
	}
}

// Design 20 §3: a parameter is nullable iff EVERY bind occurrence is a
// direct value position of a nullable base-table column.
func TestDeriveNullableParams_Postgres(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"insert: only the nullable column's param", `-- name: CreateUser :exec
INSERT INTO users (email, nickname, bio) VALUES (:email, :nickname, :bio);
`, []string{"bio", "nickname"}},
		{"update set; WHERE param stays a value", `-- name: SetNickname :exec
UPDATE users SET nickname = :nickname WHERE id = :id;
`, []string{"nickname"}},
		{"cast-wrapped placeholder derives (Q4)", `-- name: SetNickname :exec
UPDATE users SET nickname = :nickname::text, bio = CAST(:bio AS text) WHERE id = :id;
`, []string{"bio", "nickname"}},
		{"expression does not derive", `-- name: SetNickname :exec
UPDATE users SET nickname = lower(:nickname) WHERE id = :id;
`, nil},
		{"co-occurrence outside a value position keeps T", `-- name: SetNickname :exec
UPDATE users SET nickname = :nickname WHERE nickname <> :nickname;
`, nil},
		{"every occurrence nullable: derives", `-- name: Dup :exec
INSERT INTO users (nickname, bio) VALUES (:x, :x);
`, []string{"x"}},
		{"one occurrence NOT NULL: keeps T", `-- name: Dup :exec
INSERT INTO users (email, nickname) VALUES (:x, :x);
`, nil},
		{"guarded INSERT pair derives (nested Omittable[Option])", `-- name: CreateUser :exec
INSERT INTO users (
    email
@if-present(nickname)
  , nickname
@endif
@if-present(status)
  , status
@endif
) VALUES (
    :email
@if-present(nickname)
  , :nickname
@endif
@if-present(status)
  , :status
@endif
);
`, []string{"nickname"}},
		{"guarded UPDATE SET derives", `-- name: Patch :exec
UPDATE users SET updated_at = now()
@if-present(nickname)
  , nickname = :nickname
@endif
WHERE id = :id;
`, []string{"nickname"}},
		{"@when control parameter never derives", `-- name: Patch :exec
UPDATE users SET updated_at = now()
@when(nickname != 'keep')
  , nickname = :nickname
@end
WHERE id = :id;
`, nil},
		{"value inside another parameter's guard derives", `-- name: Patch :exec
UPDATE users SET updated_at = now()
@if-present(flag)
  , nickname = :nickname, status = :flag
@endif
WHERE id = :id;
`, []string{"nickname"}},
		{"upsert arm repeating the placeholder keeps T", `-- name: Upsert :exec
INSERT INTO users (id, nickname) VALUES (:id, :nickname)
ON CONFLICT (id) DO UPDATE SET nickname = :nickname;
`, nil},
		{"upsert EXCLUDED idiom derives", `-- name: Upsert :exec
INSERT INTO users (id, nickname) VALUES (:id, :nickname)
ON CONFLICT (id) DO UPDATE SET nickname = EXCLUDED.nickname;
`, []string{"nickname"}},
		{"view target never derives", `-- name: ViaView :exec
INSERT INTO users_v (email, nickname) VALUES (:email, :nickname);
`, nil},
		{"unknown table never derives", `-- name: Ghost :exec
INSERT INTO ghosts (nickname) VALUES (:nickname);
`, nil},
		{"unknown column never derives", `-- name: Ghost :exec
UPDATE users SET ghost = :ghost WHERE id = :id;
`, nil},
		{"schema-qualified target resolves", `-- name: Qualified :exec
UPDATE public.users SET nickname = :nickname WHERE id = :id;
`, []string{"nickname"}},
		{"wrong schema never derives", `-- name: Qualified :exec
UPDATE other.users SET nickname = :nickname WHERE id = :id;
`, nil},
		{"PostgreSQL identifiers are case-sensitive once quoted", `-- name: Quoted :exec
UPDATE users SET "NickName" = :nickname WHERE id = :id;
`, nil},
		{"select never derives", `-- name: Pick :many
SELECT u.id FROM users AS u WHERE u.nickname = :nickname;
`, nil},
		{"multibyte text before the placeholder", `-- name: CreateUser :exec
INSERT INTO users (email, nickname) VALUES ('日本語', :nickname);
`, []string{"nickname"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := scanFor(t, derivePG, tc.src)
			assertNullable(t, deriveFor(t, derivePG, q, nullableParamsCatalog("public")), tc.want...)
		})
	}
}

// A nil catalog (no snapshot yet) derives nothing — never a guess.
func TestDeriveNullableParams_NilCatalog(t *testing.T) {
	q := scanFor(t, derivePG, `-- name: SetNickname :exec
UPDATE users SET nickname = :nickname WHERE id = :id;
`)
	assertNullable(t, deriveFor(t, derivePG, q, nil))
}

// A policy-woven parameter is a scoping value: it must never become
// nullable, even if it were somehow bound in a value position.
func TestDeriveNullableParams_PolicyParamNeverDerives(t *testing.T) {
	q := scanFor(t, derivePG, `-- name: SetNickname :exec
UPDATE users SET nickname = :nickname WHERE id = :id;
`)
	q.Params["nickname"].Policy = "tenant"
	assertNullable(t, deriveFor(t, derivePG, q, nullableParamsCatalog("public")))
}

func TestDeriveNullableParams_MySQL(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"insert values", `-- name: CreateUser :exec
INSERT INTO users (email, nickname) VALUES (:email, :nickname);
`, []string{"nickname"}},
		{"case-insensitive column and table", `-- name: CreateUser :exec
INSERT INTO Users (EMAIL, NickName) VALUES (:email, :nickname);
`, []string{"nickname"}},
		{"alias-qualified set column", `-- name: SetNickname :exec
UPDATE users AS u SET u.nickname = :nickname WHERE u.id = :id;
`, []string{"nickname"}},
		{"table-qualified set column", `-- name: SetNickname :exec
UPDATE users SET users.nickname = :nickname WHERE id = :id;
`, []string{"nickname"}},
		{"qualifier naming another relation never derives", `-- name: SetNickname :exec
UPDATE users AS u SET users.nickname = :nickname WHERE u.id = :id;
`, nil},
		{"cast-wrapped", `-- name: SetNickname :exec
UPDATE users SET nickname = CAST(:nickname AS CHAR) WHERE id = :id;
`, []string{"nickname"}},
		{"on duplicate key arm repeating the placeholder keeps T", `-- name: Upsert :exec
INSERT INTO users (id, nickname) VALUES (:id, :nickname)
ON DUPLICATE KEY UPDATE nickname = :nickname;
`, nil},
		{"multi-table update never derives", `-- name: Multi :exec
UPDATE users AS u JOIN users_v AS v ON v.email = u.email SET u.nickname = :nickname;
`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := scanFor(t, deriveMySQL, tc.src)
			assertNullable(t, deriveFor(t, deriveMySQL, q, nullableParamsCatalog("app")), tc.want...)
		})
	}
}

func TestDeriveNullableParams_SQLite(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"insert values", `-- name: CreateUser :exec
INSERT INTO users (email, nickname) VALUES (:email, :nickname);
`, []string{"nickname"}},
		{"case-insensitive column", `-- name: CreateUser :exec
INSERT INTO users (email, NickName) VALUES (:email, :nickname);
`, []string{"nickname"}},
		{"update set with multibyte before", `-- name: SetNickname :exec
UPDATE users SET status = '日本語', nickname = :nickname WHERE id = :id;
`, []string{"nickname"}},
		{"view target never derives", `-- name: ViaView :exec
INSERT INTO users_v (email, nickname) VALUES (:email, :nickname);
`, nil},
		{"upsert excluded idiom derives", `-- name: Upsert :exec
INSERT INTO users (id, nickname) VALUES (:id, :nickname)
ON CONFLICT (id) DO UPDATE SET nickname = excluded.nickname;
`, []string{"nickname"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := scanFor(t, deriveSQLite, tc.src)
			assertNullable(t, deriveFor(t, deriveSQLite, q, nullableParamsCatalog("main")), tc.want...)
		})
	}
}
