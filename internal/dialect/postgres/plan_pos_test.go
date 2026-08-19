package postgres

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// A plan-stage PgError position is 1-based against the EXPLAIN-prefixed
// string; after mapOracleErr + ShiftOracleErrPos it must be the 0-based
// rendering-relative offset, with no residual prefix drift.
func TestPlanErrorOffsetIsRenderingRelative(t *testing.T) {
	const prefixLen = len(genericPlanPrefix) // 23

	for _, renderOff := range []int{0, 5, 42} {
		// PgError.Position is 1-based over the prefixed string.
		pgPos := int32(prefixLen + renderOff + 1)
		mapped := dialect.ShiftOracleErrPos(
			mapOracleErr(&pgconn.PgError{Position: pgPos, Code: "42601", Message: "syntax error"}),
			prefixLen)
		oe, ok := mapped.(*dialect.OracleError)
		if !ok {
			t.Fatalf("want *OracleError, got %T", mapped)
		}
		if oe.Pos != renderOff {
			t.Errorf("Position=%d → Pos=%d, want rendering offset %d", pgPos, oe.Pos, renderOff)
		}
	}

	// An error landing inside the stripped prefix is unpositioned, never
	// mis-attributed into the rendering.
	inPrefix := dialect.ShiftOracleErrPos(
		mapOracleErr(&pgconn.PgError{Position: 3, Code: "42601", Message: "x"}),
		prefixLen)
	if oe := inPrefix.(*dialect.OracleError); oe.Pos != -1 {
		t.Errorf("prefix-internal error Pos=%d, want -1", oe.Pos)
	}

	// A non-positional error stays unpositioned.
	noPos := dialect.ShiftOracleErrPos(
		mapOracleErr(&pgconn.PgError{Position: 0, Code: "42601", Message: "x"}),
		prefixLen)
	if oe := noPos.(*dialect.OracleError); oe.Pos != -1 {
		t.Errorf("non-positional error Pos=%d, want -1", oe.Pos)
	}
}
