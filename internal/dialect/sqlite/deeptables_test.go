package sqlite

import (
	"slices"
	"testing"
)

func TestFrontend_DeepTables(t *testing.T) {
	cases := []struct {
		sql  string
		want []string
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
}
