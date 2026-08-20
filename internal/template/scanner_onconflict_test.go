package template

import "testing"

// upsertGuarded is an INSERT ... ON CONFLICT DO UPDATE whose VALUES row
// carries a guarded (optional) value paired with a guarded column. The
// conflict target `(a)` and the DO-UPDATE `WHERE` follow the VALUES row.
const upsertGuarded = `-- name: Up :exec
INSERT INTO t (
    a
@if-present(b)
  , b
@endif
) VALUES (
    :a
@if-present(b)
  , :b
@endif
) ON CONFLICT (a) DO UPDATE SET b = excluded.b WHERE t.tenant = 1;
`

// The conflict-target parens after a closed VALUES row must NOT be read
// as another VALUES row: doing so appended a phantom empty
// InsertValGuards entry, which then false-rejected the upsert under R7
// (SQLETCH119, "VALUES row 2 has 0 optional items…").
func TestScan_OnConflictNoPhantomValuesRow(t *testing.T) {
	q := scanClean(t, upsertGuarded).Queries[0]
	if len(q.InsertValGuards) != 1 {
		t.Fatalf("want exactly one VALUES row, got %d: %+v", len(q.InsertValGuards), q.InsertValGuards)
	}
	if got := len(q.InsertValGuards[0]); got != 1 {
		t.Fatalf("want one guarded value item in the row, got %d", got)
	}
}

// The `WHERE` belongs to ON CONFLICT / the DO-UPDATE predicate, not the
// statement's row filter, so it must not be recorded as WhereKwEnd — the
// weaver would otherwise splice a tenant conjunct into the wrong clause.
func TestScan_OnConflictWhereNotStatementWhere(t *testing.T) {
	q := scanClean(t, upsertGuarded).Queries[0]
	if q.WhereKwEnd != -1 {
		t.Fatalf("WhereKwEnd must stay -1 for a WHERE after ON CONFLICT, got %d", q.WhereKwEnd)
	}
}

// ON CONFLICT after an INSERT ... SELECT (no VALUES) must likewise not
// record the DO-UPDATE WHERE as the statement WHERE.
func TestScan_OnConflictAfterSelectWhereNotStatementWhere(t *testing.T) {
	src := "-- name: Up :exec\n" +
		"INSERT INTO t (a, b) SELECT x, y FROM s ON CONFLICT (a) DO UPDATE SET b = excluded.b WHERE t.tenant = 1;\n"
	q := scanClean(t, src).Queries[0]
	if q.WhereKwEnd != -1 {
		t.Fatalf("WhereKwEnd must stay -1 for a WHERE after ON CONFLICT, got %d", q.WhereKwEnd)
	}
}

// Regression guard: an ordinary multi-row INSERT ... VALUES still counts
// its real rows (and only those).
func TestScan_PlainMultiRowValues(t *testing.T) {
	src := "-- name: Ins :exec\nINSERT INTO t (a, b) VALUES (:a, :b), (:c, :d);\n"
	q := scanClean(t, src).Queries[0]
	if len(q.InsertValGuards) != 2 {
		t.Fatalf("want two VALUES rows, got %d: %+v", len(q.InsertValGuards), q.InsertValGuards)
	}
	for i, r := range q.InsertValGuards {
		if len(r) != 0 {
			t.Errorf("row %d unexpectedly has guarded items: %+v", i+1, r)
		}
	}
}

// Regression guard: a normal statement WHERE is still recorded.
func TestScan_OrdinaryWhereStillRecorded(t *testing.T) {
	src := "-- name: Sel :many\nSELECT id FROM t WHERE id = :id;\n"
	q := scanClean(t, src).Queries[0]
	if q.WhereKwEnd <= 0 {
		t.Fatalf("ordinary statement WHERE must set WhereKwEnd, got %d", q.WhereKwEnd)
	}
}

