package template

import "testing"

// A directive comment sitting in the gap between query A's body and
// query B's `-- name:` header annotates the FOLLOWING query (B), not the
// previous one (A). Before the fix it leaked onto A — a tenant-policy
// exemption silently landing on the wrong query.
func TestScan_DirectiveInGapAttachesToFollowingQuery(t *testing.T) {
	src := "-- name: A :many\n" +
		"SELECT id FROM orders\n" +
		"-- @policy-optout: tenant (gap job)\n" +
		"-- name: B :many\n" +
		"SELECT id FROM orders\n"
	f := scanClean(t, src)
	a, b := f.Queries[0], f.Queries[1]
	if len(a.PolicyOptOuts) != 0 {
		t.Errorf("gap directive leaked onto query A: %+v", a.PolicyOptOuts)
	}
	if len(b.PolicyOptOuts) != 1 || b.PolicyOptOuts[0].Policy != "tenant" {
		t.Errorf("gap directive did not attach to query B: %+v", b.PolicyOptOuts)
	}
}

// The same holds for @param / @column type hints in the gap.
func TestScan_GapTypeHintsAttachToFollowingQuery(t *testing.T) {
	src := "-- name: A :many\n" +
		"SELECT id FROM orders\n" +
		"-- @param x: int4\n" +
		"-- @column c: text\n" +
		"-- name: B :many\n" +
		"SELECT id FROM orders WHERE id = :x\n"
	f := scanClean(t, src)
	a, b := f.Queries[0], f.Queries[1]
	if len(a.TypeHints) != 0 || len(a.ColumnHints) != 0 {
		t.Errorf("gap hints leaked onto query A: params=%+v cols=%+v", a.TypeHints, a.ColumnHints)
	}
	if _, ok := b.TypeHints["x"]; !ok {
		t.Errorf("gap @param did not attach to query B: %+v", b.TypeHints)
	}
	if _, ok := b.ColumnHints["c"]; !ok {
		t.Errorf("gap @column did not attach to query B: %+v", b.ColumnHints)
	}
}

// A directive INSIDE a query's body (more of that query's SQL follows it)
// still attaches to that query.
func TestScan_DirectiveInsideBodyStaysWithQuery(t *testing.T) {
	src := "-- name: A :many\n" +
		"SELECT id\n" +
		"-- @policy-optout: tenant (this query is fine)\n" +
		"FROM orders WHERE id = :id\n" +
		"-- name: B :many\n" +
		"SELECT 1\n"
	f := scanClean(t, src)
	a, b := f.Queries[0], f.Queries[1]
	if len(a.PolicyOptOuts) != 1 || a.PolicyOptOuts[0].Policy != "tenant" {
		t.Errorf("in-body directive did not stay with query A: %+v", a.PolicyOptOuts)
	}
	if len(b.PolicyOptOuts) != 0 {
		t.Errorf("in-body directive leaked onto query B: %+v", b.PolicyOptOuts)
	}
}

// A trailing directive after the last query's body (no following query)
// attaches to that last query.
func TestScan_TrailingDirectiveAttachesToLastQuery(t *testing.T) {
	src := "-- name: A :many\n" +
		"SELECT id FROM orders\n" +
		"-- @policy-optout: tenant (last one)\n"
	q := scanClean(t, src).Queries[0]
	if len(q.PolicyOptOuts) != 1 || q.PolicyOptOuts[0].Policy != "tenant" {
		t.Errorf("trailing directive did not attach to the last query: %+v", q.PolicyOptOuts)
	}
}
