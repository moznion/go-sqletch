package devdb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Security audit F15 (CWE-22): the SQLite dev database runs IN-PROCESS
// with the host's filesystem (ncruces/go-sqlite3 over an OS VFS), unlike
// PostgreSQL/MySQL whose DDL runs inside a throwaway container. Schema
// files are repo-controlled, so `ATTACH DATABASE '/elsewhere.db'` or
// `VACUUM INTO '/elsewhere.db'` in one would create or overwrite files
// outside the project on the first cold run — bypassing the SQLETCH306
// path-escape policy the SQLite DSN itself is held to. Schema
// application must refuse every statement that names a database file.

// outsidePath returns a path in a directory distinct from anything the
// dev database uses, plus a check that nothing was created there.
func outsidePath(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "escaped.db")
	return p, func() {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("schema application touched the filesystem outside the dev database: %v", names)
		}
	}
}

func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func TestAcquireSQLite_RefusesFileAccessInSchema(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema func(target string) []string
		input  int
		action string
	}{
		{
			name: "attach database",
			schema: func(target string) []string {
				return []string{"CREATE TABLE ok (id INTEGER PRIMARY KEY);\nATTACH DATABASE " + sqlQuote(target) + " AS x;\nCREATE TABLE x.t (id INTEGER);"}
			},
			action: "ATTACH",
		},
		{
			name: "attach without DATABASE keyword, second schema input",
			schema: func(target string) []string {
				return []string{"CREATE TABLE ok (id INTEGER PRIMARY KEY);", "ATTACH " + sqlQuote(target) + " AS x;"}
			},
			input:  1,
			action: "ATTACH",
		},
		{
			name: "vacuum into",
			schema: func(target string) []string {
				return []string{"CREATE TABLE ok (id INTEGER PRIMARY KEY);\nVACUUM INTO " + sqlQuote(target) + ";"}
			},
			action: "VACUUM",
		},
		{
			name: "attach as a uri filename",
			schema: func(target string) []string {
				return []string{"ATTACH " + sqlQuote("file:"+target+"?mode=rwc") + " AS x;"}
			},
			action: "ATTACH",
		},
		{
			name: "detach",
			schema: func(string) []string {
				return []string{"DETACH DATABASE temp;"}
			},
			action: "DETACH",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, assertUntouched := outsidePath(t)
			_, cleanup, err := AcquireSQLite(context.Background(), Config{SchemaSQL: tc.schema(target)})
			t.Cleanup(cleanup)
			var fae *SQLiteFileAccessError
			if !errors.As(err, &fae) {
				t.Fatalf("err = %v (%T), want *SQLiteFileAccessError", err, err)
			}
			if fae.Input != tc.input {
				t.Errorf("Input = %d, want %d", fae.Input, tc.input)
			}
			if fae.Action != tc.action {
				t.Errorf("Action = %q, want %q", fae.Action, tc.action)
			}
			assertUntouched()
		})
	}
}

// Plain VACUUM attaches a temporary database internally; it is refused
// too rather than special-cased, because a schema file has no reason to
// vacuum a database sqletch just created.
func TestAcquireSQLite_RefusesPlainVacuum(t *testing.T) {
	_, cleanup, err := AcquireSQLite(context.Background(), Config{SchemaSQL: []string{"CREATE TABLE ok (id INTEGER);\nVACUUM;"}})
	t.Cleanup(cleanup)
	var fae *SQLiteFileAccessError
	if !errors.As(err, &fae) || fae.Action != "VACUUM" {
		t.Fatalf("err = %v, want *SQLiteFileAccessError{Action: VACUUM}", err)
	}
}

// The restriction must outlive schema application: the returned
// connection is the oracle's, and templates must not attach either.
func TestAcquireSQLite_ReturnedConnStillRefusesAttach(t *testing.T) {
	target, assertUntouched := outsidePath(t)
	conn, cleanup, err := AcquireSQLite(context.Background(), Config{SchemaSQL: []string{"CREATE TABLE ok (id INTEGER);"}})
	t.Cleanup(cleanup)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec("ATTACH " + sqlQuote(target) + " AS x; CREATE TABLE x.t (id INTEGER);"); err == nil {
		t.Fatal("ATTACH on the returned connection must fail")
	}
	if err := conn.Exec("VACUUM INTO " + sqlQuote(target)); err == nil {
		t.Fatal("VACUUM INTO on the returned connection must fail")
	}
	assertUntouched()
}

// Must-stay: ordinary DDL — tables, indexes, views, triggers, virtual
// tables, pragmas, seed rows, transactions — still applies under the
// restriction.
func TestAcquireSQLite_OrdinaryDDLStillApplies(t *testing.T) {
	conn, cleanup, err := AcquireSQLite(context.Background(), Config{SchemaSQL: []string{`
PRAGMA foreign_keys = ON;
BEGIN;
CREATE TABLE orgs (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE users (
    id     INTEGER PRIMARY KEY,
    org_id INTEGER REFERENCES orgs(id),
    email  TEXT NOT NULL UNIQUE
);
CREATE INDEX users_org ON users(org_id);
CREATE VIEW user_emails AS SELECT id, email FROM users;
CREATE TRIGGER users_ai AFTER INSERT ON users BEGIN SELECT 1; END;
CREATE TEMP TABLE scratch (x INTEGER);
CREATE VIRTUAL TABLE docs USING fts5(body);
INSERT INTO orgs (id, name) VALUES (1, 'acme');
COMMIT;
`, `ALTER TABLE users ADD COLUMN nick TEXT;`}})
	t.Cleanup(cleanup)
	if err != nil {
		t.Fatalf("ordinary schema must apply: %v", err)
	}
	stmt, _, err := conn.Prepare("SELECT count(*) FROM sqlite_master WHERE name IN ('orgs','users','users_org','user_emails','users_ai','docs')")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stmt.Close() }()
	if !stmt.Step() {
		t.Fatal(stmt.Err())
	}
	if n := stmt.ColumnInt(0); n != 6 {
		t.Errorf("schema objects = %d, want 6", n)
	}
}
