package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlite3 "github.com/ncruces/go-sqlite3"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// Security audit F15 (CWE-22): the SQLite engine runs in-process over
// the host filesystem, so any statement naming a database file (ATTACH,
// VACUUM INTO) would read or write outside the project. Restrict is the
// one seam that closes it for schema application (devdb) and the oracle.

func emptyDir(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	return dir, func() {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("the engine touched the filesystem outside its database: %d entries in %s", len(entries), dir)
		}
	}
}

func q(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func TestGuardExec_RefusesFileAccess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		script  func(dir string) string
		action  string
		wantPos int
		target  bool // the refusal names the file
	}{
		{"attach", func(d string) string {
			return "CREATE TABLE a (x INTEGER);\n  ATTACH DATABASE " + q(filepath.Join(d, "x.db")) + " AS x;"
		}, "ATTACH", len("CREATE TABLE a (x INTEGER);\n  "), true},
		{"attach after a comment", func(d string) string {
			return "-- seed\n/* c */ ATTACH " + q(filepath.Join(d, "x.db")) + " AS x"
		}, "ATTACH", len("-- seed\n/* c */ "), true},
		{"attach computed filename", func(d string) string {
			return "ATTACH " + q(d) + " || '/x.db' AS x"
		}, "ATTACH", 0, false},
		{"vacuum into", func(d string) string {
			return "CREATE TABLE a (x INTEGER); VACUUM INTO " + q(filepath.Join(d, "x.db"))
		}, "VACUUM", len("CREATE TABLE a (x INTEGER); "), true},
		{"plain vacuum", func(string) string { return "VACUUM" }, "VACUUM", 0, false},
		{"detach", func(string) string { return "DETACH temp" }, "DETACH", 0, false},
		{"temp_store_directory", func(d string) string {
			return "PRAGMA temp_store_directory = " + q(d)
		}, "PRAGMA temp_store_directory", 0, false},
		{"data_store_directory", func(d string) string {
			return "PRAGMA data_store_directory = " + q(d)
		}, "PRAGMA data_store_directory", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, assertEmpty := emptyDir(t)
			conn, err := sqlite3.Open(filepath.Join(t.TempDir(), "main.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			g, err := Restrict(conn)
			if err != nil {
				t.Fatal(err)
			}
			script := tc.script(dir)
			fae, ok := g.Exec(script).(*FileAccessError)
			if !ok {
				t.Fatalf("Exec(%q) = %v, want *FileAccessError", script, g.Exec(script))
			}
			if fae.Action != tc.action {
				t.Errorf("Action = %q, want %q", fae.Action, tc.action)
			}
			if fae.Pos != tc.wantPos {
				t.Errorf("Pos = %d, want %d", fae.Pos, tc.wantPos)
			}
			if tc.target && !strings.Contains(fae.Target, dir) {
				t.Errorf("Target = %q, want the named file under %s", fae.Target, dir)
			}
			assertEmpty()
		})
	}
}

// The authorizer is not the only wall: LIMIT_ATTACHED=0 still refuses
// an ATTACH if something replaces or clears the authorizer.
func TestRestrict_AttachLimitSurvivesAuthorizerRemoval(t *testing.T) {
	dir, assertEmpty := emptyDir(t)
	conn, err := sqlite3.Open(filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := Restrict(conn); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetAuthorizer(nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec("ATTACH " + q(filepath.Join(dir, "x.db")) + " AS x"); err == nil {
		t.Fatal("ATTACH must fail under LIMIT_ATTACHED=0")
	}
	if err := conn.Exec("VACUUM INTO " + q(filepath.Join(dir, "y.db"))); err == nil {
		t.Fatal("VACUUM INTO must fail under LIMIT_ATTACHED=0")
	}
	assertEmpty()
}

// The rest of the file-touching surface is closed by the engine build:
// no loadable extensions, no fileio. Pinned so a driver upgrade that
// compiles them in is noticed.
func TestRestrict_NoFileFunctions(t *testing.T) {
	conn, err := sqlite3.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	for _, sql := range []string{
		"SELECT load_extension('x')",
		"SELECT readfile('x')",
		"SELECT writefile('x', 'y')",
		"SELECT * FROM fsdir('.')",
	} {
		stmt, _, err := conn.Prepare(sql)
		if err == nil {
			_ = stmt.Close()
			t.Errorf("%s must not prepare: the function must not exist in this engine build", sql)
		}
	}
}

// Must-stay: ordinary schema scripts run unchanged under the guard,
// including comment-only and empty statements between real ones.
func TestGuardExec_OrdinaryScript(t *testing.T) {
	conn, err := sqlite3.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	g, err := Restrict(conn)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Exec(`
PRAGMA foreign_keys = ON;
-- a comment-only stretch
;;
BEGIN;
CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT);
CREATE INDEX t_v ON t(v);
CREATE VIEW tv AS SELECT v FROM t;
INSERT INTO t (v) VALUES ('a'), ('b');
COMMIT;
/* trailing comment */`); err != nil {
		t.Fatalf("ordinary script: %v", err)
	}
	stmt, _, err := conn.Prepare("SELECT count(*) FROM tv")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stmt.Close() }()
	if !stmt.Step() || stmt.ColumnInt(0) != 2 {
		t.Fatalf("seed rows not applied: %v", stmt.Err())
	}
	// A genuine SQL error is passed through, not mistaken for a refusal.
	err = g.Exec("CREATE TABLE t (id INTEGER)")
	if err == nil {
		t.Fatal("duplicate table must fail")
	}
	if _, ok := err.(*FileAccessError); ok {
		t.Fatalf("plain SQL error reported as a file-access refusal: %v", err)
	}
}

// Oracle side: a template that names a database file is refused at
// prepare time with a message stating the rule, and the oracle survives.
func TestOracle_RefusesAttach(t *testing.T) {
	dir, assertEmpty := emptyDir(t)
	o := NewOracle(testConn(t))
	ctx := context.Background()
	for _, sql := range []string{
		"ATTACH DATABASE " + q(filepath.Join(dir, "x.db")) + " AS x",
		"DETACH DATABASE temp",
	} {
		_, err := o.Describe(ctx, sql)
		var oe *dialect.OracleError
		if !asOracleErr(err, &oe) {
			t.Fatalf("Describe(%q) = %v, want *dialect.OracleError", sql, err)
		}
		if !strings.Contains(oe.Msg, "refused") || !strings.Contains(oe.Msg, "outside the project") {
			t.Errorf("message must state the rule and its rationale: %q", oe.Msg)
		}
		if err := o.Plan(ctx, sql); err == nil {
			t.Errorf("Plan(%q) must fail", sql)
		}
	}
	// The connection itself stays restricted: nothing the oracle could
	// step may attach.
	if err := o.conn.Exec("VACUUM INTO " + q(filepath.Join(dir, "v.db"))); err == nil {
		t.Error("VACUUM INTO on the oracle's connection must fail")
	}
	if _, err := o.Describe(ctx, "SELECT id FROM users"); err != nil {
		t.Fatalf("oracle unusable after a refusal: %v", err)
	}
	// Must-stay: an unrelated prepare error is not reported as a refusal.
	_, err := o.Describe(ctx, "SELECT nonexistent FROM users")
	var oe *dialect.OracleError
	if !asOracleErr(err, &oe) || strings.Contains(oe.Msg, "outside the project") {
		t.Errorf("plain error misreported: %v", err)
	}
	assertEmpty()
}
