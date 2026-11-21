package dialect_test

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
)

// TargetItem.FuncName names the BUILTIN a call resolves to, keyed the
// way the engine matches function names — never through a Go fold.
// nullability's funcWhitelist (count, now) and strictAggs (sum, min,
// max, avg) trust it, so a user function lowered into one of those
// names was narrowed to NOT NULL (devdb counterexamples in
// nullability_soundness_devdb_test.go).
//
//   - PostgreSQL resolves function names exactly; its parser already
//     downcased unquoted ASCII. `"COUNT"` and `mİn` are user functions.
//   - MySQL 8.4 matches builtins ASCII-only: `İFNULL(NULL, 1)` is
//     "FUNCTION d.İFNULL does not exist" (a stored-function lookup).
//   - SQLite matches function names ASCII-case-insensitively.
func TestTargetItemFuncNameUsesEngineKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		fe   dialect.Frontend
		sql  string
		want string
	}{
		{"pg unquoted", postgres.Frontend{}, "SELECT COUNT(t.x) AS c FROM t", "count"},
		{"pg quoted upper", postgres.Frontend{}, `SELECT "COUNT"(t.x) AS c FROM t`, "COUNT"},
		{"pg catalog quoted upper", postgres.Frontend{}, `SELECT pg_catalog."COUNT"(t.x) AS c FROM t`, "COUNT"},
		{"pg dotted I", postgres.Frontend{}, "SELECT mİn(t.x) AS c FROM t GROUP BY t.x", "mİn"},
		{"mysql ascii", mysql.Frontend{}, "SELECT IfNull(t.x, 1) AS c FROM t", "ifnull"},
		{"mysql dotted I", mysql.Frontend{}, "SELECT İFNULL(t.x, 1) AS c FROM t", "İfnull"},
		{"mysql aggregate", mysql.Frontend{}, "SELECT MAX(t.x) AS c FROM t GROUP BY t.y", "max"},
		{"sqlite ascii", sqlite.Frontend{}, "SELECT Count(t.x) AS c FROM t", "count"},
		{"sqlite dotted I", sqlite.Frontend{}, "SELECT MİN(t.x) AS c FROM t GROUP BY t.x", "mİn"},
	} {
		tree, err := tc.fe.Parse(tc.sql)
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		if got := tree.TargetItems()[0].FuncName; got != tc.want {
			t.Errorf("%s: FuncName = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// SQLite's `coalesce` totality check matches ASCII-only too: `coaleſce`
// (long s) is not coalesce, although strings.EqualFold says so.
func TestSQLiteCoalesceTotalityIsASCIIOnly(t *testing.T) {
	for sql, want := range map[string]bool{
		"SELECT CoAlEsCe(t.x, 1) AS c FROM t": true,
		"SELECT coaleſce(t.x, 1) AS c FROM t": false,
	} {
		tree, err := sqlite.Frontend{}.Parse(sql)
		if err != nil {
			t.Fatalf("parse %q: %v", sql, err)
		}
		if got := tree.TargetItems()[0].Total; got != want {
			t.Errorf("%q: Total = %v, want %v", sql, got, want)
		}
	}
}
