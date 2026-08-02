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
	"github.com/moznion/go-sqletch/internal/policy"
	"github.com/moznion/go-sqletch/internal/rules"
)

// The queries of the policy E2E: one with no tenant filter at all —
// the query that would leak without weaving — one explicit opt-out,
// and the hand-scoped corpus query (idempotence: not double-woven).
const allAudit = `-- name: AllAudit :many
SELECT a.id, a.action FROM audit_logs AS a ORDER BY a.id;
`

const allAuditBackfill = `-- name: AllAuditBackfill :many
-- @policy-optout: tenant_scope (backfill; deliberately cross-tenant)
SELECT a.id, a.action FROM audit_logs AS a ORDER BY a.id;
`

// The D2(a) case: audit_logs on the null-extended side of a LEFT
// JOIN. The conjunct must land in the ON clause — every user row
// stays present, but only the tenant's audit rows join.
const usersWithAudit = `-- name: UsersWithAudit :many
SELECT u.id, a.action
FROM users AS u
LEFT JOIN audit_logs AS a ON a.actor_id = u.id
ORDER BY u.id, a.id;
`

func tenantScopePolicy() policy.Policy {
	return policy.Policy{
		Name:      "tenant_scope",
		Tables:    []string{"audit_logs"},
		Predicate: "{}.tenant_id = :tenant_id",
		ParamName: "tenant_id",
		ParamType: "bigint",
	}
}

// TestPolicyWeavingEndToEnd is the test that would catch a regression
// in the feature's purpose: with a two-tenant dataset, the woven query
// must not return the other tenant's rows (the unwoven one would),
// the opt-out legitimately sees both, and a hand-scoped query is not
// double-woven. The woven templates flow through the full loop:
// oracle Describe, enforcement, codegen, a compiled module, real
// execution.
func TestPolicyWeavingEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn, cleanup, err := devdb.AcquireDSN(ctx, devdb.Config{
		DSN:              os.Getenv("SQLETCH_TEST_DSN"),
		AllowDestructive: true,
		ServerVersion:    "16",
		SchemaSQL:        []string{schemaSQL},
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

	pols := []policy.Policy{tenantScopePolicy()}
	if d := policy.Validate(postgres.Profile{}, postgres.Frontend{}, pols, "sqletch.yaml"); len(d) != 0 {
		t.Fatalf("policy validation: %+v", d)
	}

	var inputs []codegen.QueryInput
	for _, src := range []string{allAudit, allAuditBackfill, usersWithAudit, corpus["list_audit_logs"]} {
		q := compile(t, src)
		if d := rules.CheckLexical(postgres.Profile{}, q); len(d) != 0 {
			t.Fatalf("lexical: %+v", d)
		}
		wres := policy.Weave(postgres.Profile{}, postgres.Frontend{}, pols, q)
		if len(wres.Diags) != 0 {
			t.Fatalf("%s: weave: %+v", q.Name, wres.Diags)
		}
		wq := wres.Query
		rs, err := ast.Renderings(postgres.Profile{}, wq)
		if err != nil {
			t.Fatal(err)
		}
		if d := rules.CheckR1(postgres.Profile{}, postgres.Frontend{}, wq, rs); len(d) != 0 {
			t.Fatalf("%s: R1 on woven: %+v", q.Name, d)
		}

		// The enforcement pass must accept every woven result.
		tree, err := postgres.Frontend{}.Parse(rs[0].SQL)
		if err != nil {
			t.Fatal(err)
		}
		if d := policy.Enforce(postgres.Profile{}, postgres.Frontend{}, pols, wq, tree, rs[0]); len(d) != 0 {
			t.Fatalf("%s: enforcement rejects the woven template: %+v", q.Name, d)
		}

		switch q.Name {
		case "AllAudit":
			if !strings.Contains(rs[0].SQL, "WHERE a.tenant_id = $1") {
				t.Fatalf("AllAudit not woven:\n%s", rs[0].SQL)
			}
		case "AllAuditBackfill":
			if strings.Contains(rs[0].SQL, "tenant_id") {
				t.Fatalf("opt-out was woven anyway:\n%s", rs[0].SQL)
			}
		case "UsersWithAudit":
			if !strings.Contains(rs[0].SQL, "ON a.actor_id = u.id AND a.tenant_id = $1") {
				t.Fatalf("outer-join occurrence not woven into the ON clause:\n%s", rs[0].SQL)
			}
		case "ListAuditLogs":
			if n := strings.Count(rs[0].SQL, "tenant_id ="); n != 1 {
				t.Fatalf("hand-scoped query has %d tenant conjuncts (double-weave?):\n%s", n, rs[0].SQL)
			}
		}

		var descs []dialect.Desc
		for _, r := range rs {
			d, err := oracle.Describe(ctx, r.SQL)
			if err != nil {
				t.Fatalf("%s: describe: %v", q.Name, err)
			}
			descs = append(descs, d)
		}
		if d := rules.CheckTypeAgreement(wq, rs, descs); len(d) != 0 {
			t.Fatalf("%s: agreement: %+v", q.Name, d)
		}
		types, d := rules.ResolveParamTypes(wq, rs, descs)
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
			Q:          wq,
			Frags:      codegen.BuildFrags(postgres.Profile{}, wq),
			ParamTypes: typeMap,
			Columns:    descs[0].Columns,
			Nullable:   nullable,
		})
	}

	files, diags := codegen.Generate(codegen.Options{Package: "gen"}, postgres.TypeMap{}, inputs)
	if len(diags) != 0 {
		t.Fatalf("generate: %+v", diags)
	}

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
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(policyE2EMain), 0o644); err != nil {
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
	if !strings.Contains(out, "POLICY-E2E-OK") {
		t.Fatalf("generated module run did not report success:\n%s", out)
	}
}

