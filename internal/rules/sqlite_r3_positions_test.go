package rules

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
)

// R3 end to end on SQLite for the expression positions the hand-written
// refWalker must descend into (sqlite.TestColumnRefs_EveryExpressionPosition
// pins the walker; this pins that R3 acts on what it collects). A
// skeleton reference to the guarded join's alias `a` exists in only the
// guard-on shapes' FROM, so every case must be SQLETCH115 — a silent
// pass would ship SQL that fails (or, worse, resolves elsewhere) when
// the guard is off.
func TestR3_SQLiteGuardedRefInEveryPosition(t *testing.T) {
	positions := []struct {
		name string
		proj string
	}{
		{"window partition", "sum(u.id) OVER (PARTITION BY a.kind) AS s"},
		{"window order", "sum(u.id) OVER (ORDER BY a.kind) AS s"},
		{"aggregate filter", "count(*) FILTER (WHERE a.kind = 'k') AS s"},
		{"case condition", "CASE WHEN a.kind = 'k' THEN 1 ELSE 0 END AS s"},
		{"case else", "CASE WHEN u.id = 1 THEN 'x' ELSE a.kind END AS s"},
		{"collate", "a.kind COLLATE NOCASE = 'k' AS s"},
		{"between", "u.id BETWEEN 0 AND a.user_id AS s"},
	}
	for _, p := range positions {
		t.Run(p.name, func(t *testing.T) {
			src := `-- name: Bad :many
-- @param x: integer
SELECT u.id, ` + p.proj + ` FROM users AS u
@if-present(x)
JOIN audits AS a ON a.user_id = u.id AND a.id = :x
@endif
WHERE TRUE
;
`
			diags := checkResolvedDialect(t, sqlite.Profile{}, sqlite.Frontend{}, src)
			if !hasCode(diags, diagnostics.CodeScopeViolation) {
				t.Fatalf("want SQLETCH115 for a guarded ref in %s, got %+v", p.name, diags)
			}
			// The diagnostic must point at the offending reference, not
			// at the query header: the span is what the editor shows.
			at := strings.Index(src, "a.kind")
			if at < 0 {
				at = strings.Index(src, "a.user_id AS")
			}
			for _, d := range diags {
				if d.Code == diagnostics.CodeScopeViolation && d.Span.Start != at {
					t.Errorf("SQLETCH115 span starts at %d, want the reference at %d", d.Span.Start, at)
				}
			}
		})
	}
}
