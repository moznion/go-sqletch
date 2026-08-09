//go:build devdb

package e2e_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/cli"
	"github.com/moznion/go-sqletch/internal/devdb"
)

// TestMySQLCLIAndGeneratedModule is the MySQL full loop: the real CLI
// pipeline (dialect: mysql) generates a package cold, goes offline
// once the cache is committed, and the generated database/sql code
// runs against the live server with NULL-heavy seed data — @in
// arities, PATCH semantics, @when/HAVING, @order-by, and
// @filter-tree all through the public API.
func TestMySQLCLIAndGeneratedModule(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	dsn, cleanup, err := devdb.AcquireMySQLDSN(ctx, devdb.Config{
		DSN:           os.Getenv("SQLETCH_TEST_MYSQL_DSN"),
		ServerVersion: "8.4",
		SchemaSQL:     nil, // schema applied by the CLI from config
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	dir := t.TempDir()
	var queriesSrc strings.Builder
	for _, name := range []string{"search_users", "in_list", "update_user_profile", "create_user", "when_and_having", "order_by_users", "filter_tree"} {
		queriesSrc.WriteString(mysqlCorpus[name])
		queriesSrc.WriteString("\n")
	}
	queriesSrc.WriteString(mysqlFindUserByEmail)
	writeFile(t, dir, "db/schema.sql", mysqlSchemaSQL)
	writeFile(t, dir, "queries/users.sql", queriesSrc.String())
	writeConfig := func(dsn string) {
		writeFile(t, dir, "sqletch.yaml", `version: 1
dialect: mysql
server_version: "8.4"
database:
  dsn: `+dsn+`
schema:
  files: [db/schema.sql]
queries: [queries/*.sql]
output:
  package: gen
  path: gen
cache:
  path: .sqletch/cache
`)
	}
	writeConfig(dsn)
	configPath := filepath.Join(dir, "sqletch.yaml")

	// 1. Cold generate: needs the DB, fills cache and gen/.
	var out, errW bytes.Buffer
	if code := cli.Generate(ctx, configPath, false, cli.RunOptions{}, &out, &errW); code != cli.ExitOK {
		t.Fatalf("cold generate: exit %d\n%s%s", code, out.String(), errW.String())
	}
	if !strings.Contains(out.String(), "offline: no") {
		t.Errorf("cold generate must not be offline: %s", out.String())
	}
	inGen := readFile(t, dir, "gen/users_in_statuses.sql.gen.go")
	for _, want := range []string{`Statuses\s+\[\]string`, `runtime\.InList`, `QueryContext`} {
		if !regexp.MustCompile(want).MatchString(inGen) {
			t.Errorf("generated @in query missing %q:\n%s", want, inGen)
		}
	}
	if db := readFile(t, dir, "gen/db.gen.go"); !strings.Contains(db, `"database/sql"`) {
		t.Errorf("db.go must be the database/sql flavor:\n%s", db)
	}

	// 2. Warm check with an unreachable DSN proves offline capability.
	writeConfig("nobody:nothing@tcp(unreachable.invalid:1)/nope")
	out.Reset()
	errW.Reset()
	if code := cli.Check(ctx, configPath, false, false, cli.RunOptions{}, &out, &errW); code != cli.ExitOK {
		t.Fatalf("warm offline check: exit %d\n%s%s", code, out.String(), errW.String())
	}
	if !strings.Contains(out.String(), "offline: yes") {
		t.Errorf("warm check must report offline: %s", out.String())
	}
	writeConfig(dsn)

	// 3. A missing annotation is a diagnostic, not an env error.
	writeFile(t, dir, "queries/broken.sql",
		"-- name: Broken :many\nSELECT u.id FROM users AS u WHERE u.id = :id;\n")
	out.Reset()
	errW.Reset()
	if code := cli.Check(ctx, configPath, false, false, cli.RunOptions{}, &out, &errW); code != cli.ExitDiagnostics {
		t.Fatalf("missing annotation: exit %d\n%s%s", code, out.String(), errW.String())
	}
	if !strings.Contains(errW.String(), "@param id") {
		t.Errorf("diagnostic must name the missing annotation: %s", errW.String())
	}
	if err := os.Remove(filepath.Join(dir, "queries/broken.sql")); err != nil {
		t.Fatal(err)
	}

	// ---- run the generated module against the live server ---------------
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	parentMod, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	drvVer := regexp.MustCompile(`github\.com/go-sql-driver/mysql (v[0-9A-Za-z.\-+]+)`).FindStringSubmatch(string(parentMod))
	if drvVer == nil {
		t.Fatal("go-sql-driver version not found in parent go.mod")
	}
	optVer := regexp.MustCompile(`github\.com/moznion/go-optional (v[0-9A-Za-z.\-+]+)`).FindStringSubmatch(string(parentMod))
	if optVer == nil {
		t.Fatal("go-optional version not found in parent go.mod")
	}
	goMod := "module sqletchgen\n\ngo 1.24\n\nrequire (\n" +
		"\tgithub.com/go-sql-driver/mysql " + drvVer[1] + "\n" +
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
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mysqlE2EMain), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "SQLETCH_TEST_MYSQL_DSN="+dsn+"?parseTime=true")
		cmdOut, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go %s failed: %v\n%s", strings.Join(args, " "), err, cmdOut)
		}
		return string(cmdOut)
	}
	run("mod", "tidy")
	if runOut := run("run", "."); !strings.Contains(runOut, "E2E-OK") {
		t.Fatalf("generated module run did not report success:\n%s", runOut)
	}
}

const mysqlFindUserByEmail = `-- name: FindUserByEmail :maybe-one
-- @param email: varchar(255)
SELECT u.id, u.email, u.nickname
FROM users AS u
WHERE u.email = :email;
`

