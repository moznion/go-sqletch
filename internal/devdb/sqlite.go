package devdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sqlite3 "github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/ext/fts5"

	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
)

// SQLiteFileAccessError reports a schema statement refused because it
// names a database file (ATTACH, VACUUM INTO, …): the SQLite engine
// runs in-process over the host filesystem, so repo-controlled DDL
// could otherwise create or overwrite files outside the project
// (security audit F15; SQLETCH322 at the CLI layer). Nothing was
// written: the statement is denied before SQLite opens the file.
type SQLiteFileAccessError struct {
	Input  int    // index into Config.SchemaSQL
	Pos    int    // byte offset of the refused statement within that input
	Action string // "ATTACH", "DETACH", "VACUUM", "PRAGMA <name>"
	Target string // the named file, when SQLite saw it as a literal
	// File is the schema file path; devdb only sees SQL text, so the
	// caller that read the files fills it in.
	File string
}

func (e *SQLiteFileAccessError) Error() string {
	return fmt.Sprintf("schema input %d at byte %d: %s", e.Input, e.Pos,
		(&sqlite.FileAccessError{Action: e.Action, Target: e.Target}).Error())
}

// AcquireSQLite opens (or creates) the SQLite dev database — fully
// in-process, nothing external. DSN is a database file path; empty
// means a fresh file in a private temp directory. The database is
// disposable by contract: when SchemaSQL is non-empty, every table and
// view is dropped before the schema is applied.
func AcquireSQLite(ctx context.Context, cfg Config) (*sqlite3.Conn, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, func() {}, err
	}
	removeDir := func() {}
	path := cfg.DSN
	if path == "" {
		dir, err := os.MkdirTemp("", "sqletch-sqlite-*")
		if err != nil {
			return nil, func() {}, err
		}
		removeDir = func() { _ = os.RemoveAll(dir) }
		path = filepath.Join(dir, "dev.sqlite")
	}

	conn, err := sqlite3.Open(path)
	if err != nil {
		removeDir()
		return nil, func() {}, fmt.Errorf("open SQLite dev database: %w", err)
	}
	closeAll := func() {
		_ = conn.Close()
		removeDir()
	}
	if err := fts5.Register(conn); err != nil {
		closeAll()
		return nil, func() {}, fmt.Errorf("register SQLite FTS5 extension: %w", err)
	}
	// Restrict BEFORE any repo-controlled SQL runs; the restriction
	// stays on the returned connection, which becomes the oracle's.
	guard, err := sqlite.Restrict(conn)
	if err != nil {
		closeAll()
		return nil, func() {}, err
	}

	if cfg.wantVersion() {
		actual, err := querySQLiteVersion(conn)
		if err != nil {
			closeAll()
			return nil, func() {}, err
		}
		if err := cfg.recordVersion(actual, "SQLite"); err != nil {
			closeAll()
			return nil, func() {}, err
		}
	}

	if hasSchema(cfg.SchemaSQL) {
		// `:memory:` holds no persistent data — a reset can destroy
		// nothing, so it is exempt from the user-supplied-DSN guard.
		if cfg.DSN != ":memory:" && cfg.guardReset() {
			closeAll()
			return nil, func() {}, &DestructiveResetError{Server: "SQLite"}
		}
		if err := resetSQLite(conn); err != nil {
			closeAll()
			return nil, func() {}, fmt.Errorf("reset dev database schema: %w", err)
		}
		for i, stmt := range cfg.SchemaSQL {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if err := guard.Exec(stmt); err != nil {
				closeAll()
				if fae, ok := errors.AsType[*sqlite.FileAccessError](err); ok {
					return nil, func() {}, &SQLiteFileAccessError{Input: i, Pos: fae.Pos, Action: fae.Action, Target: fae.Target}
				}
				return nil, func() {}, fmt.Errorf("apply schema input %d: %w", i, err)
			}
		}
	}
	return conn, closeAll, nil
}

func querySQLiteVersion(conn *sqlite3.Conn) (string, error) {
	stmt, _, err := conn.Prepare("SELECT sqlite_version()")
	if err != nil {
		return "", err
	}
	defer func() { _ = stmt.Close() }()
	if !stmt.Step() {
		return "", fmt.Errorf("sqlite_version returned no row: %w", stmt.Err())
	}
	return stmt.ColumnText(0), nil
}

// resetSQLite drops every table and view in the main database.
func resetSQLite(conn *sqlite3.Conn) error {
	stmt, _, err := conn.Prepare(
		"SELECT type, name FROM sqlite_master WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		return err
	}
	type obj struct{ typ, name string }
	var objs []obj
	for stmt.Step() {
		objs = append(objs, obj{stmt.ColumnText(0), stmt.ColumnText(1)})
	}
	if err := stmt.Err(); err != nil {
		_ = stmt.Close()
		return err
	}
	if err := stmt.Close(); err != nil {
		return err
	}
	if len(objs) == 0 {
		return nil
	}
	if err := conn.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	for _, o := range objs {
		kind := "TABLE"
		if o.typ == "view" {
			kind = "VIEW"
		}
		quoted := `"` + strings.ReplaceAll(o.name, `"`, `""`) + `"`
		if err := conn.Exec("DROP " + kind + " IF EXISTS " + quoted); err != nil {
			return err
		}
	}
	return conn.Exec("PRAGMA foreign_keys = ON")
}
