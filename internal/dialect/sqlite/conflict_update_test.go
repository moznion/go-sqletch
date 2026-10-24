package sqlite

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// HasConflictUpdate reports SQLite's ON CONFLICT … DO UPDATE (a
// row-modifying upsert arm) — the policy weaver's audit-12 M10 input.
// DO NOTHING modifies nothing and must report false.
//
// The REPLACE conflict-resolution algorithm (F17) is a row-modifying
// conflict arm too: INSERT OR REPLACE / REPLACE INTO / UPDATE OR REPLACE
// DELETE whichever existing row collides on a UNIQUE/PRIMARY KEY
// constraint — another tenant's included. The other algorithms (IGNORE,
// ABORT, FAIL, ROLLBACK) never touch an existing row and must stay false.
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
		{"insert or replace", "INSERT OR REPLACE INTO orders (id, v) VALUES (1, 2)", true},
		{"replace into", "REPLACE INTO orders (id, v) VALUES (1, 2)", true},
		{"insert or replace select", "INSERT OR REPLACE INTO orders (id, v) SELECT id, v FROM staging", true},
		{"update or replace", "UPDATE OR REPLACE orders SET id = 3 WHERE id = 2", true},
		{"insert or ignore", "INSERT OR IGNORE INTO orders (id, v) VALUES (1, 2)", false},
		{"insert or abort", "INSERT OR ABORT INTO orders (id, v) VALUES (1, 2)", false},
		{"insert or fail", "INSERT OR FAIL INTO orders (id, v) VALUES (1, 2)", false},
		{"insert or rollback", "INSERT OR ROLLBACK INTO orders (id, v) VALUES (1, 2)", false},
		{"update or ignore", "UPDATE OR IGNORE orders SET v = 1 WHERE id = 2", false},
		{"update or abort", "UPDATE OR ABORT orders SET v = 1 WHERE id = 2", false},
		{"update or fail", "UPDATE OR FAIL orders SET v = 1 WHERE id = 2", false},
		{"update or rollback", "UPDATE OR ROLLBACK orders SET v = 1 WHERE id = 2", false},
		{"delete", "DELETE FROM orders WHERE id = 2", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parse(t, tc.sql).HasConflictUpdate(); got != tc.want {
				t.Errorf("HasConflictUpdate(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}

// HasConflictDelete reports the REPLACE algorithm only: it DELETES the
// colliding row. ON CONFLICT … DO UPDATE rewrites but never deletes, and
// the other algorithms touch no existing row.
func TestHasConflictDelete(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want bool
	}{
		{"insert or replace", "INSERT OR REPLACE INTO orders (id, v) VALUES (1, 2)", true},
		{"replace into", "REPLACE INTO orders (id, v) VALUES (1, 2)", true},
		{"update or replace", "UPDATE OR REPLACE orders SET id = 3 WHERE id = 2", true},
		{"do update", "INSERT INTO orders (id, v) VALUES (1, 2) ON CONFLICT (id) DO UPDATE SET v = 3", false},
		{"plain insert values", "INSERT INTO orders (id, v) VALUES (1, 2)", false},
		{"insert or ignore", "INSERT OR IGNORE INTO orders (id, v) VALUES (1, 2)", false},
		{"update or ignore", "UPDATE OR IGNORE orders SET v = 1 WHERE id = 2", false},
		{"update", "UPDATE orders SET v = 1 WHERE id = 2", false},
		{"select", "SELECT id FROM orders", false},
		{"delete", "DELETE FROM orders WHERE id = 2", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cd, ok := parse(t, tc.sql).(dialect.ConflictDeleter)
			if !ok {
				t.Fatalf("SQLite tree does not implement dialect.ConflictDeleter")
			}
			if got := cd.HasConflictDelete(); got != tc.want {
				t.Errorf("HasConflictDelete(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}
