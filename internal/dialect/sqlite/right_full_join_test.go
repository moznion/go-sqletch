package sqlite

import (
	"errors"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// Audit-18: rqlite/sql predates SQLite 3.39's RIGHT/FULL JOIN and has no
// operator for them — it silently parses `a RIGHT JOIN b` as
// `a AS "RIGHT" INNER JOIN b`. The real engine runs a true RIGHT/FULL
// OUTER JOIN, so the analyzer saw INNER and missed the null-extension,
// narrowing a NOT NULL column on the preserved side that the engine can
// return NULL for (silent for BLOB columns). The frontend must reject the
// construct (its documented contract) rather than mis-analyze it.
func TestParse_RejectsRightFullJoin(t *testing.T) {
	reject := []string{
		"SELECT x FROM a RIGHT JOIN b USING(id)",
		"SELECT x FROM a FULL JOIN b USING(id)",
		"SELECT x FROM a RIGHT OUTER JOIN b ON a.id=b.id",
		"SELECT x FROM a FULL OUTER JOIN b ON a.id=b.id",
		"SELECT x FROM a right join b using(id)",              // case-insensitive
		"SELECT x FROM t1 JOIN a RIGHT JOIN b ON a.id=b.id",   // nested
		"SELECT x FROM (SELECT 1) s RIGHT JOIN b ON s.c=b.id", // after derived
	}
	for _, sql := range reject {
		_, err := Frontend{}.Parse(sql)
		if err == nil {
			t.Errorf("expected rejection of RIGHT/FULL JOIN, got nil for %q", sql)
			continue
		}
		var pe *dialect.ParseError
		if !errors.As(err, &pe) || !strings.Contains(pe.Msg, "RIGHT/FULL JOIN") {
			t.Errorf("%q: expected a RIGHT/FULL JOIN ParseError, got %v", sql, err)
		}
	}
}

// A table deliberately aliased `right`/`full` via AS is an INNER join in
// both rqlite and the engine, so it must NOT be rejected. LEFT/INNER/CROSS
// joins and a `right`/`full` column reference also stay accepted.
func TestParse_RightFullJoin_NoFalsePositive(t *testing.T) {
	accept := []string{
		"SELECT x FROM a LEFT JOIN b USING(id)",
		"SELECT x FROM a INNER JOIN b USING(id)",
		"SELECT x FROM a CROSS JOIN b",
		"SELECT x FROM a JOIN b USING(id)",
		"SELECT t.right FROM t",                     // `right` as a column
		"SELECT x FROM a AS right JOIN b USING(id)", // explicit alias -> INNER
		"SELECT x FROM a AS full JOIN b USING(id)",
	}
	for _, sql := range accept {
		if _, err := (Frontend{}).Parse(sql); err != nil {
			t.Errorf("unexpected rejection of %q: %v", sql, err)
		}
	}
}
