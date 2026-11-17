package dialect_test

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
)

// Each profile folds the way its engine compares identifiers (server
// ground truth: TestMySQLIdentFold* in internal/e2e, SQLite readings in
// internal/dialect/sqlite).
//
//   - SQLite (sqlite3StrICmp) folds ASCII letters only, for columns and
//     relations alike: `É` ≠ `é`, the Kelvin sign is not `k`.
//   - MySQL columns fold through utf8mb3_general_ci's own case table:
//     `İ` = `i` and the Kelvin sign = `k`, but `ẞ` ≠ `ß` and `Ƀ` ≠ `ƀ`
//     (Go's ToLower merges those). Relation names compare
//     exactly under lower_case_table_names=0 and case-insensitively
//     under 1/2: "must" is exact, "may" is the column fold.
//   - PostgreSQL's facade names are already folded: identity.
func TestColumnAndRelationFolds(t *testing.T) {
	type fold = func(string) string
	sqMay, sqMust := dialect.RelationFolds(sqlite.Profile{})
	myMay, myMust := dialect.RelationFolds(mysql.Profile{})
	pgMay, pgMust := dialect.RelationFolds(postgres.Profile{})
	for _, tc := range []struct {
		name string
		f    fold
		a, b string
		same bool
	}{
		{"sqlite column ascii", dialect.ColumnFold(sqlite.Profile{}), "UsErS", "users", true},
		{"sqlite column non-ascii", dialect.ColumnFold(sqlite.Profile{}), "É", "é", false},
		{"sqlite column kelvin", dialect.ColumnFold(sqlite.Profile{}), "Kelvin", "kelvin", false},
		{"sqlite column invalid utf-8", dialect.ColumnFold(sqlite.Profile{}), "A\xffB", "a\xffb", true},
		{"sqlite relation may", sqMay, "É", "é", false},
		{"sqlite relation must ascii", sqMust, "A", "a", true},

		{"mysql column ascii", dialect.ColumnFold(mysql.Profile{}), "UsErS", "users", true},
		{"mysql column accented", dialect.ColumnFold(mysql.Profile{}), "É", "é", true},
		{"mysql column dotted I", dialect.ColumnFold(mysql.Profile{}), "İ", "i", true},
		{"mysql column dotless i", dialect.ColumnFold(mysql.Profile{}), "ı", "i", false},
		{"mysql column kelvin", dialect.ColumnFold(mysql.Profile{}), "Kelvin", "kelvin", true},
		{"mysql column capital sharp s", dialect.ColumnFold(mysql.Profile{}), "ẞ", "ß", false},
		{"mysql column titlecase", dialect.ColumnFold(mysql.Profile{}), "Ǆ", "ǅ", true},
		{"mysql column ligature", dialect.ColumnFold(mysql.Profile{}), "ﬀ", "ff", false},
		{"mysql column barred b", dialect.ColumnFold(mysql.Profile{}), "Ƀ", "ƀ", false},
		{"mysql column invalid utf-8", dialect.ColumnFold(mysql.Profile{}), "A\xffB", "a\xffb", true},
		{"mysql relation may", myMay, "A", "a", true},
		{"mysql relation may accented", myMay, "É", "é", true},
		{"mysql relation must", myMust, "A", "a", false},
		{"mysql relation must identical", myMust, "a", "a", true},

		{"postgres column", dialect.ColumnFold(postgres.Profile{}), "A", "a", false},
		{"postgres relation may", pgMay, "A", "a", false},
		{"postgres relation must", pgMust, "A", "a", false},
	} {
		if got := tc.f(tc.a) == tc.f(tc.b); got != tc.same {
			t.Errorf("%s: %q vs %q equal = %v, want %v", tc.name, tc.a, tc.b, got, tc.same)
		}
	}
}
