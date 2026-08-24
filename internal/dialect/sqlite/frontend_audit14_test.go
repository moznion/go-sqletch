package sqlite

import (
	"slices"
	"testing"
)

// TestFrontend_DeepTables_HiddenReadPositions pins the audit-14 findings:
// the hand-written tableWalker missed three positions that can carry a
// subquery reading a policy-designated table, so DeepTables undercounted
// and the weaver neither wove nor refused — silent tenant leaks of the
// same class as H5 (IN operand) and the window clause (#116). PG/MySQL
// full-AST walks surface all of these; only the SQLite facade missed
// them. Each case names a table (`tenants`) reachable ONLY through the
// position under test.
func TestFrontend_DeepTables_HiddenReadPositions(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		// Finding 1: INSERT ... ON CONFLICT DO UPDATE — the SET
		// assignment expr, the DO UPDATE ... WHERE, and the
		// ON CONFLICT (...) WHERE all bear expressions.
		{
			"upsert-do-update-set-subquery",
			"INSERT INTO orders (id, v) VALUES (1, 2) ON CONFLICT (id) DO UPDATE SET v = (SELECT secret FROM tenants WHERE tenants.id = 1)",
			[]string{"orders", "tenants"},
		},
		{
			"upsert-do-update-where-in-table",
			"INSERT INTO orders (id, v) VALUES (1, 2) ON CONFLICT (id) DO UPDATE SET v = 3 WHERE v IN tenants",
			[]string{"orders", "tenants"},
		},
		{
			"upsert-target-where-subquery",
			"INSERT INTO orders (id, v) VALUES (1, 2) ON CONFLICT (id) WHERE id > (SELECT max(id) FROM tenants) DO NOTHING",
			[]string{"orders", "tenants"},
		},
		// Finding 2: DELETE ... RETURNING subquery (Update/Insert
		// already walked RETURNING; Delete did not).
		{
			"delete-returning-subquery",
			"DELETE FROM orders WHERE id = 1 RETURNING (SELECT secret FROM tenants)",
			[]string{"orders", "tenants"},
		},
		// Finding 3: VALUES-form select rows (standalone, derived, CTE).
		{
			"values-standalone-subquery",
			"VALUES ((SELECT secret FROM tenants))",
			[]string{"tenants"},
		},
		{
			"values-derived-subquery",
			"SELECT * FROM (VALUES ((SELECT secret FROM tenants))) v",
			[]string{"tenants"},
		},
		// Negatives: these positions without a table read add nothing.
		{
			"upsert-plain-no-extra-table",
			"INSERT INTO orders (id, v) VALUES (1, 2) ON CONFLICT (id) DO UPDATE SET v = 3",
			[]string{"orders"},
		},
		{
			"delete-returning-plain",
			"DELETE FROM orders WHERE id = 1 RETURNING id, v",
			[]string{"orders"},
		},
		{
			"values-plain",
			"VALUES (1, 2), (3, 4)",
			nil,
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
