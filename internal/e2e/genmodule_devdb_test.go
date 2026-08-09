//go:build devdb

package e2e_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/codegen"
	"github.com/moznion/go-sqletch/internal/devdb"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/nullability"
	"github.com/moznion/go-sqletch/internal/rules"
)

const getUserProfile = `-- name: GetUserProfile :one
SELECT u.id, u.email, u.nickname, u.org_id
FROM users AS u
WHERE u.id = :id
@if-present(status)
  AND u.status = :status
@endif
;
`

const findUserByEmail = `-- name: FindUserByEmail :maybe-one
SELECT u.id, u.email, u.nickname
FROM users AS u
WHERE u.email = :email;
`

// TestGeneratedModuleEndToEnd is the full-loop E2E: run the complete
// pipeline (scan → rules → renderings → oracle → nullability →
// codegen), materialize the generated package as a standalone Go
// module, compile it, and execute it against the dev database with
// NULL-heavy seed data. Scanning NULLs into the generated structs and
// shape selection through the public API are what's under test.
func TestGeneratedModuleEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn, cleanup, err := devdb.AcquireDSN(ctx, devdb.Config{
		DSN:           os.Getenv("SQLETCH_TEST_DSN"),
		ServerVersion: "16",
		SchemaSQL:     []string{schemaSQL},
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer cleanup()

	conn, connCleanup, err := devdb.Acquire(ctx, devdb.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer connCleanup()
	oracle := postgres.NewOracle(conn)
	cat, err := oracle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Full pipeline for the corpus + the :one/:maybe-one nullable-
	// columns queries.
	var inputs []codegen.QueryInput
	for _, src := range []string{corpus["search_users"], corpus["list_audit_logs"], corpus["update_user_profile"], corpus["create_user"], corpus["signups_by_bucket"], corpus["when_and_having"], corpus["order_by_users"], corpus["filter_tree"], corpus["in_list"], getUserProfile, findUserByEmail} {
		q := compile(t, src)
		if d := rules.CheckLexical(postgres.Profile{}, q); len(d) != 0 {
			t.Fatalf("lexical: %+v", d)
		}
		rs, err := ast.Renderings(postgres.Profile{}, q)
		if err != nil {
			t.Fatal(err)
		}
		var descs []dialect.Desc
		for _, r := range rs {
			d, err := oracle.Describe(ctx, r.SQL)
			if err != nil {
				t.Fatalf("%s: describe: %v", q.Name, err)
			}
			descs = append(descs, d)
		}
		if d := rules.CheckTypeAgreement(q, rs, descs); len(d) != 0 {
			t.Fatalf("%s: agreement: %+v", q.Name, d)
		}
		types, d := rules.ResolveParamTypes(q, rs, descs)
		if len(d) != 0 {
			t.Fatalf("%s: types: %+v", q.Name, d)
		}
		typeMap := map[string]dialect.TypeRef{}
		for _, pt := range types {
			typeMap[pt.Name] = pt.Type
		}
		nullable, err := nullability.AnalyzeAll(postgres.Frontend{}, rs, descs, cat, nil)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, codegen.QueryInput{
			Q:          q,
			Frags:      codegen.BuildFrags(postgres.Profile{}, q),
			ParamTypes: typeMap,
			Columns:    descs[0].Columns,
			Nullable:   nullable,
		})
	}

	files, diags := codegen.Generate(codegen.Options{Package: "gen"}, postgres.TypeMap{}, inputs)
	if len(diags) != 0 {
		t.Fatalf("generate: %+v", diags)
	}

	// ---- materialize a standalone module --------------------------------
	dir := t.TempDir()
	genDir := filepath.Join(dir, "gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(genDir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	parentMod, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	pgxVer := regexp.MustCompile(`github\.com/jackc/pgx/v5 (v[0-9A-Za-z.\-+]+)`).FindStringSubmatch(string(parentMod))
	if pgxVer == nil {
		t.Fatal("pgx version not found in parent go.mod")
	}
	optVer := regexp.MustCompile(`github\.com/moznion/go-optional (v[0-9A-Za-z.\-+]+)`).FindStringSubmatch(string(parentMod))
	if optVer == nil {
		t.Fatal("go-optional version not found in parent go.mod")
	}
	goMod := "module sqletchgen\n\ngo 1.24\n\nrequire (\n" +
		"\tgithub.com/jackc/pgx/v5 " + pgxVer[1] + "\n" +
		"\tgithub.com/moznion/go-optional " + optVer[1] + "\n" +
		"\tgithub.com/moznion/go-sqletch v0.0.0\n)\n\n" +
		"replace github.com/moznion/go-sqletch => " + repoRoot + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	parentSum, err := os.ReadFile(filepath.Join(repoRoot, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), parentSum, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(e2eMain), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "SQLETCH_TEST_DSN="+dsn)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	run("mod", "tidy")
	out := run("run", ".")
	if !strings.Contains(out, "E2E-OK") {
		t.Fatalf("generated module run did not report success:\n%s", out)
	}
}

const e2eMain = `package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/moznion/go-optional"

	sqletchruntime "github.com/moznion/go-sqletch/runtime"

	gen "sqletchgen/gen"
)

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		os.Exit(1)
	}
}

func expect(cond bool, msg string) {
	if !cond {
		fmt.Fprintln(os.Stderr, "EXPECT FAILED:", msg)
		os.Exit(1)
	}
}

func main() {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("SQLETCH_TEST_DSN"))
	die(err)
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx, ` + "`" + `
		TRUNCATE users, organization_users, audit_logs RESTART IDENTITY;
		INSERT INTO users (email, status, tenant_id, org_id, nickname) VALUES
			('alice@example.com', 'active', 1, 10, 'al'),
			('bob@example.com',   'active', 1, NULL, NULL),
			('carol@example.com', 'banned', 1, NULL, NULL);
		INSERT INTO organization_users (user_id, organization_id) VALUES (1, 77);
		INSERT INTO audit_logs (tenant_id, actor_id, action) VALUES
			(1, 1, 'login'), (1, NULL, 'cron'), (1, 2, 'logout');
	` + "`" + `)
	die(err)

	q := gen.New(conn)
	shapes := 0
	q.OnQuery(func(key, sql string) { shapes++ })

	all, err := q.SearchUsers(ctx, gen.SearchUsersParams{Limit: 100})
	die(err)
	expect(len(all) == 3, "all users")

	active, err := q.SearchUsers(ctx, gen.SearchUsersParams{
		Status: optional.Some("active"),
		Sort:   gen.SearchUsersSortEmailAsc,
		Limit:  100,
	})
	die(err)
	expect(len(active) == 2 && active[0].Email == "alice@example.com", "active users sorted by email")

	org, err := q.SearchUsers(ctx, gen.SearchUsersParams{
		OrganizationID: optional.Some(int64(77)),
		Limit:          100,
	})
	die(err)
	expect(len(org) == 1 && org[0].ID == 1, "org-scoped join shape")

	// NULL-heavy scan through the generated :one function.
	bob, err := q.GetUserProfile(ctx, gen.GetUserProfileParams{ID: 2})
	die(err)
	expect(bob.Nickname.IsNone() && bob.OrgID.IsNone(), "bob's NULLs scan into None")
	alice, err := q.GetUserProfile(ctx, gen.GetUserProfileParams{ID: 1})
	die(err)
	expect(alice.Nickname.TakeOr("") == "al", "alice's nickname")

	_, err = q.GetUserProfile(ctx, gen.GetUserProfileParams{ID: 999})
	expect(errors.Is(err, pgx.ErrNoRows), "missing row yields pgx.ErrNoRows")

	// :maybe-one — no row is None, never an error; a hit is Some with
	// NULL columns still scanning into None fields.
	hitOpt, err := q.FindUserByEmail(ctx, gen.FindUserByEmailParams{Email: "bob@example.com"})
	die(err)
	hit, err := hitOpt.Take()
	die(err)
	expect(hit.ID == 2 && hit.Nickname.IsNone(), "maybe-one hit with NULL nickname")
	missOpt, err := q.FindUserByEmail(ctx, gen.FindUserByEmailParams{Email: "zoe@example.com"})
	die(err)
	expect(missOpt.IsNone(), "maybe-one miss is None, not an error")

	// PATCH semantics: update only the provided field; others untouched.
	upd, err := q.UpdateUserProfile(ctx, gen.UpdateUserProfileParams{
		ID:       1,
		Nickname: optional.Some("allie"),
	})
	die(err)
	expect(upd.Nickname.TakeOr("") == "allie", "nickname updated")
	expect(upd.Email == "alice@example.com", "email untouched by partial update")
	expect(upd.Bio.IsNone(), "bio untouched (still NULL)")

	upd2, err := q.UpdateUserProfile(ctx, gen.UpdateUserProfileParams{
		ID:       1,
		NewEmail: optional.Some("alice2@example.com"),
		Bio:      optional.Some("hello"),
	})
	die(err)
	expect(upd2.Email == "alice2@example.com", "email updated")
	expect(upd2.Nickname.TakeOr("") == "allie", "nickname survives the second patch")
	expect(upd2.Bio.TakeOr("") == "hello", "bio updated")

	// Minimal shape: no fields provided — the anchor keeps it valid.
	upd3, err := q.UpdateUserProfile(ctx, gen.UpdateUserProfileParams{ID: 1})
	die(err)
	expect(upd3.Email == "alice2@example.com", "no-op patch changes nothing")

	// Optional INSERT pairs: omitted columns receive their defaults
	// (NULL here); provided pairs land together.
	created, err := q.CreateUser(ctx, gen.CreateUserParams{
		Email:    "dave@example.com",
		Status:   "active",
		TenantID: 1,
	})
	die(err)
	expect(created.Nickname.IsNone() && created.Bio.IsNone(), "omitted optional columns default to NULL")
	created2, err := q.CreateUser(ctx, gen.CreateUserParams{
		Email:    "erin@example.com",
		Status:   "active",
		TenantID: 1,
		Nickname: optional.Some("er"),
	})
	die(err)
	expect(created2.Nickname.TakeOr("") == "er" && created2.Bio.IsNone(),
		"provided pair inserts, unprovided stays NULL")

	// @choose in a projection slot: the aggregation expression swaps
	// per case; a required enum's zero value errors before the DB.
	buckets, err := q.SignupsByBucket(ctx, gen.SignupsByBucketParams{
		Bucket: gen.SignupsByBucketBucketDaily,
		Since:  time.Now().Add(-24 * time.Hour),
	})
	die(err)
	total := int64(0)
	for _, b := range buckets {
		total += b.Signups
	}
	expect(total == 5, "daily buckets count all five users")
	_, err = q.SignupsByBucket(ctx, gen.SignupsByBucketParams{Since: time.Now()})
	expect(err != nil, "zero-value required @choose errors before touching the DB")

	// @when value guard + HAVING conjunct: audit_logs has one row with
	// a NULL actor (the cron entry). include_cron=false filters it.
	act, err := q.TenantActivity(ctx, gen.TenantActivityParams{IncludeCron: true})
	die(err)
	expect(len(act) == 1 && act[0].Actions == 3, "all actions incl. cron")
	act, err = q.TenantActivity(ctx, gen.TenantActivityParams{IncludeCron: false})
	die(err)
	expect(len(act) == 1 && act[0].Actions == 2, "@when guard drops the NULL-actor row")
	act, err = q.TenantActivity(ctx, gen.TenantActivityParams{IncludeCron: true, MinActions: optional.Some(int64(99))})
	die(err)
	expect(len(act) == 0, "HAVING conjunct filters the group out")

	// @order-by: permutation + direction through the generated API.
	ord, err := q.OrderedUsers(ctx, gen.OrderedUsersParams{
		Sort:  []gen.OrderedUsersSortKey{gen.OrderedUsersSortEmailDesc, gen.OrderedUsersSortCreatedAtAsc},
		Limit: 100,
	})
	die(err)
	expect(len(ord) >= 5, "ordered users returned")
	expect(ord[0].Email > ord[len(ord)-1].Email, "email DESC is the primary key sequence")
	// Empty selection falls back to @default (ORDER BY u.id ASC).
	ord, err = q.OrderedUsers(ctx, gen.OrderedUsersParams{Limit: 100})
	die(err)
	expect(ord[0].ID < ord[1].ID, "default sort by id")
	// Duplicate key selection errors before touching the DB.
	_, err = q.OrderedUsers(ctx, gen.OrderedUsersParams{
		Sort:  []gen.OrderedUsersSortKey{gen.OrderedUsersSortEmailAsc, gen.OrderedUsersSortEmailDesc},
		Limit: 1,
	})
	expect(err != nil, "duplicate order key rejected")

	// @filter-tree!: typed values cross the layer boundary, never SQL.
	// Required mode: the scope is an argument of a value type, so both
	// omitting it and passing nil fail to compile; the zero Tree is the
	// only way left to arrive without a decision, and it errors before
	// the DB. Unscoped is the explicit opt-out; And/Or compose the
	// closed vocabulary.
	_, err = q.FilterUsers(ctx, sqletchruntime.Tree{}, gen.FilterUsersParams{Limit: 100})
	expect(errors.Is(err, sqletchruntime.ErrFilterRequired), "zero tree rejected by @filter-tree!")

	unscoped, err := q.FilterUsers(ctx, gen.FilterUsersUnscoped(), gen.FilterUsersParams{Limit: 100})
	die(err)
	expect(len(unscoped) == 5, "Unscoped sees everyone")

	scoped, err := q.FilterUsers(ctx,
		gen.And(gen.FilterUsersTenant(1), gen.FilterUsersStatusEq("active")),
		gen.FilterUsersParams{Limit: 100})
	die(err)
	expect(len(scoped) == 4, "tenant AND active")

	either, err := q.FilterUsers(ctx,
		gen.Or(gen.FilterUsersStatusEq("banned"), gen.FilterUsersEmailPrefix("alice")),
		gen.FilterUsersParams{Limit: 100})
	die(err)
	expect(len(either) == 2, "banned OR alice*")

	// @in: a slice parameter crosses as one array bind (= ANY($n)).
	// Empty slice means "matches nothing", never "matches everything".
	inBoth, err := q.UsersInStatuses(ctx, gen.UsersInStatusesParams{
		TenantID: 1,
		Statuses: []string{"active", "banned"},
		Limit:    100,
	})
	die(err)
	expect(len(inBoth) == 5, "both statuses match every tenant-1 user")
	inBanned, err := q.UsersInStatuses(ctx, gen.UsersInStatusesParams{
		TenantID: 1,
		Statuses: []string{"banned"},
		Limit:    100,
	})
	die(err)
	expect(len(inBanned) == 1 && inBanned[0].Email == "carol@example.com", "single status")
	inNone, err := q.UsersInStatuses(ctx, gen.UsersInStatusesParams{
		TenantID: 1,
		Statuses: []string{},
		Limit:    100,
	})
	die(err)
	expect(len(inNone) == 0, "empty list matches nothing")
	inGuarded, err := q.UsersInStatuses(ctx, gen.UsersInStatusesParams{
		TenantID: 1,
		Statuses: []string{"active", "banned"},
		MinID:    optional.Some(int64(3)),
		Limit:    100,
	})
	die(err)
	expect(len(inGuarded) == 3 && inGuarded[0].ID == 3, "@in composes with a guarded conjunct")

	// Cursor pagination across two shapes.
	page1, err := q.ListAuditLogs(ctx, gen.ListAuditLogsParams{TenantID: 1, Limit: 2})
	die(err)
	expect(len(page1) == 2, "first page")
	page2, err := q.ListAuditLogs(ctx, gen.ListAuditLogsParams{
		TenantID: 1,
		AfterID:  optional.Some(page1[len(page1)-1].ID),
		Limit:    2,
	})
	die(err)
	expect(len(page2) == 1, "second page")
	expect(page1[1].ActorID.IsNone() || page2[0].ActorID.IsNone(), "NULL actor scans somewhere")

	expect(shapes >= 7, "OnQuery hook observed the calls")
	fmt.Println("E2E-OK")
}
`
