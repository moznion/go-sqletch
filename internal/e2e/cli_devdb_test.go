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
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// TestCLIColdWarmRoundTrip is the DX flagship (design 04/07): a cold
// generate needs the database; once the cache is committed, check and
// generate run fully offline — proven by handing them an unreachable
// DSN and watching them succeed.
func TestCLIColdWarmRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn, cleanup, err := devdb.AcquireDSN(ctx, devdb.Config{
		DSN:           os.Getenv("SQLETCH_TEST_DSN"),
		ServerVersion: "16",
		SchemaSQL:     nil, // schema applied by the CLI from config
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Project fixture: a mini examples/ clone.
	dir := t.TempDir()
	writeFile(t, dir, "db/schema.sql", cliSchema)
	writeFile(t, dir, "queries/users.sql", cliQuery)
	baseConfig := func(dsn string) string {
		return `version: 1
dialect: postgres
server_version: "16"
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
static_expansion:
  queries: [SearchUsers]
`
	}
	writeConfig := func(dsn string) {
		writeFile(t, dir, "sqletch.yaml", baseConfig(dsn))
	}
	writeConfig(dsn)
	configPath := filepath.Join(dir, "sqletch.yaml")

	runCLI := func(mode string, exhaustive bool) (int, string, string) {
		var out, errW bytes.Buffer
		var code int
		switch mode {
		case "generate":
			code = cli.Generate(ctx, configPath, false, cli.RunOptions{}, &out, &errW)
		case "check":
			code = cli.Check(ctx, configPath, exhaustive, false, cli.RunOptions{}, &out, &errW)
		}
		return code, out.String(), errW.String()
	}

	// 1. Cold generate: needs the DB, fills cache and gen/.
	code, out, errOut := runCLI("generate", false)
	if code != cli.ExitOK {
		t.Fatalf("cold generate: exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "offline: no") {
		t.Errorf("cold generate must not be offline: %s", out)
	}
	for _, f := range []string{"gen/db.gen.go", "gen/querier.gen.go", "gen/search_users.sql.gen.go"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("missing generated %s: %v", f, err)
		}
	}
	genBefore := readFile(t, dir, "gen/search_users.sql.gen.go")

	// The @param hint fed the pipeline: the @in parameter is a plain
	// slice in the generated params struct.
	if inGen := readFile(t, dir, "gen/users_in_statuses.sql.gen.go"); !strings.Contains(inGen, "Statuses []string") {
		t.Errorf("@in param must generate a slice field:\n%s", inGen)
	}

	// Static expansion: the audit .sql files exist (8 shapes: 2 guards
	// x 2 sort cases) and the generated code dispatches via the
	// precomposed shape table instead of the composer.
	expanded, err := filepath.Glob(filepath.Join(dir, ".sqletch/expanded/SearchUsers/*.sql"))
	if err != nil || len(expanded) != 8 {
		t.Fatalf("expanded shape files = %d (%v), want 8", len(expanded), err)
	}
	if !strings.Contains(genBefore, "searchUsersShapes") || strings.Contains(genBefore, "searchUsersFrags") {
		t.Errorf("expanded query must dispatch via the shape table:\n%s", genBefore)
	}

	// 2. Warm check with an UNREACHABLE DSN: success proves the cache
	// made it fully offline.
	writeConfig("postgres://unreachable.invalid:1/nope")
	code, out, errOut = runCLI("check", false)
	if code != cli.ExitOK {
		t.Fatalf("warm offline check: exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "offline: yes") {
		t.Errorf("warm check must report offline: %s", out)
	}

	// 3. Warm generate is offline too, and byte-stable.
	code, _, errOut = runCLI("generate", false)
	if code != cli.ExitOK {
		t.Fatalf("warm generate: exit %d\n%s", code, errOut)
	}
	if got := readFile(t, dir, "gen/search_users.sql.gen.go"); got != genBefore {
		t.Error("warm regenerate changed the output (determinism violated)")
	}

	// 4. Query change invalidates the cache: with the unreachable DSN
	// this becomes an environment error (needs the DB again).
	writeFile(t, dir, "queries/users.sql", cliQuery+"\n-- name: Extra :one\nSELECT u.id FROM users AS u WHERE u.id = :id;\n")
	code, _, _ = runCLI("check", false)
	if code != cli.ExitEnvironment {
		t.Fatalf("changed query with dead DSN: exit %d, want %d", code, cli.ExitEnvironment)
	}

	// 5. With the real DSN the new query compiles and re-fills the cache.
	writeConfig(dsn)
	code, out, errOut = runCLI("check", false)
	if code != cli.ExitOK {
		t.Fatalf("recheck: exit %d\n%s%s", code, out, errOut)
	}

	// 6. Exhaustive verification prepares and plans every shape.
	code, out, errOut = runCLI("check", true)
	if code != cli.ExitOK {
		t.Fatalf("exhaustive: exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "shapes prepared and planned") {
		t.Errorf("exhaustive summary missing: %s", out)
	}

	// 6a. verification.max_shapes is the exhaustive budget: a cap the
	// query outgrows fails the check (never a silent partial pass), and
	// raising it makes the same query verifiable again. Without the
	// second half the cap would be a wall, not a budget.
	writeFile(t, dir, "sqletch.yaml", baseConfig(dsn)+"verification:\n  max_shapes: 2\n")
	code, _, errOut = runCLI("check", true)
	if code != cli.ExitDiagnostics {
		t.Fatalf("exhaustive over budget: exit %d, want %d\n%s", code, cli.ExitDiagnostics, errOut)
	}
	if !strings.Contains(errOut, "exceeds the exhaustive-check cap of 2 shapes") ||
		!strings.Contains(errOut, "verification.max_shapes") {
		t.Errorf("over-budget diagnostic must name the cap and the config key:\n%s", errOut)
	}
	writeFile(t, dir, "sqletch.yaml", baseConfig(dsn)+"verification:\n  max_shapes: 10000\n")
	code, out, errOut = runCLI("check", true)
	if code != cli.ExitOK {
		t.Fatalf("exhaustive with a raised budget: exit %d\n%s%s", code, out, errOut)
	}
	writeConfig(dsn)

	// 6b. explain --analyze prints a plan per shape via the dev DB.
	writeConfig(dsn)
	var out2, err2 bytes.Buffer
	analyze := cli.ExplainOptions{Analyze: true}
	if code := cli.Explain(ctx, configPath, []string{"SearchUsers"}, analyze, &out2, &err2); code != cli.ExitOK {
		t.Fatalf("explain --analyze: exit %d\n%s", code, err2.String())
	}
	if strings.Count(out2.String(), "-- SearchUsers shape ") != 8 ||
		!strings.Contains(out2.String(), "Seq Scan") {
		t.Errorf("analyze output unexpected:\n%s", out2.String())
	}

	// 6c. Truncated analysis FAILS: the plans printed cover only the low
	// guard bits, so no claim about the shape space survives. The plans
	// it did produce are still printed.
	var out2c, err2c bytes.Buffer
	capped := cli.ExplainOptions{Analyze: true, MaxShapes: 3}
	if code := cli.Explain(ctx, configPath, []string{"SearchUsers"}, capped, &out2c, &err2c); code != cli.ExitDiagnostics {
		t.Fatalf("truncated --analyze: exit %d, want %d\n%s", code, cli.ExitDiagnostics, err2c.String())
	}
	if n := strings.Count(out2c.String(), "-- SearchUsers shape "); n != 3 {
		t.Errorf("truncated --analyze printed %d plans, want 3:\n%s", n, out2c.String())
	}
	if !strings.Contains(err2c.String(), string(diagnostics.CodeShapeCapReached)) ||
		!strings.Contains(err2c.String(), "--max-shapes") {
		t.Errorf("truncated --analyze must report %s with a hint:\n%s",
			diagnostics.CodeShapeCapReached, err2c.String())
	}

	// 7. A user mistake yields diagnostics (exit 1), not an env error.
	writeFile(t, dir, "queries/users.sql", strings.Replace(cliQuery,
		"AND u.status = :status", "AND ou.bogus = :status", 1))
	code, _, errOut = runCLI("check", false)
	if code != cli.ExitDiagnostics {
		t.Fatalf("broken query: exit %d, want %d\n%s", code, cli.ExitDiagnostics, errOut)
	}
	if !strings.Contains(errOut, "SQLETCH") {
		t.Errorf("diagnostics output missing code: %s", errOut)
	}
}

const cliSchema = `
CREATE TABLE users (
    id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email     text NOT NULL,
    status    text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE organization_users (
    user_id         bigint NOT NULL,
    organization_id bigint NOT NULL
);
`

const cliQuery = `-- name: SearchUsers :many
SELECT u.id, u.email, u.status
FROM users AS u

@if-present(organization_id)
JOIN organization_users AS ou
  ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif

WHERE TRUE

@if-present(status)
  AND u.status = :status
@endif

@choose(sort)
@case(email_asc)
ORDER BY u.email ASC
@default
ORDER BY u.id ASC
@end

LIMIT :limit;

-- name: UsersInStatuses :many
-- @param statuses: text[]
SELECT u.id, u.email
FROM users AS u
WHERE u.status @in(:statuses)
ORDER BY u.id
LIMIT :limit;
`

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
