package dialect_test

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
)

// FoldIdent folds the way each engine compares identifiers. SQLite's
// sqlite3StrICmp folds ASCII letters only (the bundled engine finds
// table `É` as `É`, not as `é`, and `K`elvin is not `kelvin`), so
// its fold must leave every non-ASCII rune alone; MySQL keeps Unicode
// folding; PostgreSQL's facade names are already folded (identity).
func TestFoldIdent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile dialect.LexerProfile
		in, out string
	}{
		{"sqlite ascii", sqlite.Profile{}, "UsErS", "users"},
		{"sqlite non-ascii untouched", sqlite.Profile{}, "CAFÉ", "cafÉ"},
		{"sqlite kelvin sign untouched", sqlite.Profile{}, "Kelvin", "Kelvin"},
		{"sqlite invalid utf-8 untouched", sqlite.Profile{}, "A\xffB", "a\xffb"},
		{"mysql unicode", mysql.Profile{}, "CAFÉ", "café"},
		{"postgres identity", postgres.Profile{}, "CAFÉ", "CAFÉ"},
	} {
		if got := dialect.FoldIdent(tc.profile)(tc.in); got != tc.out {
			t.Errorf("%s: FoldIdent(%q) = %q, want %q", tc.name, tc.in, got, tc.out)
		}
	}
}
