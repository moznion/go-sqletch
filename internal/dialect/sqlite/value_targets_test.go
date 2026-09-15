package sqlite

import (
	"reflect"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// nthQ returns the byte offset of the n-th (0-based) '?' in s.
func nthQ(t *testing.T, s string, n int) int {
	t.Helper()
	seen := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '?' {
			if seen == n {
				return i
			}
			seen++
		}
	}
	t.Fatalf("placeholder %d not found in %q", n, s)
	return -1
}

type wantTarget struct {
	col string
	nth int // which '?' is the Loc
}

// ValueTargets is design 20 §3.1's facade for SQLite: rqlite's CastExpr
// and ParenExpr are looked through; everything else is an expression.
// Locs are BYTE offsets even though rqlite reports rune positions.
func TestValueTargets(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []wantTarget
	}{
		{"insert values", "INSERT INTO users (email, nickname) VALUES (?, ?)",
			[]wantTarget{{"email", 0}, {"nickname", 1}}},
		{"multi-row values", "INSERT INTO users (email, nickname) VALUES (?, ?), (?, ?)",
			[]wantTarget{{"email", 0}, {"nickname", 1}, {"email", 2}, {"nickname", 3}}},
		{"insert or replace", "INSERT OR REPLACE INTO users (email) VALUES (?)",
			[]wantTarget{{"email", 0}}},
		{"replace into", "REPLACE INTO users (email) VALUES (?)",
			[]wantTarget{{"email", 0}}},
		{"casts and parentheses are looked through",
			"INSERT INTO users (a, b, c) VALUES (CAST(? AS TEXT), CAST(CAST(? AS TEXT) AS INTEGER), ((?)))",
			[]wantTarget{{"a", 0}, {"b", 1}, {"c", 2}}},
		{"multibyte text before the placeholder", "INSERT INTO users (a, b) VALUES ('日本語', ?)",
			[]wantTarget{{"b", 0}}},
		{"quoted column keeps its spelling", `INSERT INTO users ("NickName") VALUES (?)`,
			[]wantTarget{{"NickName", 0}}},
		{"expressions are not direct", "INSERT INTO users (a, b, c) VALUES (coalesce(?, 'x'), lower(?), ? || 'y')", nil},
		{"insert without column list", "INSERT INTO users VALUES (?, ?)", nil},
		{"insert select", "INSERT INTO users (a) SELECT ?", nil},
		{"default values", "INSERT INTO users DEFAULT VALUES", nil},
		{"row arity mismatch is skipped", "INSERT INTO users (a, b) VALUES (?)", nil},
		{"upsert arm is not a value position",
			"INSERT INTO users (id, nickname) VALUES (?, ?) ON CONFLICT (id) DO UPDATE SET nickname = ?",
			[]wantTarget{{"id", 0}, {"nickname", 1}}},
		{"excluded idiom", "INSERT INTO users (id, nickname) VALUES (?, ?) ON CONFLICT (id) DO UPDATE SET nickname = excluded.nickname",
			[]wantTarget{{"id", 0}, {"nickname", 1}}},
		{"returning is not a value position", "INSERT INTO users (a) VALUES (?) RETURNING ?",
			[]wantTarget{{"a", 0}}},
		{"update set", "UPDATE users SET nickname = ?, email = CAST(? AS TEXT) WHERE id = ?",
			[]wantTarget{{"nickname", 0}, {"email", 1}}},
		{"update or ignore", "UPDATE OR IGNORE users SET nickname = ?",
			[]wantTarget{{"nickname", 0}}},
		{"update from", "UPDATE users SET nickname = ? FROM orgs WHERE orgs.id = users.org_id AND orgs.name = ?",
			[]wantTarget{{"nickname", 0}}},
		{"tuple set", "UPDATE users SET (a, b) = (?, ?)", nil},
		{"parenthesized single-column set", "UPDATE users SET (a) = (?)", nil},
		{"set expression", "UPDATE users SET n = n + ?", nil},
		{"select", "SELECT CAST(? AS TEXT)", nil},
		{"delete", "DELETE FROM users WHERE id = ?", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parse(t, tc.sql).ValueTargets()
			var want []dialect.ValueTarget
			for _, w := range tc.want {
				want = append(want, dialect.ValueTarget{Column: w.col, Loc: nthQ(t, tc.sql, w.nth)})
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ValueTargets(%q)\n got  %+v\n want %+v", tc.sql, got, want)
			}
		})
	}
}
