package sqlite

import (
	"slices"
	"testing"
)

// TestFrontend_DeepTables_WindowSubquery pins the audit-13 finding: a
// designated table reached ONLY through a subquery inside a window
// OVER(...) clause or a named WINDOW definition was invisible to the
// tableWalker (it descended a *rsql.Call's Args/Filter but not its Over,
// and a SelectStatement's clauses but not its Windows), so DeepTables
// undercounted and the policy weaver neither wove nor refused it — the
// same silent-leak class as the H5 IN-operand blind spot, relocated to
// the window clause. DeepTables must now surface the table. PG/MySQL
// facades already walk the full AST and were never affected.
func TestFrontend_DeepTables_WindowSubquery(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{
			"over-partition-subquery",
			"SELECT t.x, row_number() OVER (PARTITION BY (SELECT max(id) FROM orders)) AS rn FROM t",
			[]string{"orders", "t"},
		},
		{
			"over-orderby-subquery",
			"SELECT row_number() OVER (ORDER BY (SELECT max(id) FROM orders)) FROM t",
			[]string{"orders", "t"},
		},
		{
			"named-window-subquery",
			"SELECT row_number() OVER w FROM t WINDOW w AS (ORDER BY (SELECT max(id) FROM orders))",
			[]string{"orders", "t"},
		},
		{
			"over-filter-subquery-control",
			// FILTER was already walked; keep it green as a control.
			"SELECT count(*) FILTER (WHERE x IN orders) OVER () FROM t",
			[]string{"orders", "t"},
		},
		{
			// negative: a window over non-subquery expressions adds no table.
			"over-plain-no-extra-table",
			"SELECT row_number() OVER (PARTITION BY t.x ORDER BY t.y) FROM t",
			[]string{"t"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tree, err := Frontend{}.Parse(tc.sql)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.sql, err)
			}
			var got []string
			for _, tr := range tree.DeepTables() {
				got = append(got, tr.Name)
			}
			slices.Sort(got)
			got = slices.Compact(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("DeepTables(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}
