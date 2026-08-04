package postgres

import (
	"slices"
	"testing"
)

// DeepTables must see every base-table reference — subqueries and CTE
// bodies included — where Relations deliberately stops at the
// statement's own FROM/target clauses (design 14 §11.1).
func TestFrontend_DeepTables(t *testing.T) {
	cases := []struct {
		sql  string
		want []string // sorted table names
	}{
		{"SELECT o.id FROM orders o JOIN users u ON u.id = o.user_id", []string{"orders", "users"}},
		{"SELECT id FROM orders WHERE user_id IN (SELECT id FROM users)", []string{"orders", "users"}},
		{"SELECT s.n FROM (SELECT count(*) AS n FROM invoices) s", []string{"invoices"}},
		{"WITH recent AS (SELECT * FROM orders) SELECT * FROM recent", []string{"orders", "recent"}},
		{"INSERT INTO archive SELECT * FROM orders", []string{"archive", "orders"}},
		{"UPDATE orders SET total = 0 WHERE id IN (SELECT order_id FROM order_items)", []string{"order_items", "orders"}},
		{"DELETE FROM orders WHERE EXISTS (SELECT 1 FROM users WHERE users.id = orders.user_id)", []string{"orders", "users"}},
		{"SELECT 1", nil},
	}
	for _, tc := range cases {
		tree, err := Frontend{}.Parse(tc.sql)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.sql, err)
		}
		var got []string
		for _, tr := range tree.DeepTables() {
			got = append(got, tr.Name)
		}
		slices.Sort(got)
		if !slices.Equal(got, tc.want) {
			t.Errorf("DeepTables(%q) = %v, want %v", tc.sql, got, tc.want)
		}
	}

	// Locations point at the reference (usable for spans).
	tree, err := Frontend{}.Parse("SELECT id FROM orders WHERE user_id IN (SELECT id FROM users)")
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range tree.DeepTables() {
		if tr.Loc < 0 {
			t.Errorf("table %q has no location", tr.Name)
		}
	}
}
