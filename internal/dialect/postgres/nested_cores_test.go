package postgres

import (
	"slices"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// NestedCores lists the leaf SELECT cores the policy weaver may scope
// below the statement's own cores (design 14 §14, §15): non-recursive
// WITH bodies, FROM-position subqueries, and EXISTS/ANY/ALL/scalar
// sublinks in a WHERE or HAVING, recursively, set operations flattened
// — never a sublink in any other clause or of another kind, never a
// WITH RECURSIVE body. Each core's
// relations are located in the original SQL.
func TestFrontend_NestedCores(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want [][]string // per core, its own relations ("table" or "table alias")
	}{
		{name: "plain statement", sql: "SELECT id FROM orders", want: nil},
		{name: "IN sublink in WHERE", sql: "SELECT id FROM users WHERE id IN (SELECT user_id FROM orders)", want: [][]string{{"orders"}}},
		{name: "SELECT-list sublink is not a nested core", sql: "SELECT (SELECT max(id) FROM orders) FROM users", want: nil},
		{name: "ORDER BY sublink is not a nested core", sql: "SELECT id FROM users ORDER BY (SELECT max(id) FROM orders)", want: nil},
		{name: "ON sublink is not a nested core", sql: "SELECT u.id FROM users u JOIN acc a ON a.id IN (SELECT id FROM orders)", want: nil},
		{name: "ARRAY sublink is not a nested core", sql: "SELECT id FROM users WHERE ids @> ARRAY(SELECT id FROM orders)", want: nil},
		{
			name: "every eligible kind, in document order",
			sql: "SELECT id FROM u WHERE EXISTS (SELECT 1 FROM t1) AND NOT EXISTS (SELECT 1 FROM t2) AND id IN (SELECT id FROM t3) " +
				"AND id NOT IN (SELECT id FROM t4) AND id = ANY (SELECT id FROM t5) AND id > ALL (SELECT id FROM t6) AND id = (SELECT max(id) FROM t7)",
			want: [][]string{{"t1"}, {"t2"}, {"t3"}, {"t4"}, {"t5"}, {"t6"}, {"t7"}},
		},
		{
			name: "WHERE sublinks before HAVING sublinks, after FROM subqueries",
			sql:  "SELECT k FROM (SELECT k FROM t1) d WHERE k IN (SELECT k FROM t2) GROUP BY k HAVING count(*) > (SELECT count(*) FROM t3)",
			want: [][]string{{"t1"}, {"t2"}, {"t3"}},
		},
		{
			name: "sublinks inside CASE, function arguments and a test expression",
			sql:  "SELECT id FROM u WHERE CASE WHEN ok THEN id IN (SELECT id FROM t1) END AND coalesce((SELECT max(id) FROM t2), 0) > 0 AND (SELECT max(id) FROM t3) IN (SELECT id FROM t4)",
			want: [][]string{{"t1"}, {"t2"}, {"t3"}, {"t4"}},
		},
		{
			name: "sublink inside a sublink, and a set operation inside one",
			sql:  "SELECT id FROM u WHERE EXISTS (SELECT 1 FROM t1 WHERE t1.id IN (SELECT id FROM t2 UNION SELECT id FROM t3))",
			want: [][]string{{"t1"}, {"t2"}, {"t3"}},
		},
		{
			name: "a sublink's own WITH and FROM subquery",
			sql:  "SELECT id FROM u WHERE EXISTS (WITH z AS (SELECT id FROM t1) SELECT 1 FROM z JOIN (SELECT id FROM t2) q ON true)",
			want: [][]string{{"t1"}, {"z", " q"}, {"t2"}},
		},
		{name: "a WITH RECURSIVE list inside a sublink is not walked", sql: "SELECT id FROM u WHERE EXISTS (WITH RECURSIVE z AS (SELECT id FROM t1) SELECT 1 FROM z)", want: [][]string{{"z"}}},
		{name: "CTE body", sql: "WITH x AS (SELECT id FROM orders o) SELECT id FROM x", want: [][]string{{"orders o"}}},
		{
			name: "set operation inside a CTE body is flattened",
			sql:  "WITH a AS (SELECT id FROM users u UNION ALL SELECT id FROM orders o) SELECT id FROM a",
			want: [][]string{{"users u"}, {"orders o"}},
		},
		{
			name: "several CTEs, then FROM subqueries in FROM order",
			sql:  "WITH x AS (SELECT 1 AS id FROM t1), y AS (SELECT id FROM t2) SELECT * FROM x JOIN (SELECT id FROM t3) d ON true LEFT JOIN LATERAL (SELECT id FROM t4 WHERE t4.id = d.id) l ON true",
			want: [][]string{{"t1"}, {"t2"}, {"t3"}, {"t4"}},
		},
		{
			name: "nested in nested",
			sql:  "SELECT * FROM (WITH z AS (SELECT id FROM t1) SELECT id FROM z JOIN (SELECT id FROM t2) q USING (id)) d",
			// WITH bodies precede their query's own core; a derived table is listed
			// in its parent core's relations with an empty table name.
			want: [][]string{{"t1"}, {"z", " q"}, {"t2"}},
		},
		{
			name: "a WHERE sublink inside a CTE body",
			sql:  "WITH x AS (SELECT id FROM t1 WHERE id IN (SELECT id FROM t2)) SELECT id FROM x",
			want: [][]string{{"t1"}, {"t2"}},
		},
		{name: "WITH RECURSIVE is never walked", sql: "WITH RECURSIVE x AS (SELECT id FROM t1) SELECT id FROM x", want: nil},
		{
			name: "set-level WITH and a branch's FROM subquery",
			sql:  "WITH x AS (SELECT id FROM t1) SELECT id FROM x UNION SELECT id FROM (SELECT id FROM t2) d",
			want: [][]string{{"t1"}, {"t2"}},
		},
		{name: "a DML statement has no nested cores", sql: "WITH x AS (SELECT id FROM t1) DELETE FROM orders WHERE id IN (SELECT id FROM x)", want: nil},
		{name: "a data-modifying CTE body is not a core", sql: "WITH d AS (DELETE FROM t1 RETURNING id) SELECT id FROM d", want: nil},
	}
	fe := Frontend{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tree, err := fe.Parse(tc.sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			cores := tree.NestedCores()
			var got [][]string
			for i, c := range cores {
				if c.Kind() != dialect.StmtSelect {
					t.Errorf("core %d: Kind = %v, want StmtSelect", i, c.Kind())
				}
				rels := []string{}
				for _, r := range c.Relations() {
					s := r.Table
					if r.Alias != "" {
						s += " " + r.Alias
					}
					rels = append(rels, s)
					if r.Table != "" && (r.Loc < 0 || !strings.HasPrefix(strings.ToLower(tc.sql[r.Loc:]), strings.ToLower(r.Table))) {
						t.Errorf("core %d: relation %q Loc=%d does not point at its name", i, r.Table, r.Loc)
					}
				}
				got = append(got, rels)
			}
			if !slices.EqualFunc(got, tc.want, slices.Equal[[]string]) {
				t.Errorf("NestedCores relations = %v, want %v", got, tc.want)
			}
			// Deterministic: a second walk yields the same list.
			if again := tree.NestedCores(); len(again) != len(cores) {
				t.Errorf("second walk: %d cores, want %d", len(again), len(cores))
			}
		})
	}
}
