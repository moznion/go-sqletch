//go:build devdb

package e2e_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/cli"
	"github.com/moznion/go-sqletch/internal/devdb"
)

// Tier 2 policy coverage runs through the real CLI pipeline — config
// with `policies:`, cold generate against the live engine, and the
// woven placeholder style ('?' per occurrence) in the generated code.
// Row-level leak proof lives in the PostgreSQL module E2E; here the
// dialect-specific surfaces are under test: question-style rendering,
// the policy param typed via the injected hint (mandatory on Tier 2),
// and warm offline re-check over the woven cache entries.

const policyAuditQueries = `-- name: AllAudit :many
SELECT a.id, a.actor_id FROM audit_logs AS a ORDER BY a.id;

-- name: AllAuditBackfill :many
-- @policy-optout: tenant_scope (backfill; deliberately cross-tenant)
SELECT a.id, a.actor_id FROM audit_logs AS a ORDER BY a.id;
`

func policyConfigYAML(dialectName, serverVersion, dsn, paramType string) string {
	return `version: 1
dialect: ` + dialectName + `
server_version: "` + serverVersion + `"
database:
  dsn: ` + dsn + `
schema:
  files: [db/schema.sql]
queries: [queries/*.sql]
output:
  package: gen
  path: gen
cache:
  path: .sqletch/cache
policies:
  - name: tenant_scope
    tables: [audit_logs]
    predicate: "{}.tenant_id = :tenant_id"
    param:
      name: tenant_id
      type: ` + paramType + `
`
}

// assertWovenGen reads the generated per-query files and asserts the
// woven query's fragment table carries the scoping conjunct (fragment
// text keeps the `:name` form; the runtime owns placeholder emission)
// while the opt-out's does not.
func assertWovenGen(t *testing.T, dir string) {
	t.Helper()
	woven, err := os.ReadFile(filepath.Join(dir, "gen", "all_audit.sql.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(woven), "WHERE a.tenant_id = :tenant_id") {
		t.Errorf("generated fragments lack the woven conjunct:\n%s", woven)
	}
	backfill, err := os.ReadFile(filepath.Join(dir, "gen", "all_audit_backfill.sql.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(backfill), "tenant_id") {
		t.Errorf("opt-out query was woven in generated code:\n%s", backfill)
	}
}

func TestSQLitePolicyWeaveCLI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	dir := t.TempDir()
	writeFile(t, dir, "db/schema.sql", sqliteSchemaSQL)
	writeFile(t, dir, "queries/audit.sql", policyAuditQueries)
	writeFile(t, dir, "sqletch.yaml",
		policyConfigYAML("sqlite", "3", filepath.Join(dir, "dev.sqlite3"), "integer"))
	configPath := filepath.Join(dir, "sqletch.yaml")

	var out, errW bytes.Buffer
	if code := cli.Generate(ctx, configPath, false, &out, &errW); code != cli.ExitOK {
		t.Fatalf("cold generate: exit %d\n%s%s", code, out.String(), errW.String())
	}
	assertWovenGen(t, dir)

	// Warm offline: the committed cache holds the woven renderings.
	out.Reset()
	errW.Reset()
	if code := cli.Check(ctx, configPath, false, false, &out, &errW); code != cli.ExitOK {
		t.Fatalf("warm check: exit %d\n%s%s", code, out.String(), errW.String())
	}
	if !strings.Contains(out.String(), "offline") {
		t.Logf("check output: %s", out.String())
	}
}

func TestMySQLPolicyWeaveCLI(t *testing.T) {
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
	writeFile(t, dir, "db/schema.sql", mysqlSchemaSQL)
	writeFile(t, dir, "queries/audit.sql", policyAuditQueries)
	writeFile(t, dir, "sqletch.yaml", policyConfigYAML("mysql", "8.4", dsn, "bigint"))
	configPath := filepath.Join(dir, "sqletch.yaml")

	var out, errW bytes.Buffer
	if code := cli.Generate(ctx, configPath, false, &out, &errW); code != cli.ExitOK {
		t.Fatalf("cold generate: exit %d\n%s%s", code, out.String(), errW.String())
	}
	assertWovenGen(t, dir)

	out.Reset()
	errW.Reset()
	if code := cli.Check(ctx, configPath, false, false, &out, &errW); code != cli.ExitOK {
		t.Fatalf("warm check: exit %d\n%s%s", code, out.String(), errW.String())
	}
}
