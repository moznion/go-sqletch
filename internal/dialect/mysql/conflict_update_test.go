package mysql

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// HasConflictUpdate reports MySQL's ON DUPLICATE KEY UPDATE (a
// row-modifying upsert arm) and REPLACE (which deletes the colliding row
// before inserting) — the policy weaver's audit-12 M10 input.
func TestHasConflictUpdate(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want bool
	}{
		{"on duplicate key update", "INSERT INTO orders (id, v) VALUES (1, 2) ON DUPLICATE KEY UPDATE v = 3", true},
		{"replace into values", "REPLACE INTO orders (id, v) VALUES (1, 2)", true},
		{"replace values without into", "REPLACE orders (id, v) VALUES (1, 2)", true},
		{"replace set", "REPLACE INTO orders SET id = 1, v = 2", true},
		{"replace select", "REPLACE INTO orders (id) SELECT id FROM src", true},
		{"plain insert values", "INSERT INTO orders (id, v) VALUES (1, 2)", false},
		{"insert ignore", "INSERT IGNORE INTO orders (id, v) VALUES (1, 2)", false},
		{"insert set", "INSERT INTO orders SET id = 1, v = 2", false},
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

// HasConflictDelete reports REPLACE only: its conflict handling DELETES
// the colliding row. ON DUPLICATE KEY UPDATE modifies but never deletes.
func TestHasConflictDelete(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want bool
	}{
		{"replace into values", "REPLACE INTO orders (id, v) VALUES (1, 2)", true},
		{"replace set", "REPLACE INTO orders SET id = 1, v = 2", true},
		{"replace select", "REPLACE INTO orders (id) SELECT id FROM src", true},
		{"on duplicate key update", "INSERT INTO orders (id, v) VALUES (1, 2) ON DUPLICATE KEY UPDATE v = 3", false},
		{"plain insert values", "INSERT INTO orders (id, v) VALUES (1, 2)", false},
		{"insert ignore", "INSERT IGNORE INTO orders (id, v) VALUES (1, 2)", false},
		{"select", "SELECT id FROM orders", false},
		{"delete", "DELETE FROM orders WHERE id = 2", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := parse(t, tc.sql)
			cd, ok := tr.(dialect.ConflictDeleter)
			if !ok {
				t.Fatalf("MySQL tree does not implement dialect.ConflictDeleter")
			}
			if got := cd.HasConflictDelete(); got != tc.want {
				t.Errorf("HasConflictDelete(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}
