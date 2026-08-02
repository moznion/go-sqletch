package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// writeNativeMySQLProject lays out a MySQL project pinned to the
// native oracle backend; there is no DSN and no server anywhere.
func writeNativeMySQLProject(t *testing.T, dir, schemaSQL, querySQL string) config.Config {
	t.Helper()
	mustWrite := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("db/schema.sql", schemaSQL)
	mustWrite("queries/q.sql", querySQL)
	mustWrite("sqletch.yaml", `version: 1
dialect: mysql
server_version: "8.4"
database:
  oracle: native
schema:
  files: [db/schema.sql]
queries: [queries/*.sql]
output:
  package: gen
  path: gen
`)
	cfg, diags := config.Load(filepath.Join(dir, "sqletch.yaml"))
	if len(diags) > 0 {
		t.Fatalf("config: %+v", diags)
	}
	return cfg
}

const nativeCLISchema = `CREATE TABLE users (
    id     BIGINT AUTO_INCREMENT PRIMARY KEY,
    email  VARCHAR(255) NOT NULL,
    status VARCHAR(32) NOT NULL,
    nick   VARCHAR(64)
);
`

const nativeCLIQuery = `-- name: SearchUsers :many
-- @param status: varchar(32)
-- @param limit: bigint
SELECT u.id, u.email, u.nick
FROM users AS u
WHERE TRUE
@if-present(status)
  AND u.status = :status
@endif
ORDER BY u.id
LIMIT :limit;
`

// TestRun_NativeMySQLColdGenerate is the feature's reason to exist:
// a cold generate on MySQL with no Docker, no DSN, and no server —
// then a warm, fully offline check from the cache it wrote.
func TestRun_NativeMySQLColdGenerate(t *testing.T) {
	dir := t.TempDir()
	cfg := writeNativeMySQLProject(t, dir, nativeCLISchema, nativeCLIQuery)

	res, err := Run(context.Background(), cfg, ModeGenerate)
	if err != nil {
		t.Fatalf("cold native generate must not need an environment: %v", err)
	}
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("unexpected diagnostics: %+v", res.Diags)
	}
	if res.OracleMiss == 0 {
		t.Fatal("cold run should have described renderings")
	}
	if _, err := os.Stat(filepath.Join(dir, "gen")); err != nil {
		t.Fatalf("generated module missing: %v", err)
	}

	warm, err := Run(context.Background(), cfg, ModeCheck)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.HasErrors(warm.Diags) || !warm.Offline || warm.OracleMiss != 0 {
		t.Fatalf("warm check must be fully offline: offline=%v miss=%d diags=%+v",
			warm.Offline, warm.OracleMiss, warm.Diags)
	}
}

func TestRun_NativeRefusalIsSQLETCH214(t *testing.T) {
	dir := t.TempDir()
	query := `-- name: CountUsers :many
SELECT count(*) FROM users;
`
	cfg := writeNativeMySQLProject(t, dir, nativeCLISchema, query)
	res, err := Run(context.Background(), cfg, ModeCheck)
	if err != nil {
		t.Fatal(err)
	}
	var found *diagnostics.Diagnostic
	for i := range res.Diags {
		if res.Diags[i].Code == diagnostics.CodeNativeUnsupported {
			found = &res.Diags[i]
		}
	}
	if found == nil {
		t.Fatalf("want SQLETCH214, got %+v", res.Diags)
	}
	if !strings.Contains(found.Hint, "AS alias") && !strings.Contains(found.Hint, "server") {
		t.Errorf("refusal hint must show the way out, got %q", found.Hint)
	}
	if found.Span.File != filepath.Join(dir, "queries/q.sql") {
		t.Errorf("span must point into the template file, got %q", found.Span.File)
	}
}

func TestRun_NativeDDLRefusalIsSQLETCH215(t *testing.T) {
	dir := t.TempDir()
	schema := nativeCLISchema + "ALTER TABLE users ADD COLUMN bio TEXT;\n"
	cfg := writeNativeMySQLProject(t, dir, schema, nativeCLIQuery)
	res, err := Run(context.Background(), cfg, ModeCheck)
	if err != nil {
		t.Fatal(err)
	}
	var found *diagnostics.Diagnostic
	for i := range res.Diags {
		if res.Diags[i].Code == diagnostics.CodeNativeDDL {
			found = &res.Diags[i]
		}
	}
	if found == nil {
		t.Fatalf("want SQLETCH215, got %+v", res.Diags)
	}
	if found.Span.File != filepath.Join(dir, "db/schema.sql") {
		t.Errorf("span must point into the schema file, got %q", found.Span.File)
	}
	if found.Span.Start == 0 {
		t.Error("span should point at the ALTER statement, not the file start")
	}
}

// TestRun_NativeExhaustiveSaysWhatItProves: D2 — under the native
// backend an exhaustive check runs, and the result is flagged so the
// summary does not claim EXPLAIN coverage.
func TestRun_NativeExhaustiveSaysWhatItProves(t *testing.T) {
	dir := t.TempDir()
	cfg := writeNativeMySQLProject(t, dir, nativeCLISchema, nativeCLIQuery)
	res, err := Run(context.Background(), cfg, ModeCheckExhaustive)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("unexpected diagnostics: %+v", res.Diags)
	}
	if !res.NativePlan {
		t.Fatal("Result.NativePlan must be set so the summary is honest about EXPLAIN")
	}
	if res.ShapesTotal == 0 {
		t.Fatal("exhaustive must still verify every shape natively")
	}
}