const policyE2EMain = `package main

import (
	"context"
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

	// Two tenants; tenant 2's rows are the ones a leak would expose.
	// alice (user 1) has audit rows in BOTH tenants — the ON-weave
	// case must keep her row while hiding the cross-tenant action.
	_, err = conn.Exec(ctx, ` + "`" + `
		TRUNCATE users, organization_users, audit_logs RESTART IDENTITY;
		INSERT INTO users (email, status, tenant_id) VALUES
			('alice@example.com', 'active', 1),
			('bob@example.com',   'active', 1);
		INSERT INTO audit_logs (tenant_id, actor_id, action) VALUES
			(1, 1, 'login'), (1, NULL, 'cron'), (2, 9, 'secret'), (2, 1, 'crossed');
	` + "`" + `)
	die(err)

	q := gen.New(conn)

	// The woven query is tenant-scoped even though its template never
	// mentioned tenants, and the value it scopes by is an argument of
	// the distinct type gen.TenantID: a call that forgets it does not
	// compile, and neither would swapping it with another policy's
	// same-underlying-typed argument.
	tenant1 := gen.TenantID(1)
	t1, err := q.AllAudit(ctx, tenant1, gen.AllAuditParams{})
	die(err)
	expect(len(t1) == 2, "tenant 1 sees exactly its two rows")
	for _, r := range t1 {
		expect(r.Action != "secret", "tenant 2's row must not leak")
	}
	t2, err := q.AllAudit(ctx, gen.TenantID(2), gen.AllAuditParams{})
	die(err)
	expect(len(t2) == 2 && t2[0].Action == "secret" && t2[1].Action == "crossed", "tenant 2 sees its rows")

	// The opt-out legitimately crosses tenants.
	all, err := q.AllAuditBackfill(ctx, gen.AllAuditBackfillParams{})
	die(err)
	expect(len(all) == 4, "opt-out sees every tenant")

	// ON-woven outer join: every user row survives (bob has no audit
	// rows and still appears), and alice's cross-tenant action is
	// invisible — a WHERE-placed conjunct would have dropped bob, an
	// unwoven query would have shown 'crossed'.
	rows, err := q.UsersWithAudit(ctx, gen.UsersWithAuditParams{TenantID: 1})
	die(err)
	expect(len(rows) == 2, "alice (login) + bob's null-extended row; cron is actorless")
	sawBob, sawCrossed := false, false
	for _, r := range rows {
		if r.ID == 2 {
			sawBob = true
			expect(r.Action == nil, "bob's outer row is null-extended")
		}
		if r.Action != nil && *r.Action == "crossed" {
			sawCrossed = true
		}
	}
	expect(sawBob, "outer row preserved by the ON conjunct")
	expect(!sawCrossed, "cross-tenant action must not leak through the join")

	// The hand-scoped query still paginates as written.
	page, err := q.ListAuditLogs(ctx, gen.ListAuditLogsParams{TenantID: 1, Limit: 10})
	die(err)
	expect(len(page) == 2, "hand-scoped query unchanged")

	fmt.Println("POLICY-E2E-OK")
}
`
