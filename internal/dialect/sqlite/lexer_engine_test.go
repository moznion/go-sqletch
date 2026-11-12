package sqlite

import (
	"strings"
	"testing"

	sqlite3 "github.com/ncruces/go-sqlite3"
)

// The engine readings TestLexer_NumericLiteralsMatchSQLite encodes, so
// a SQLite upgrade that changes them is noticed (the bundled engine is
// the profile's ground truth). want is the first row's columns as
// name=value, or "!" + an error substring.
func TestLexer_NumericLiteralsSQLiteReadings(t *testing.T) {
	conn, err := sqlite3.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for src, want := range map[string]string{
		"1_000":       "1_000=1000",
		"0x1_F":       "0x1_F=31",
		"1_0.0_1e1_0": "1_0.0_1e1_0=100100000000.0",
		".5e1_0":      ".5e1_0=5000000000.0",
		"1.e5":        "1.e5=100000.0",
		"0x1F'a'":     "a=31",
		"1e5'b'":      "b=100000.0",
		"0x_1":        `!unrecognized token: "0x_1"`,
		"1._5":        `!unrecognized token: "1._5"`,
		"1e_5":        `!unrecognized token: "1e_5"`,
		"1e+":         `!unrecognized token: "1e"`,
		"1.2.3":       `!near ".3": syntax error`,
		"1..2":        `!near ".2": syntax error`,
	} {
		got := sqliteReading(conn, "SELECT "+src)
		if strings.HasPrefix(want, "!") {
			if !strings.HasPrefix(got, "!") || !strings.Contains(got, want[1:]) {
				t.Errorf("%q: got %q, want error containing %q", src, got, want[1:])
			}
			continue
		}
		if got != want {
			t.Errorf("%q: got %q, want %q", src, got, want)
		}
	}
}

func sqliteReading(conn *sqlite3.Conn, sql string) string {
	stmt, _, err := conn.Prepare(sql)
	if err != nil {
		return "!" + err.Error()
	}
	defer func() { _ = stmt.Close() }()
	if !stmt.Step() {
		return "!no row: " + stmt.Err().Error()
	}
	var cols []string
	for i := range stmt.ColumnCount() {
		cols = append(cols, stmt.ColumnName(i)+"="+stmt.ColumnText(i))
	}
	return strings.Join(cols, ",")
}

// The engine reads `$` inside an unquoted identifier as part of it
// (TestLexer_DollarInIdentifierMatchesSQLite). rqlite/sql's scanner
// does not; the frontend's rewrite (dollar_ident.go) makes it accept the
// same statements, which this pins against the engine.
func TestLexer_DollarInIdentifierReadings(t *testing.T) {
	conn, err := sqlite3.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Exec("CREATE TABLE t (a$b INTEGER, c INTEGER); INSERT INTO t VALUES (1, 2)"); err != nil {
		t.Fatal(err)
	}
	for sql, want := range map[string]string{
		"SELECT a$b FROM t":   "a$b=1",
		"SELECT t.a$b FROM t": "a$b=1",
		"SELECT 1 AS x$$y":    "x$$y=1",
		"SELECT c x$1 FROM t": "x$1=2",
	} {
		if got := sqliteReading(conn, sql); got != want {
			t.Errorf("engine %q: got %q, want %q", sql, got, want)
		}
		if _, err := (Frontend{}).Parse(sql); err != nil {
			t.Errorf("frontend refuses %q: %v", sql, err)
		}
	}
}
