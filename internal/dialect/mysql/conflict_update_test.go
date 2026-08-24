package mysql

import "testing"

// HasConflictUpdate reports MySQL's ON DUPLICATE KEY UPDATE (a
// row-modifying upsert arm) — the policy weaver's audit-12 M10 input.
func TestHasConflictUpdate(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want bool
	}{
		{"on duplicate key update", "INSERT INTO orders (id, v) VALUES (1, 2) ON DUPLICATE KEY UPDATE v = 3", true},
		{"plain insert values", "INSERT INTO orders (id, v) VALUES (1, 2)", false},
		{"insert select", "INSERT INTO orders (id) SELECT id FROM src", false},
		{"select", "SELECT id FROM orders", false},
		{"update", "UPDATE orders SET v = 1 WHERE id = 2", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parse(t, tc.sql).HasConflictUpdate(); got != tc.want {
				t.Errorf("HasConflictUpdate(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}