// Regression guard (#89 follow-up): `ON CONFLICT` detection must be gated
// on the statement being an INSERT. In a SELECT, `ON` is the ordinary JOIN
// keyword, so a bare column literally named `conflict` in the join
// condition forms the depth-0 token pair `ON` `CONFLICT` — but this is NOT
// an upsert. Ungated, afterOnConflict flipped and the real statement WHERE
// was skipped (WhereKwEnd stayed -1), which drove the policy weaver to
// synthesize a spurious second WHERE (doubled-WHERE oracle syntax error).
func TestScan_SelectJoinOnConflictColumnRecordsRealWhere(t *testing.T) {
	src := "-- name: Sel :many\n" +
		"SELECT a.id FROM a JOIN b ON conflict = b.id WHERE a.tenant = :t;\n"
	q := scanClean(t, src).Queries[0]
	if q.WhereKwEnd <= 0 {
		t.Fatalf("SELECT with a JOIN column named `conflict` must record its real WHERE, got WhereKwEnd=%d", q.WhereKwEnd)
	}
	// The recorded end must sit just past the real WHERE keyword, i.e. the
	// tenant predicate follows it.
	const wantAfter = " a.tenant"
	if q.WhereKwEnd < 0 || q.WhereKwEnd+len(wantAfter) > len(src) || src[q.WhereKwEnd:q.WhereKwEnd+len(wantAfter)] != wantAfter {
		t.Fatalf("WhereKwEnd=%d does not point past the real WHERE keyword; src[end:]=%q", q.WhereKwEnd, src[q.WhereKwEnd:])
	}
}

// Regression guard (#90 follow-up): inside a genuine INSERT ... SELECT,
// isInsert is legitimately true, so gating the depth-0 `ON` `CONFLICT`
// pair on isInsert ALONE does not block a bare column named `conflict`
// in the feeding SELECT's `JOIN ... ON conflict = ...`. The real ON
// CONFLICT clause only appears past the completed value source (the
// VALUES rows or the feeding SELECT), never inside the feed's join
// tree — so afterOnConflict must not flip there and the feeding
// SELECT's own row-filter WHERE must still be recorded.
func TestScan_InsertSelectJoinOnConflictColumnRecordsRealWhere(t *testing.T) {
	src := "-- name: Q :exec\n" +
		"INSERT INTO t (id) SELECT a.id FROM a JOIN b ON conflict = b.id WHERE a.tenant = :ten;\n"
	q := scanClean(t, src).Queries[0]
	if q.WhereKwEnd <= 0 {
		t.Fatalf("INSERT ... SELECT with a JOIN column named `conflict` must record the feeding SELECT's real WHERE, got WhereKwEnd=%d", q.WhereKwEnd)
	}
	// The recorded end must sit just past the real WHERE keyword, i.e.
	// the tenant predicate follows it.
	const wantAfter = " a.tenant"
	if q.WhereKwEnd+len(wantAfter) > len(src) || src[q.WhereKwEnd:q.WhereKwEnd+len(wantAfter)] != wantAfter {
		t.Fatalf("WhereKwEnd=%d does not point past the real WHERE keyword; src[end:]=%q", q.WhereKwEnd, src[q.WhereKwEnd:])
	}
}

// Companion to the above: a join in the feeding SELECT whose ON is
// consumed by a genuine join predicate, followed by the real ON
// CONFLICT clause, must STILL suppress the DO-UPDATE WHERE. This is the
// shape a naive "ignore every ON inside the feed" fix would break.
func TestScan_InsertSelectJoinThenRealOnConflictSuppresses(t *testing.T) {
	src := "-- name: Up :exec\n" +
		"INSERT INTO t (id) SELECT a.id FROM a JOIN b ON a.k = b.k ON CONFLICT (id) DO UPDATE SET n = 1 WHERE t.tenant = 1;\n"
	q := scanClean(t, src).Queries[0]
	if q.WhereKwEnd != -1 {
		t.Fatalf("WhereKwEnd must stay -1 for a WHERE after a real ON CONFLICT (join in feed), got %d", q.WhereKwEnd)
	}
}

// The partial-index predicate WHERE inside a conflict target
// (`ON CONFLICT (a) WHERE <pred> DO UPDATE ...`) is part of the conflict
// clause, not the statement's row filter, and must not be recorded.
func TestScan_OnConflictPartialIndexWhereNotStatementWhere(t *testing.T) {
	src := "-- name: Up :exec\n" +
		"INSERT INTO t (a) VALUES (1) ON CONFLICT (a) WHERE a > 0 DO UPDATE SET b = 2 WHERE t.tenant = 1;\n"
	q := scanClean(t, src).Queries[0]
	if q.WhereKwEnd != -1 {
		t.Fatalf("WhereKwEnd must stay -1 for a partial-index WHERE inside ON CONFLICT, got %d", q.WhereKwEnd)
	}
}
