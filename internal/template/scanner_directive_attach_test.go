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

// A directive placed after query A's last plain SQL token but BEFORE a
// trailing construct (no `;`) annotates A: A's body continues past it,
// through the construct. Constructs used not to claim buffered
// directives, so the opt-out stayed pending until B's header and
// attached to B — B shipped unscoped while A was scoped (CWE-863).
func TestScan_DirectiveBeforeTrailingConstructStaysWithQuery(t *testing.T) {
	directives := "-- @policy-optout: tenant (ops export)\n" +
		"-- @policy-apply: other\n" +
		"-- @param since: timestamptz\n" +
		"-- @column id: int8\n" +
		"-- @timeout 5s\n" +
		"-- @nolint SQLETCHL003 (bounded by caller)\n" +
		"-- @nullable id: false\n"
	trailers := []struct{ name, before, construct string }{
		{"if-present", "SELECT o.id FROM orders AS o\nWHERE TRUE\n",
			"@if-present(since) AND o.created_at >= :since @endif\n"},
		{"when", "SELECT o.id FROM orders AS o\nWHERE TRUE\n",
			"@when(f = false)\n  AND o.deleted_at IS NULL\n@end\n"},
		{"order-by", "SELECT o.id FROM orders AS o\n",
			"@order-by(sort)\n@key(id) o.id\n@end\n"},
		{"choose", "SELECT o.id FROM orders AS o\n",
			"@choose(s)\n@case(a) ORDER BY o.id\n@end\n"},
		{"in", "SELECT o.id FROM orders AS o WHERE o.id\n",
			"@in(:ids)\n"},
		{"filter-tree", "SELECT o.id FROM orders AS o WHERE TRUE\n  AND\n",
			"@filter-tree(s)\n@predicate(x)\no.a = :a\n@end\n"},
	}
	for _, tc := range trailers {
		t.Run(tc.name, func(t *testing.T) {
			src := "-- name: A :many\n" + tc.before + directives + tc.construct +
				"\n-- name: B :many\nSELECT o.id FROM orders AS o\n"
			f := scanClean(t, src)
			if len(f.Queries) != 2 {
				t.Fatalf("got %d queries", len(f.Queries))
			}
			a, b := f.Queries[0], f.Queries[1]
			if len(a.PolicyOptOuts) != 1 || a.PolicyOptOuts[0].Policy != "tenant" {
				t.Errorf("opt-out did not stay with query A: %+v", a.PolicyOptOuts)
			}
			if len(a.PolicyApplies) != 1 || a.PolicyApplies[0].Policy != "other" {
				t.Errorf("@policy-apply did not stay with query A: %+v", a.PolicyApplies)
			}
			if _, ok := a.TypeHints["since"]; !ok {
				t.Errorf("@param did not stay with query A: %+v", a.TypeHints)
			}
			if _, ok := a.ColumnHints["id"]; !ok {
				t.Errorf("@column did not stay with query A: %+v", a.ColumnHints)
			}
			if a.Timeout == nil {
				t.Errorf("@timeout did not stay with query A")
			}
			if len(a.NoLints) != 1 {
				t.Errorf("@nolint did not stay with query A: %+v", a.NoLints)
			}
			if _, ok := a.NullOverrides["id"]; !ok {
				t.Errorf("@nullable did not stay with query A: %+v", a.NullOverrides)
			}
			if len(b.PolicyOptOuts) != 0 || len(b.PolicyApplies) != 0 || len(b.TypeHints) != 0 ||
				len(b.ColumnHints) != 0 || b.Timeout != nil || len(b.NoLints) != 0 || len(b.NullOverrides) != 0 {
				t.Errorf("query A's directives leaked onto query B: %+v", b)
			}
		})
	}
}

// A directive in the gap AFTER a trailing construct (i.e. after A's
// body is complete) still annotates the following query.
func TestScan_DirectiveAfterTrailingConstructAttachesToFollowingQuery(t *testing.T) {
	src := "-- name: A :many\n" +
		"SELECT o.id FROM orders AS o WHERE TRUE\n" +
		"@if-present(since) AND o.created_at >= :since @endif\n" +
		"-- @policy-optout: tenant (gap job)\n" +
		"-- name: B :many\n" +
		"SELECT o.id FROM orders AS o\n"
	f := scanClean(t, src)
	a, b := f.Queries[0], f.Queries[1]
	if len(a.PolicyOptOuts) != 0 {
		t.Errorf("gap directive leaked onto query A: %+v", a.PolicyOptOuts)
	}
	if len(b.PolicyOptOuts) != 1 || b.PolicyOptOuts[0].Policy != "tenant" {
		t.Errorf("gap directive did not attach to query B: %+v", b.PolicyOptOuts)
	}
}
