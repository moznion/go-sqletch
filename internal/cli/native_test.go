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

	res, err := Run(context.Background(), cfg, ModeGenerate, RunOptions{})
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

	warm, err := Run(context.Background(), cfg, ModeCheck, RunOptions{})
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
	res, err := Run(context.Background(), cfg, ModeCheck, RunOptions{})
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
	res, err := Run(context.Background(), cfg, ModeCheck, RunOptions{})
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
	res, err := Run(context.Background(), cfg, ModeCheckExhaustive, RunOptions{})
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

// TestRun_NativeGenerateMatchesCommittedExample is the cold-run gate
// (design 15 §7): generating the examples/mysql module natively —
// fresh cache, no server — must reproduce the committed, server-built
// module byte for byte.
func TestRun_NativeGenerateMatchesCommittedExample(t *testing.T) {
	src := filepath.Join("..", "..", "examples", "mysql")
	dir := t.TempDir()
	for _, rel := range []string{"db/schema.sql", "queries/users.sql"} {
		data, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "sqletch.yaml"), []byte(`version: 1
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
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, diags := config.Load(filepath.Join(dir, "sqletch.yaml"))
	if len(diags) > 0 {
		t.Fatalf("config: %+v", diags)
	}
	res, err := Run(context.Background(), cfg, ModeGenerate, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("diagnostics: %+v", res.Diags)
	}

	want, err := os.ReadDir(filepath.Join(src, "gen"))
	if err != nil {
		t.Fatal(err)
	}
	compared := 0
	for _, e := range want {
		if e.IsDir() {
			continue
		}
		wantBytes, err := os.ReadFile(filepath.Join(src, "gen", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		gotBytes, err := os.ReadFile(filepath.Join(dir, "gen", e.Name()))
		if err != nil {
			t.Fatalf("native generate did not produce %s: %v", e.Name(), err)
		}
		if string(wantBytes) != string(gotBytes) {
			t.Errorf("%s: native-generated module differs from the committed server-built one", e.Name())
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("no committed generated files to compare against")
	}
}

// TestRun_ColumnHintConflictIsSQLETCH216: a wrong `-- @column` must
// be loud whenever an oracle answer exists to check it against — the
// oracle wins, never the annotation (D7).
func TestRun_ColumnHintConflictIsSQLETCH216(t *testing.T) {
	dir := t.TempDir()
	query := `-- name: OneUser :many
-- @param id: bigint
-- @column email: bigint
SELECT u.email FROM users AS u WHERE u.id = :id;
`
	cfg := writeNativeMySQLProject(t, dir, nativeCLISchema, query)
	res, err := Run(context.Background(), cfg, ModeCheck, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range res.Diags {
		if d.Code == diagnostics.CodeColumnHintConflict {
			found = true
			if !strings.Contains(d.Message, "oracle wins") {
				t.Errorf("message must state the precedence rule, got %q", d.Message)
			}
		}
	}
	if !found {
		t.Fatalf("want SQLETCH216, got %+v", res.Diags)
	}
}
