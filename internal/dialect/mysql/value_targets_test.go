package mysql

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
	col, qual string
	nth       int // which '?' is the Loc
}

// ValueTargets is design 20 §3.1's facade for MySQL: TiDB's
// FuncCastExpr (CAST, CONVERT(…, T), BINARY) and ParenthesesExpr are
// looked through; everything else is an expression.
func TestValueTargets(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []wantTarget
	}{
		{"insert values", "INSERT INTO users (email, nickname) VALUES (?, ?)",
			[]wantTarget{{"email", "", 0}, {"nickname", "", 1}}},
		{"multi-row values", "INSERT INTO users (email, nickname) VALUES (?, ?), (?, ?)",
			[]wantTarget{{"email", "", 0}, {"nickname", "", 1}, {"email", "", 2}, {"nickname", "", 3}}},
		{"replace and insert ignore", "REPLACE INTO users (email) VALUES (?)",
			[]wantTarget{{"email", "", 0}}},
		{"insert ignore", "INSERT IGNORE INTO users (email) VALUES (?)",
			[]wantTarget{{"email", "", 0}}},
		{"casts and parentheses are looked through",
			"INSERT INTO users (a, b, c, d, e) VALUES (CAST(? AS CHAR), CONVERT(?, CHAR), BINARY ?, CAST(CAST(? AS CHAR) AS JSON), ((?)))",
			[]wantTarget{{"a", "", 0}, {"b", "", 1}, {"c", "", 2}, {"d", "", 3}, {"e", "", 4}}},
		{"DEFAULT item is skipped", "INSERT INTO users (a, b) VALUES (DEFAULT, ?)",
			[]wantTarget{{"b", "", 0}}},
		{"multibyte text before the placeholder", "INSERT INTO users (a, b) VALUES ('日本語', ?)",
			[]wantTarget{{"b", "", 0}}},
		{"quoted column keeps its spelling", "INSERT INTO users (`NickName`) VALUES (?)",
			[]wantTarget{{"NickName", "", 0}}},
		{"qualified insert column", "INSERT INTO users (users.nickname) VALUES (?)",
			[]wantTarget{{"nickname", "users", 0}}},
		{"expressions are not direct", "INSERT INTO users (a, b, c) VALUES (CONVERT(? USING utf8mb4), COALESCE(?, 'x'), LOWER(?))", nil},
		{"arithmetic is not direct", "INSERT INTO users (a) VALUES (? + 1)", nil},
		{"insert without column list", "INSERT INTO users VALUES (?, ?)", nil},
		{"insert select", "INSERT INTO users (a) SELECT ?", nil},
		{"insert set form", "INSERT INTO users SET a = ?", nil},
		{"row arity mismatch is skipped", "INSERT INTO users (a, b) VALUES (?)", nil},
		{"on duplicate key arm is not a value position",
			"INSERT INTO users (id, nickname) VALUES (?, ?) ON DUPLICATE KEY UPDATE nickname = ?",
			[]wantTarget{{"id", "", 0}, {"nickname", "", 1}}},
		{"row alias idiom", "INSERT INTO users (id, nickname) VALUES (?, ?) AS new ON DUPLICATE KEY UPDATE nickname = new.nickname",
			[]wantTarget{{"id", "", 0}, {"nickname", "", 1}}},
		{"update set", "UPDATE users SET nickname = ?, email = CAST(? AS CHAR) WHERE id = ?",
			[]wantTarget{{"nickname", "", 0}, {"email", "", 1}}},
		{"update set qualified by alias", "UPDATE users AS u SET u.nickname = ? WHERE u.id = ?",
			[]wantTarget{{"nickname", "u", 0}}},
		{"update order limit", "UPDATE users SET nickname = ? ORDER BY id LIMIT 1",
			[]wantTarget{{"nickname", "", 0}}},
		{"multi-table update is skipped", "UPDATE users AS u JOIN orgs AS o ON o.id = u.org_id SET u.nickname = ? WHERE o.name = ?", nil},
		{"comma multi-table update is skipped", "UPDATE users AS u, orgs AS o SET u.nickname = ? WHERE o.id = u.org_id", nil},
		{"schema-qualified set column is skipped", "UPDATE users SET app.users.nickname = ?", nil},
		{"set expression", "UPDATE users SET n = n + ?", nil},
		{"select", "SELECT CAST(? AS CHAR)", nil},
		{"delete", "DELETE FROM users WHERE id = ?", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parse(t, tc.sql).ValueTargets()
			var want []dialect.ValueTarget
			for _, w := range tc.want {
				want = append(want, dialect.ValueTarget{Column: w.col, Qualifier: w.qual, Loc: nthQ(t, tc.sql, w.nth)})
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ValueTargets(%q)\n got  %+v\n want %+v", tc.sql, got, want)
			}
		})
	}
}