const mysqlE2EMain = `package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	_ "github.com/go-sql-driver/mysql"

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
	db, err := sql.Open("mysql", os.Getenv("SQLETCH_TEST_MYSQL_DSN"))
	die(err)
	defer db.Close()

	for _, stmt := range []string{
		"TRUNCATE users",
		"TRUNCATE organization_users",
		"TRUNCATE audit_logs",
		"INSERT INTO users (email, status, tenant_id, org_id, nickname) VALUES" +
			" ('alice@example.com', 'active', 1, 10, 'アリス')," +
			" ('bob@example.com',   'active', 1, NULL, NULL)," +
			" ('carol@example.com', 'banned', 1, NULL, NULL)",
		"INSERT INTO organization_users (user_id, organization_id) VALUES (1, 77)",
		"INSERT INTO audit_logs (tenant_id, actor_id, action) VALUES" +
			" (1, 1, 'login'), (1, NULL, 'cron'), (1, 2, 'logout')",
	} {
		_, err := db.ExecContext(ctx, stmt)
		die(err)
	}

	q := gen.New(db)
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

	// @in arity expansion: 2, 1, 0 elements and a guarded conjunct.
	inBoth, err := q.UsersInStatuses(ctx, gen.UsersInStatusesParams{
		TenantID: 1,
		Statuses: []string{"active", "banned"},
		Limit:    100,
	})
	die(err)
	expect(len(inBoth) == 3, "two-element list matches everyone")
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
		MinID:    optional.Some(int64(2)),
		Limit:    100,
	})
	die(err)
	expect(len(inGuarded) == 2 && inGuarded[0].ID == 2, "@in composes with a guarded conjunct")

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

	// PATCH semantics through :execrows; verify via a direct query.
	n, err := q.UpdateUserProfile(ctx, gen.UpdateUserProfileParams{
		ID:       1,
		Nickname: optional.Some("allie"),
	})
	die(err)
	expect(n == 1, "one row patched")
	var email, nickname string
	var bio *string
	die(db.QueryRowContext(ctx, "SELECT email, nickname, bio FROM users WHERE id = 1").Scan(&email, &nickname, &bio))
	expect(nickname == "allie", "nickname updated")
	expect(email == "alice@example.com", "email untouched by partial update")
	expect(bio == nil, "bio untouched (still NULL)")

	// Optional INSERT pairs.
	n, err = q.CreateUser(ctx, gen.CreateUserParams{
		Email:    "dave@example.com",
		Status:   "active",
		TenantID: 1,
	})
	die(err)
	expect(n == 1, "insert without optional pair")
	var nick *string
	die(db.QueryRowContext(ctx, "SELECT nickname FROM users WHERE email = 'dave@example.com'").Scan(&nick))
	expect(nick == nil, "omitted optional column defaults to NULL")

	// @when value guard + HAVING conjunct.
	act, err := q.TenantActivity(ctx, gen.TenantActivityParams{IncludeCron: true})
	die(err)
	expect(len(act) == 1 && act[0].Actions == 3, "all actions incl. cron")
	act, err = q.TenantActivity(ctx, gen.TenantActivityParams{IncludeCron: false})
	die(err)
	expect(len(act) == 1 && act[0].Actions == 2, "@when guard drops the NULL-actor row")
	act, err = q.TenantActivity(ctx, gen.TenantActivityParams{IncludeCron: true, MinActions: optional.Some(int64(99))})
	die(err)
	expect(len(act) == 0, "HAVING conjunct filters the group out")

	// @order-by permutation, default, and duplicate rejection.
	ord, err := q.OrderedUsers(ctx, gen.OrderedUsersParams{
		Sort:  []gen.OrderedUsersSortKey{gen.OrderedUsersSortEmailDesc},
		Limit: 100,
	})
	die(err)
	expect(len(ord) == 4 && ord[0].Email > ord[len(ord)-1].Email, "email DESC")
	ord, err = q.OrderedUsers(ctx, gen.OrderedUsersParams{Limit: 100})
	die(err)
	expect(ord[0].ID < ord[1].ID, "default sort by id")
	_, err = q.OrderedUsers(ctx, gen.OrderedUsersParams{
		Sort:  []gen.OrderedUsersSortKey{gen.OrderedUsersSortEmailAsc, gen.OrderedUsersSortEmailDesc},
		Limit: 1,
	})
	expect(err != nil, "duplicate order key rejected")

	// @filter-tree!: required, unscoped, composed.
	// Omitting the scope and passing nil are both compile errors now;
	// the zero Tree is the only undecided value that reaches here.
	_, err = q.FilterUsers(ctx, sqletchruntime.Tree{}, gen.FilterUsersParams{Limit: 100})
	expect(errors.Is(err, sqletchruntime.ErrFilterRequired), "zero tree rejected by @filter-tree!")
	unscoped, err := q.FilterUsers(ctx, gen.FilterUsersUnscoped(), gen.FilterUsersParams{Limit: 100})
	die(err)
	expect(len(unscoped) == 4, "Unscoped sees everyone")
	scoped, err := q.FilterUsers(ctx, gen.And(gen.FilterUsersTenant(1), gen.FilterUsersStatusEq("active")),
		gen.FilterUsersParams{Limit: 100})
	die(err)
	expect(len(scoped) == 3, "tenant AND active")
	either, err := q.FilterUsers(ctx, gen.Or(gen.FilterUsersStatusEq("banned"), gen.FilterUsersEmailPrefix("alice")),
		gen.FilterUsersParams{Limit: 100})
	die(err)
	expect(len(either) == 2, "banned OR alice*")

	expect(shapes >= 8, "OnQuery hook observed the calls")
	fmt.Println("E2E-OK")
}
`
