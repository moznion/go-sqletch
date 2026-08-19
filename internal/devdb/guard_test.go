package devdb

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	sqlite3 "github.com/ncruces/go-sqlite3"
)

// The destructive-reset guard (H1) is fully exercisable in-process via
// the SQLite backend — no Docker, no container — so the whole
// connect → refuse-or-reset path runs in a plain `go test`.

func TestGuardReset(t *testing.T) {
	for _, tc := range []struct {
		name             string
		dsn              string
		allowDestructive bool
		want             bool
	}{
		{"self-provisioned resets", "", false, false},
		{"self-provisioned resets even with flag", "", true, false},
		{"user dsn refused by default", "/tmp/dev.sqlite", false, true},
		{"user dsn allowed by flag", "/tmp/dev.sqlite", true, false},
	} {
		got := Config{DSN: tc.dsn, AllowDestructive: tc.allowDestructive}.guardReset()
		if got != tc.want {
			t.Errorf("%s: guardReset() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// seedSQLite creates a file database holding a `keep` table with one row
// — data a reset would destroy — and returns its path.
func seedSQLite(t *testing.T, path string) {
	t.Helper()
	conn, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Exec("CREATE TABLE keep (id INTEGER PRIMARY KEY); INSERT INTO keep (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
}

func sqliteHasTable(t *testing.T, path, name string) bool {
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

// A user-supplied DSN must NOT be wiped without --allow-destructive: the
// clone-and-run guard. The refusal is *DestructiveResetError and the
// pre-existing data survives untouched.
func TestAcquireSQLite_UserDSNRefusedByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dev.sqlite")
	seedSQLite(t, path)

	_, cleanup, err := AcquireSQLite(context.Background(), Config{
		DSN:       path,
		SchemaSQL: []string{"CREATE TABLE users (id INTEGER PRIMARY KEY)"},
	})
	cleanup()

	var dre *DestructiveResetError
	if !errors.As(err, &dre) {
		t.Fatalf("want *DestructiveResetError, got %v", err)
	}
	if dre.Server != "SQLite" {
		t.Errorf("Server = %q, want SQLite", dre.Server)
	}
	if !sqliteHasTable(t, path, "keep") {
		t.Error("pre-existing data was destroyed despite the refusal")
	}
	if sqliteHasTable(t, path, "users") {
		t.Error("schema was applied despite the refusal")
	}
}

// With the explicit opt-in the reset proceeds: the pre-existing table is
// dropped and the configured schema is applied.
func TestAcquireSQLite_UserDSNAllowedByFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dev.sqlite")
	seedSQLite(t, path)

	_, cleanup, err := AcquireSQLite(context.Background(), Config{
		DSN:              path,
		SchemaSQL:        []string{"CREATE TABLE users (id INTEGER PRIMARY KEY)"},
		AllowDestructive: true,
	})
	defer cleanup()
	if err != nil {
		t.Fatalf("AcquireSQLite with --allow-destructive: %v", err)
	}
	if sqliteHasTable(t, path, "keep") {
		t.Error("reset did not drop the pre-existing table")
	}
	if !sqliteHasTable(t, path, "users") {
		t.Error("schema was not applied")
	}
}

// A self-provisioned database (empty DSN → a fresh temp file) always
// resets: sqletch created it, so there is nothing of the user's to lose.
func TestAcquireSQLite_SelfProvisionedResets(t *testing.T) {
	conn, cleanup, err := AcquireSQLite(context.Background(), Config{
		SchemaSQL: []string{"CREATE TABLE users (id INTEGER PRIMARY KEY)"},
	})
	defer cleanup()
	if err != nil {
		t.Fatalf("self-provisioned AcquireSQLite: %v", err)
	}
	if conn == nil {
		t.Fatal("nil connection")
	}
}

// `:memory:` holds no persistent data, so it is exempt from the guard —
// gating it would break a common in-memory dev workflow for no safety
// gain.
func TestAcquireSQLite_MemoryExemptFromGuard(t *testing.T) {
	conn, cleanup, err := AcquireSQLite(context.Background(), Config{
		DSN:       ":memory:",
		SchemaSQL: []string{"CREATE TABLE users (id INTEGER PRIMARY KEY)"},
	})
	defer cleanup()
	if err != nil {
		t.Fatalf(":memory: must not be gated: %v", err)
	}
	if conn == nil {
		t.Fatal("nil connection")
	}
}
