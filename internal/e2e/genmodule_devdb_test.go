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

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/codegen"
	"github.com/moznion/sqletch/internal/devdb"
	"github.com/moznion/sqletch/internal/dialect"
	"github.com/moznion/sqletch/internal/dialect/postgres"
	"github.com/moznion/sqletch/internal/nullability"
	"github.com/moznion/sqletch/internal/rules"
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

	// Full pipeline for the corpus + the :one nullable-columns query.
	var inputs []codegen.QueryInput
	for _, src := range []string{corpus["search_users"], corpus["list_audit_logs"], getUserProfile} {
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
		tree, err := postgres.Frontend{}.Parse(rs[0].SQL)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, codegen.QueryInput{
			Q:          q,
			Frags:      codegen.BuildFrags(postgres.Profile{}, q),
			ParamTypes: typeMap,
			Columns:    descs[0].Columns,
			Nullable:   nullability.Analyze(tree, rs[0], descs[0], cat, nil),
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
	goMod := "module sqletchgen\n\ngo 1.24\n\nrequire (\n" +
		"\tgithub.com/jackc/pgx/v5 " + pgxVer[1] + "\n" +
		"\tgithub.com/moznion/sqletch v0.0.0\n)\n\n" +
		"replace github.com/moznion/sqletch => " + repoRoot + "\n"
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

	"github.com/jackc/pgx/v5"

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
		Status: gen.Ptr("active"),
		Sort:   gen.SearchUsersSortEmailAsc,
		Limit:  100,
	})
	die(err)
	expect(len(active) == 2 && active[0].Email == "alice@example.com", "active users sorted by email")

	org, err := q.SearchUsers(ctx, gen.SearchUsersParams{
		OrganizationID: gen.Ptr(int64(77)),
		Limit:          100,
	})
	die(err)
	expect(len(org) == 1 && org[0].ID == 1, "org-scoped join shape")

	// NULL-heavy scan through the generated :one function.
	bob, err := q.GetUserProfile(ctx, gen.GetUserProfileParams{ID: 2})
	die(err)
	expect(bob.Nickname == nil && bob.OrgID == nil, "bob's NULLs scan into nil pointers")
	alice, err := q.GetUserProfile(ctx, gen.GetUserProfileParams{ID: 1})
	die(err)
	expect(alice.Nickname != nil && *alice.Nickname == "al", "alice's nickname")

	_, err = q.GetUserProfile(ctx, gen.GetUserProfileParams{ID: 999})
	expect(errors.Is(err, pgx.ErrNoRows), "missing row yields pgx.ErrNoRows")

	// Cursor pagination across two shapes.
	page1, err := q.ListAuditLogs(ctx, gen.ListAuditLogsParams{TenantID: 1, Limit: 2})
	die(err)
	expect(len(page1) == 2, "first page")
	page2, err := q.ListAuditLogs(ctx, gen.ListAuditLogsParams{
		TenantID: 1,
		AfterID:  gen.Ptr(page1[len(page1)-1].ID),
		Limit:    2,
	})
	die(err)
	expect(len(page2) == 1, "second page")
	expect(page1[1].ActorID == nil || page2[0].ActorID == nil, "NULL actor scans somewhere")

	expect(shapes >= 7, "OnQuery hook observed the calls")
	fmt.Println("E2E-OK")
}
`
