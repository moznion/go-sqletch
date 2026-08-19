package sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// A rendering may legally end with a comment after the terminating ';'
// (the scanner preserves post-';' comments in the skeleton). Such a tail
// must not be rejected as a second statement, while a genuine trailing
// statement still is.
func TestPrepareOne_TrailingCommentAccepted(t *testing.T) {
	o := NewOracle(testConn(t))
	ctx := context.Background()

	ok := []string{
		"SELECT id FROM users; -- trailing line comment",
		"SELECT id FROM users; /* trailing block */",
		"SELECT id FROM users;   ",
		"SELECT id FROM users;;",
		"SELECT id FROM users; /* a */ -- b",
	}
	for _, sql := range ok {
		if _, err := o.Describe(ctx, sql); err != nil {
			t.Errorf("legal trailing comment rejected for %q: %v", sql, err)
		}
	}

	bad := []string{
		"SELECT id FROM users; SELECT 1",
		"SELECT id FROM users; -- c\nSELECT 1",
	}
	for _, sql := range bad {
		_, err := o.Describe(ctx, sql)
		var oe *dialect.OracleError
		if err == nil || !asOracleErr(err, &oe) || !strings.Contains(oe.Msg, "multiple statements") {
			t.Errorf("genuine second statement must be rejected for %q: err=%v", sql, err)
		}
	}
}

// tailHasStatement is the pure decision the trailing-comment check rests
// on; pin it directly.
func TestTailHasStatement(t *testing.T) {
	cases := []struct {
		tail string
		want bool
	}{
		{"", false},
		{"   ", false},
		{";", false},
		{" -- note", false},
		{" /* note */ ", false},
		{" /* a */ ; -- b", false},
		{" SELECT 1", true},
		{" ; SELECT 1", true},
		{" INSERT INTO t VALUES (1)", true},
	}
	for _, c := range cases {
		if got := tailHasStatement(c.tail); got != c.want {
			t.Errorf("tailHasStatement(%q) = %v, want %v", c.tail, got, c.want)
		}
	}
}

// Plan-stage error offsets are measured against the EXPLAIN-prefixed
// string but must be reported rendering-relative — identical to the
// Describe path's offset for the same rendering, NOT shifted by the
// 19-byte "EXPLAIN QUERY PLAN " prefix.
func TestPlanRows_OffsetIsRenderingRelative(t *testing.T) {
	o := NewOracle(testConn(t))
	ctx := context.Background()

	// A syntax error at a known offset (the stray comma before FROM).
	sql := "SELECT id, FROM users"
	wantAt := strings.Index(sql, "FROM")

	_, descErr := o.Describe(ctx, sql)
	var de *dialect.OracleError
	if descErr == nil || !asOracleErr(descErr, &de) {
		t.Fatalf("describe should fail with an OracleError, got %v", descErr)
	}
	if de.Pos < 0 {
		t.Skipf("engine reported no position for this error (Pos=%d); differential test not applicable", de.Pos)
	}

	planErr := o.Plan(ctx, sql)
	var pe *dialect.OracleError
	if planErr == nil || !asOracleErr(planErr, &pe) {
		t.Fatalf("plan should fail with an OracleError, got %v", planErr)
	}

	if pe.Pos != de.Pos {
		t.Errorf("plan Pos=%d differs from describe Pos=%d — prefix not stripped", pe.Pos, de.Pos)
	}
	if de.Pos != wantAt {
		t.Errorf("describe Pos=%d, want offset of FROM=%d", de.Pos, wantAt)
	}
}
