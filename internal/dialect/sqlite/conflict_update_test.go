package sqlite

import "testing"

// HasConflictUpdate reports SQLite's ON CONFLICT … DO UPDATE (a
// row-modifying upsert arm) — the policy weaver's audit-12 M10 input.
// DO NOTHING modifies nothing and must report false.
func TestHasConflictUpdate(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want bool
	}{
		{"do update", "INSERT INTO orders (id, v) VALUES (1, 2) ON CONFLICT (id) DO UPDATE SET v = 3", true},
		{"do nothing", "INSERT INTO orders (id, v) VALUES (1, 2) ON CONFLICT (id) DO NOTHING", false},
		{"plain insert values", "INSERT INTO orders (id, v) VALUES (1, 2)", false},
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
