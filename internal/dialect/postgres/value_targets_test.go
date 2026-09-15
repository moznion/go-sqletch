package postgres

import (
	"reflect"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// nthIndex returns the byte offset of the n-th (0-based) occurrence of
// sub in s, failing the test when there is none.
func nthIndex(t *testing.T, s, sub string, n int) int {
	t.Helper()
	off := 0
	for i := 0; ; i++ {
		j := strings.Index(s[off:], sub)
		if j < 0 {
			t.Fatalf("occurrence %d of %q not found in %q", n, sub, s)
		}
		if i == n {
			return off + j
		}
		off += j + len(sub)
	}
}

type wantTarget struct {
	col, qual string
	ph        string // placeholder token whose occurrence is the Loc
	nth       int
}

// ValueTargets is design 20 §3.1's facade: only a placeholder that IS
// the whole value (casts and parentheses looked through) of an INSERT
// VALUES item with an explicit column list, or of a single-column
// UPDATE SET item, is a direct value position.
func TestValueTargets(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []wantTarget
	}{
		{"insert values", "INSERT INTO users (email, nickname) VALUES ($1, $2)",
			[]wantTarget{{"email", "", "$1", 0}, {"nickname", "", "$2", 0}}},
		{"multi-row values", "INSERT INTO users (email, nickname) VALUES ($1, $2), ($3, $4)",
			[]wantTarget{{"email", "", "$1", 0}, {"nickname", "", "$2", 0}, {"email", "", "$3", 0}, {"nickname", "", "$4", 0}}},
		{"reused placeholder reports each occurrence", "INSERT INTO users (email, nickname) VALUES ($1, $1)",
			[]wantTarget{{"email", "", "$1", 0}, {"nickname", "", "$1", 1}}},
		{"casts and parentheses are looked through",
			"INSERT INTO users (a, b, c, d) VALUES ($1::text, CAST($2 AS text), $3::text::user_status, (($4)))",
			[]wantTarget{{"a", "", "$1", 0}, {"b", "", "$2", 0}, {"c", "", "$3", 0}, {"d", "", "$4", 0}}},
		{"cast of parenthesized placeholder", "INSERT INTO users (a) VALUES (($1)::text)",
			[]wantTarget{{"a", "", "$1", 0}}},
		{"DEFAULT item is skipped, its row neighbours are not", "INSERT INTO users (a, b) VALUES (DEFAULT, $1)",
			[]wantTarget{{"b", "", "$1", 0}}},
		{"quoted column keeps its spelling", `INSERT INTO users ("NickName") VALUES ($1)`,
			[]wantTarget{{"NickName", "", "$1", 0}}},
		{"expressions are not direct", "INSERT INTO users (a, b, c, d) VALUES (lower($1), $2 || 'x', COALESCE($3, 'y'), $4 COLLATE \"C\")", nil},
		{"negation is not direct", "INSERT INTO users (a) VALUES (-$1)", nil},
		{"scalar subquery is not direct", "INSERT INTO users (a) VALUES ((SELECT $1))", nil},
		{"insert without column list", "INSERT INTO users VALUES ($1, $2)", nil},
		{"insert select", "INSERT INTO users (a) SELECT $1", nil},
		{"values set operation", "INSERT INTO users (a) VALUES ($1) UNION VALUES ($2)", nil},
		{"row arity mismatch is skipped", "INSERT INTO users (a, b) VALUES ($1)", nil},
		{"subfield column target", "INSERT INTO users (comp.f, arr[1]) VALUES ($1, $2)", nil},
		{"conflict arm is not a value position",
			"INSERT INTO users (id, nickname) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET nickname = $2",
			[]wantTarget{{"id", "", "$1", 0}, {"nickname", "", "$2", 0}}},
		{"excluded idiom", "INSERT INTO users (id, nickname) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET nickname = EXCLUDED.nickname",
			[]wantTarget{{"id", "", "$1", 0}, {"nickname", "", "$2", 0}}},
		{"returning and with are not value positions",
			"WITH s AS (SELECT $3 AS x) INSERT INTO users (a) VALUES ($1) RETURNING $2",
			[]wantTarget{{"a", "", "$1", 0}}},
		{"update set", "UPDATE users SET nickname = $1, email = $2::text WHERE id = $3",
			[]wantTarget{{"nickname", "", "$1", 0}, {"email", "", "$2", 0}}},
		{"update set with alias and from", "UPDATE users AS u SET nickname = $1 FROM orgs AS o WHERE o.id = u.org_id AND o.name = $2",
			[]wantTarget{{"nickname", "", "$1", 0}}},
		{"tuple set", "UPDATE users SET (a, b) = ($1, $2)", nil},
		{"subfield set", "UPDATE users SET arr[1] = $1, comp.f = $2", nil},
		{"set expression", "UPDATE users SET n = n + $1", nil},
		{"insert inside cte is not top level", "WITH x AS (INSERT INTO users (a) VALUES ($1) RETURNING a) SELECT a FROM x", nil},
		{"select", "SELECT $1::int", nil},
		{"delete", "DELETE FROM users WHERE id = $1", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustParse(t, tc.sql).ValueTargets()
			var want []dialect.ValueTarget
			for _, w := range tc.want {
				want = append(want, dialect.ValueTarget{Column: w.col, Qualifier: w.qual, Loc: nthIndex(t, tc.sql, w.ph, w.nth)})
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ValueTargets(%q)\n got  %+v\n want %+v", tc.sql, got, want)
			}
		})
	}
}
