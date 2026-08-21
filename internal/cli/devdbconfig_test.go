package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlite3 "github.com/ncruces/go-sqlite3"

	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// seedSQLiteFile creates a SQLite database at path holding a `keep`
// table — data the disposable reset would drop, so a refused reset can
// be verified to leave it intact.
func seedSQLiteFile(t *testing.T, path string) {
	t.Helper()
	conn, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Exec("CREATE TABLE keep (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
}

func sqliteFileHasTable(t *testing.T, path, name string) bool {
	t.Helper()
	conn, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	stmt, _, err := conn.Prepare("SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stmt.Close() }()
	if err := stmt.BindText(1, name); err != nil {
		t.Fatal(err)
	}
	return stmt.Step()
}

func TestSQLiteDSNPath(t *testing.T) {
	cfg := config.Config{Dir: "/proj"}
	cfg.Database.DSN = ""
	for _, tc := range []struct{ dsn, want, why string }{
		{"", "", "empty means a private temp file"},
		{"dev.sqlite3", "/proj/dev.sqlite3", "relative paths resolve against the config dir"},
		{"db/dev.sqlite3", "/proj/db/dev.sqlite3", "nested relative path"},
		{"/tmp/abs.sqlite3", "/tmp/abs.sqlite3", "absolute paths pass through"},
		{":memory:", ":memory:", "not a file path"},
		// A `file:` URI names a real on-disk file (M2 fix): a relative path
		// component is re-rooted against the config dir and the URI rebuilt,
		// with query params preserved; absolute/in-memory forms pass through.
		{"file:dev.sqlite3?mode=rw", "file:///proj/dev.sqlite3?mode=rw", "relative file: URI re-roots to the config dir, query preserved"},
		{"file:db/dev.sqlite3", "file:///proj/db/dev.sqlite3", "nested relative file: URI re-roots to the config dir"},
		{"file:dev.db?mode=rwc&_pragma=busy_timeout(1000)", "file:///proj/dev.db?mode=rwc&_pragma=busy_timeout(1000)", "query params preserved verbatim"},
		{"file:/abs/dev.db", "file:/abs/dev.db", "absolute file: path passes through"},
		{"file:///abs/dev.db", "file:///abs/dev.db", "absolute file: path (triple slash) passes through"},
		{"file::memory:?cache=shared", "file::memory:?cache=shared", "in-memory file: URI passes through"},
		{"file:", "file:", "empty file: path is an in-memory database"},
	} {
		cfg.Database.DSN = tc.dsn
		if got := sqliteDSNPath(cfg); got != tc.want {
			t.Errorf("sqliteDSNPath(%q) = %q, want %q (%s)", tc.dsn, got, tc.want, tc.why)
		}
	}
}

// writeSQLiteProject lays out a minimal SQLite project in dir and
// returns its config path. dsn goes into database.dsn verbatim.
func writeSQLiteProject(t *testing.T, dir, serverVersion, dsn string) string {
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
	mustWrite("db/schema.sql", "CREATE TABLE users (id INTEGER PRIMARY KEY, status TEXT NOT NULL);\n")
	mustWrite("queries/q.sql", `-- name: ListUsers :many
-- @param status: text
SELECT u.id FROM users AS u WHERE u.status = :status;
`)
	mustWrite("sqletch.yaml", `version: 1
dialect: sqlite
server_version: "`+serverVersion+`"
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
	return filepath.Join(dir, "sqletch.yaml")
}

// A relative database.dsn must resolve against the config directory,
// like every other path in sqletch.yaml — not against the process's
// working directory, which depends on where the user invoked sqletch.
func TestRun_SQLiteRelativeDSNIsConfigRelative(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSQLiteProject(t, dir, "3", "dev.sqlite3")

	cfg, diags := config.Load(cfgPath)
	if diagnostics.HasErrors(diags) {
		t.Fatalf("config: %v", diags)
	}
	// A user-supplied dsn resets the schema, so the run must opt in.
	res, err := Run(context.Background(), cfg, ModeCheck, RunOptions{AllowDestructive: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("unexpected diagnostics: %v", res.Diags)
	}
	if _, err := os.Stat(filepath.Join(dir, "dev.sqlite3")); err != nil {
		t.Errorf("dev database was not created next to sqletch.yaml: %v", err)
	}
	if _, err := os.Stat("dev.sqlite3"); err == nil {
		_ = os.Remove("dev.sqlite3")
		t.Errorf("dev database was created in the process working directory")
	}
}

// A cold run against a user-supplied database.dsn must NOT wipe it
// without --allow-destructive (H1): the reset is refused with SQLETCH204
// against the config file, and any pre-existing data survives.
func TestRun_UserDSNRefusedWithoutAllowDestructive(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSQLiteProject(t, dir, "3", "dev.sqlite3")

	// Seed the "dev" database with a table a reset would drop.
	dbPath := filepath.Join(dir, "dev.sqlite3")
	seedSQLiteFile(t, dbPath)

	cfg, diags := config.Load(cfgPath)
	if diagnostics.HasErrors(diags) {
		t.Fatalf("config: %v", diags)
	}
	res, err := Run(context.Background(), cfg, ModeCheck, RunOptions{})
	if err != nil {
		t.Fatalf("a refused reset must be a diagnostic, not an environment error: %v", err)
	}
	d := findCode(res.Diags, diagnostics.CodeDestructiveReset)
	if d == nil {
		t.Fatalf("want %s, got %v", diagnostics.CodeDestructiveReset, res.Diags)
	}
	if d.Span.File != cfgPath {
		t.Errorf("span file = %q, want the config file %q", d.Span.File, cfgPath)
	}
	if !strings.Contains(d.Hint, "--allow-destructive") {
		t.Errorf("hint must point at the opt-in flag: %q", d.Hint)
	}
	if !sqliteFileHasTable(t, dbPath, "keep") {
		t.Error("pre-existing data was destroyed despite the refusal")
	}
}

// The same run with AllowDestructive proceeds: no SQLETCH204.
func TestRun_UserDSNAllowedWithAllowDestructive(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSQLiteProject(t, dir, "3", "dev.sqlite3")

	cfg, diags := config.Load(cfgPath)
	if diagnostics.HasErrors(diags) {
		t.Fatalf("config: %v", diags)
	}
	res, err := Run(context.Background(), cfg, ModeCheck, RunOptions{AllowDestructive: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d := findCode(res.Diags, diagnostics.CodeDestructiveReset); d != nil {
		t.Fatalf("--allow-destructive must clear the guard, got %v", d)
	}
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("unexpected diagnostics: %v", res.Diags)
	}
}

// A server_version pin that does not match the engine is a user
// mistake, so it must be a coded diagnostic (SQLETCH200) rather than an
// environment error with no code, no span and no JSON representation.
func TestRun_VersionPinMismatchIsDiagnostic(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSQLiteProject(t, dir, "1.0", "dev.sqlite3")

	cfg, diags := config.Load(cfgPath)
	if diagnostics.HasErrors(diags) {
		t.Fatalf("config: %v", diags)
	}
	res, err := Run(context.Background(), cfg, ModeCheck, RunOptions{})
	if err != nil {
		t.Fatalf("a version pin mismatch must not be an environment error: %v", err)
	}
	d := findCode(res.Diags, diagnostics.CodeServerVersionMismatch)
	if d == nil {
		t.Fatalf("want %s, got %v", diagnostics.CodeServerVersionMismatch, res.Diags)
	}
	if !strings.Contains(d.Message, "SQLite") {
		t.Errorf("message must name the connected server: %q", d.Message)
	}
	if d.Span.File != cfgPath {
		t.Errorf("span file = %q, want the config file %q", d.Span.File, cfgPath)
	}
}
