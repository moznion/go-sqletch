package sqlite

import (
	"strings"
	"testing"
)

// TestColumnRefs_EveryExpressionPosition pins refWalker's coverage of
// the rqlite AST, position by position. ColumnRefs feeds R3 (a skeleton
// reference to a guarded relation's column), so a position the walker
// skips is a reference R3 never sees — the same blind-spot class the
// tableWalker had for windows, upserts and RETURNING (audits 13/14:
// the hand walkers must be diffed against rqlite's node kinds). Each
// case plants the sentinel `g.secret` in exactly one position and
// requires it back with its byte offset and subquery marking.
func TestColumnRefs_EveryExpressionPosition(t *testing.T) {
	tests := []struct {
		name  string
		sql   string
		inSub bool
	}{
		// Window definitions, inline (Call.Over) and named (WINDOW).
		{"over partition", "SELECT sum(u.id) OVER (PARTITION BY g.secret) FROM users AS u, g", false},
		{"over order by", "SELECT sum(u.id) OVER (ORDER BY g.secret) FROM users AS u, g", false},
		{"over frame bound", "SELECT sum(u.id) OVER (ORDER BY u.id ROWS g.secret PRECEDING) FROM users AS u, g", false},
		{"named window", "SELECT sum(u.id) OVER w FROM users AS u, g WINDOW w AS (PARTITION BY g.secret)", false},
		{"aggregate filter", "SELECT count(*) FILTER (WHERE g.secret > 0) FROM users AS u, g", false},

		// Expression node kinds.
		{"case operand", "SELECT CASE g.secret WHEN 1 THEN 2 END FROM g", false},
		{"case condition", "SELECT CASE WHEN g.secret THEN 2 END FROM g", false},
		{"case body", "SELECT CASE WHEN 1 THEN g.secret END FROM g", false},
		{"case else", "SELECT CASE WHEN 1 THEN 2 ELSE g.secret END FROM g", false},
		{"unary", "SELECT -g.secret FROM g", false},
		{"not", "SELECT 1 FROM g WHERE NOT g.secret", false},
		{"collate", "SELECT 1 FROM g ORDER BY g.secret COLLATE NOCASE", false},
		{"between low", "SELECT 1 FROM g WHERE 1 BETWEEN g.secret AND 3", false},
		{"between high", "SELECT 1 FROM g WHERE 1 BETWEEN 0 AND g.secret", false},
		{"isnull", "SELECT 1 FROM g WHERE g.secret ISNULL", false},
		{"cast", "SELECT CAST(g.secret AS TEXT) FROM g", false},
		{"group by", "SELECT 1 FROM g GROUP BY g.secret", false},
		{"having", "SELECT 1 FROM g GROUP BY g.id HAVING max(g.secret) > 0", false},
		{"limit", "SELECT 1 FROM g LIMIT g.secret", false},
		{"offset", "SELECT 1 FROM g LIMIT 1 OFFSET g.secret", false},

		// Statement-level positions beyond SELECT.
		{"update returning", "UPDATE g SET id = 1 RETURNING g.secret", false},
		{"delete returning", "DELETE FROM g RETURNING g.secret", false},
		{"insert returning", "INSERT INTO g (id) VALUES (1) RETURNING g.secret", false},
		{"insert values", "INSERT INTO t (a) VALUES (g.secret)", false},
		{"upsert set", "INSERT INTO g (id) VALUES (1) ON CONFLICT (id) DO UPDATE SET id = g.secret", false},
		{"upsert update where", "INSERT INTO g (id) VALUES (1) ON CONFLICT (id) DO UPDATE SET id = 2 WHERE g.secret > 0", false},
		{"upsert target where", "INSERT INTO g (id) VALUES (1) ON CONFLICT (id) WHERE g.secret > 0 DO NOTHING", false},
		{"insert select", "INSERT INTO t (a) SELECT g.secret FROM g", true},
		{"values row subquery", "VALUES ((SELECT g.secret FROM g))", true},

		// Subquery carriers mark the ref InSubquery.
		{"paren derived", "SELECT s.x FROM (SELECT g.secret AS x FROM g) AS s", true},
		{"exists", "SELECT 1 FROM users AS u WHERE EXISTS (SELECT 1 FROM g WHERE g.secret = u.id)", true},
		{"scalar subquery", "SELECT (SELECT g.secret FROM g) FROM users", true},
		{"cte body", "WITH c AS (SELECT g.secret FROM g) SELECT 1 FROM c", true},
		{"window in subquery", "SELECT 1 FROM users AS u WHERE u.id IN (SELECT sum(1) OVER (PARTITION BY g.secret) FROM g)", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			refs := parse(t, tc.sql).ColumnRefs()
			ref, ok := findScope(refs, "g", "secret")
			if !ok {
				t.Fatalf("g.secret not collected from %q; refs = %+v", tc.sql, refs)
			}
			if want := strings.Index(tc.sql, "g.secret"); ref.Loc != want {
				t.Errorf("g.secret Loc = %d, want %d", ref.Loc, want)
			}
			if ref.InSubquery != tc.inSub {
				t.Errorf("g.secret InSubquery = %v, want %v", ref.InSubquery, tc.inSub)
			}
		})
	}
}
