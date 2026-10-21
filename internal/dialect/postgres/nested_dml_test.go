package postgres

import (
	"reflect"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// NestedDML reports every data-modifying CTE body (kind + target), at
// any depth and under any outer kind — the policy weaver's input for
// security finding F9 (owner decision 2026-10-21). The top-level
// statement is never listed.
func TestNestedDML(t *testing.T) {
	type kt struct {
		kind   dialect.StmtKind
		table  string
		upsert bool
	}
	cases := []struct {
		name string
		sql  string
		want []kt
	}{
		{"plain select", "SELECT id FROM posts", nil},
		{"plain update", "UPDATE posts SET v = 1 WHERE id = 2", nil},
		{"plain upsert is top-level", "INSERT INTO posts (id) VALUES (1) ON CONFLICT (id) DO UPDATE SET id = excluded.id", nil},
		{"select cte", "WITH c AS (SELECT id FROM posts) SELECT id FROM c", nil},
		{"delete under insert",
			"WITH d AS (DELETE FROM posts WHERE id = 1 RETURNING *) INSERT INTO posts_archive SELECT * FROM d",
			[]kt{{dialect.StmtDelete, "posts", false}}},
		{"update under select",
			"WITH u AS (UPDATE posts SET v = 1 WHERE id = 1 RETURNING id) SELECT id FROM u",
			[]kt{{dialect.StmtUpdate, "posts", false}}},
		{"upsert under select",
			"WITH i AS (INSERT INTO posts (id) VALUES (1) ON CONFLICT (id) DO UPDATE SET id = excluded.id RETURNING id) SELECT id FROM i",
			[]kt{{dialect.StmtInsert, "posts", true}}},
		{"do-nothing insert under select",
			"WITH i AS (INSERT INTO posts (id) VALUES (1) ON CONFLICT (id) DO NOTHING RETURNING id) SELECT id FROM i",
			[]kt{{dialect.StmtInsert, "posts", false}}},
		{"two wctes under delete, document order",
			"WITH a AS (UPDATE s.posts SET v = 1 RETURNING id), b AS (DELETE FROM logs RETURNING id) DELETE FROM other WHERE id IN (SELECT id FROM a)",
			[]kt{{dialect.StmtUpdate, "posts", false}, {dialect.StmtDelete, "logs", false}}},
		{"wcte inside a subquery's own WITH is still found",
			"SELECT * FROM (WITH d AS (DELETE FROM posts RETURNING id) SELECT id FROM d) x",
			[]kt{{dialect.StmtDelete, "posts", false}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []kt
			for _, d := range mustParse(t, tc.sql).NestedDML() {
				got = append(got, kt{d.Kind, d.Table, d.Upsert})
				if d.Loc < 0 {
					t.Errorf("nested %v on %q has no location", d.Kind, d.Table)
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("NestedDML(%q) = %+v, want %+v", tc.sql, got, tc.want)
			}
		})
	}
	if d := mustParse(t, "WITH a AS (UPDATE s.posts SET v = 1 RETURNING id) SELECT id FROM a").NestedDML(); len(d) != 1 || d[0].Schema != "s" {
		t.Errorf("schema qualifier not reported: %+v", d)
	}
}
