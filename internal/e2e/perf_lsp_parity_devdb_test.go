//go:build devdb

package e2e_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/cli"
	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/devdb"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// The editor must show exactly the lint findings the CLI does. The LSP
// never opens a database: its SQLETCHL005 comes from the committed
// cache a real `generate` wrote, through the same resolvedChecks seam.
// So: generate against the real server with `lint: true`, then run the
// offline checker over the same project and require the same lint
// findings (code + template span) — SQLETCHL005 included, which proves
// the offline catalog pass actually ran.
func lintParity(t *testing.T, ctx context.Context, cfgPath string) {
	t.Helper()
	cfg, diags := config.Load(cfgPath)
	if diagnostics.HasErrors(diags) {
		t.Fatalf("config: %v", diags)
	}
	if !cfg.Lint {
		t.Fatal("precondition: the project sets lint: true")
	}
	res, err := cli.Run(ctx, cfg, cli.ModeGenerate, cli.RunOptions{AllowDestructive: true})
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("generate: %v", res.Diags)
	}
	off, err := cli.NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	var offDiags []diagnostics.Diagnostic
	for _, ds := range off.Diags {
		offDiags = append(offDiags, ds...)
	}
	cliLints, lspLints := lintKeys(res.Diags), lintKeys(offDiags)
	if !slices.Equal(cliLints, lspLints) {
		t.Errorf("editor and CLI disagree:\n cli: %q\n lsp: %q", cliLints, lspLints)
	}
	if !slices.ContainsFunc(cliLints, func(k string) bool { return strings.HasPrefix(k, string(diagnostics.CodePerfTypeMismatch)) }) {
		t.Errorf("fixture must exercise SQLETCHL005 (the catalog-dependent lint): %q", cliLints)
	}
}

// lintKeys renders each SQLETCHLnnn finding as code@file:start-end,
// sorted, so the two sides compare as multisets.
func lintKeys(diags []diagnostics.Diagnostic) []string {
	var out []string
	for _, d := range diags {
		if !strings.HasPrefix(string(d.Code), "SQLETCHL") {
			continue
		}
		out = append(out, string(d.Code)+"@"+filepath.Base(d.Span.File)+":"+strconv.Itoa(d.Span.Start)+"-"+strconv.Itoa(d.Span.End))
	}
	slices.Sort(out)
	return out
}

const parityQueries = `-- name: ByLowerEmail :many
SELECT u.id FROM users AS u WHERE lower(u.email) = :email;

-- name: ByIdNumeric :one
SELECT u.id FROM users AS u WHERE u.id = :id::numeric;

-- name: Paged :many
SELECT u.id FROM users AS u ORDER BY u.id LIMIT :limit OFFSET :off;

-- name: Stale :one
-- @nolint SQLETCHL002
SELECT u.id FROM users AS u WHERE u.id = :id;
`

func TestLintParityPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn, cleanup, err := devdb.AcquireDSN(ctx, devdb.Config{
		DSN:              os.Getenv("SQLETCH_TEST_DSN"),
		AllowDestructive: true,
		ServerVersion:    "16",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	dir := t.TempDir()
	writeFile(t, dir, "db/schema.sql", "CREATE TABLE users (id bigint PRIMARY KEY, email text NOT NULL);\n")
	writeFile(t, dir, "queries/q.sql", parityQueries)
	writeFile(t, dir, "sqletch.yaml", `version: 1
dialect: postgres
server_version: "16"
database:
  dsn: `+dsn+`
schema:
  files: [db/schema.sql]
targets:
  - queries: [queries/*.sql]
    output:
      package: gen
      path: gen
lint: true
`)
	lintParity(t, ctx, filepath.Join(dir, "sqletch.yaml"))
}

func TestLintParityMySQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn, cleanup, err := devdb.AcquireMySQLDSN(ctx, devdb.Config{
		DSN:              os.Getenv("SQLETCH_TEST_MYSQL_DSN"),
		AllowDestructive: true,
		ServerVersion:    "8.4",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	dir := t.TempDir()
	writeFile(t, dir, "db/schema.sql", "CREATE TABLE users (id BIGINT PRIMARY KEY, code VARCHAR(16) NOT NULL, email VARCHAR(64) NOT NULL);\n")
	writeFile(t, dir, "queries/q.sql", `-- name: ByLowerEmail :many
-- @param email: varchar(64)
SELECT u.id FROM users AS u WHERE lower(u.email) = :email;

-- name: ByCodeNumber :one
-- @param code: bigint
SELECT u.id FROM users AS u WHERE u.code = :code;

-- name: Paged :many
-- @param lim: bigint
-- @param off: bigint
SELECT u.id FROM users AS u ORDER BY u.id LIMIT :off, :lim;
`)
	writeFile(t, dir, "sqletch.yaml", `version: 1
dialect: mysql
server_version: "8.4"
database:
  dsn: `+dsn+`
schema:
  files: [db/schema.sql]
targets:
  - queries: [queries/*.sql]
    output:
      package: gen
      path: gen
lint: true
`)
	lintParity(t, ctx, filepath.Join(dir, "sqletch.yaml"))
}
