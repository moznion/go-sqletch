package mysql

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// locateRelations must pin a relation to its FROM-position token, not to
// a same-named column reference, a backquoted identifier whose content
// looks like a keyword, or a name inside a (VALUES …) constructor.
func TestLocateRelations_Precision(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		tab  string
		// want is the byte offset locateRelations should assign; it is
		// computed per-case below (LastIndex picks the real FROM entry,
		// which in each adversarial case is the LAST occurrence).
		want func(sql string) int
	}{
		{
			// '.' predecessor: the SELECT-list `u.orders` must not be
			// mistaken for the FROM relation `orders`.
			name: "dot-qualified column ref",
			sql:  "SELECT u.orders FROM orders u",
			tab:  "orders",
			want: func(s string) int { return strings.LastIndex(s, "orders") },
		},
		{
			// A backquoted identifier named FROM must not act as the
			// FROM keyword for the following token.
			name: "backquoted keyword identifier",
			sql:  "SELECT `FROM` users FROM users",
			tab:  "users",
			want: func(s string) int { return strings.LastIndex(s, "users") },
		},
		{
			// A relation-looking name inside a (VALUES …) constructor is
			// opaque; the real `orders` is the one after the comma.
			name: "values constructor is opaque",
			sql:  "SELECT * FROM (VALUES ROW(orders)) o, orders t",
			tab:  "orders",
			want: func(s string) int { return strings.LastIndex(s, "orders") },
		},
		{
			// Regression: a genuinely schema-qualified name (`db.orders`)
			// whose qualifier IS in FROM position still locates.
			name: "schema-qualified relation still located",
			sql:  "SELECT id FROM db.orders",
			tab:  "orders",
			want: func(s string) int { return strings.Index(s, "orders") },
		},
		{
			// Regression: an ordinary FROM relation still locates.
			name: "plain relation",
			sql:  "SELECT id FROM orders",
			tab:  "orders",
			want: func(s string) int { return strings.Index(s, "orders") },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rels := []dialect.RelRef{{Table: c.tab}}
			locateRelations(c.sql, rels)
			if got, want := rels[0].Loc, c.want(c.sql); got != want {
				t.Errorf("Loc = %d (%q), want %d (%q)",
					got, sliceAt(c.sql, got), want, sliceAt(c.sql, want))
			}
		})
	}
}

// TestLocateRelations_FromContext exercises FROM-clause context: the
// scan must honor ','/'(' as FROM-position predecessors ONLY inside a
// FROM/JOIN region. SELECT-list parens, expression parens, function-call
// parens, and index-hint parens must never be read as FROM openings, and
// the scan must not begin matching before the FROM keyword. These are the
// soundness repros: a mislocated RelRef.Loc feeds nullability isGuarded,
// R1 checkJoinMembership, and the policy weaver's guardedAt / D2a ON-clause
// key.
func TestLocateRelations_FromContext(t *testing.T) {
	lastIdx := func(needle string) func(string) int {
		return func(s string) int { return strings.LastIndex(s, needle) }
	}
	cases := []struct {
		name string
		sql  string
		tab  string
		want func(sql string) int
	}{
		{
			// Aggregate over a qualified column in the SELECT list: the
			// `t1` inside MAX(...) must not be mistaken for the FROM `t1`.
			name: "aggregate arg in select list",
			sql:  "SELECT MAX(t1.id) AS m FROM t1",
			tab:  "t1",
			want: lastIdx("t1"),
		},
		{
			// Expression parens in the SELECT list are not a FROM opening.
			name: "expression parens in select list",
			sql:  "SELECT (t1.a + 1) FROM t1",
			tab:  "t1",
			want: lastIdx("t1"),
		},
		{
			// The comma inside COALESCE(...) is not a table separator.
			name: "comma inside function call",
			sql:  "SELECT COALESCE(a, t1) FROM t1",
			tab:  "t1",
			want: lastIdx("t1"),
		},
		{
			// Index-hint parens are opaque: the `orders` inside
			// USE INDEX (orders) must not shadow the joined `orders`.
			name: "use index hint parens are opaque",
			sql:  "SELECT id FROM users USE INDEX (orders) JOIN orders o ON o.id = users.id",
			tab:  "orders",
			want: func(s string) int { return strings.Index(s, "JOIN orders") + len("JOIN ") },
		},
		{
			// FORCE INDEX variant, same discipline.
			name: "force index hint parens are opaque",
			sql:  "SELECT id FROM users FORCE INDEX (orders) JOIN orders o ON o.id = users.id",
			tab:  "orders",
			want: func(s string) int { return strings.Index(s, "JOIN orders") + len("JOIN ") },
		},
		{
			// A subquery in FROM is skipped whole; the outer table is the
			// one located.
			name: "subquery in from skipped whole",
			sql:  "SELECT * FROM (SELECT id FROM inner_t) x JOIN outer_t ON x.id = outer_t.id",
			tab:  "outer_t",
			want: func(s string) int { return strings.Index(s, "outer_t") },
		},
		{
			// Comment between FROM and the relation name.
			name: "comment before relation",
			sql:  "SELECT id FROM /* pick */ users",
			tab:  "users",
			want: func(s string) int { return strings.Index(s, "users") },
		},
		{
			// A SELECT-list column named like the FROM table must not be
			// located; the FROM occurrence wins.
			name: "column named like the table",
			sql:  "SELECT orders.id, orders FROM orders",
			tab:  "orders",
			want: lastIdx("orders"),
		},
		{
			// Multibyte content before FROM shifts the byte offset.
			name: "multibyte before from",
			sql:  "SELECT 'café☕' AS x FROM users",
			tab:  "users",
			want: func(s string) int { return strings.Index(s, "users") },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rels := []dialect.RelRef{{Table: c.tab}}
			locateRelations(c.sql, rels)
			if got, want := rels[0].Loc, c.want(c.sql); got != want {
				t.Errorf("Loc = %d (%q), want %d (%q)",
					got, sliceAt(c.sql, got), want, sliceAt(c.sql, want))
			}
		})
	}
}

// TestLocateRelations_MultiRelation covers the forward-scan resumption
// across several relations in one FROM tree: joins and comma-joins.
func TestLocateRelations_MultiRelation(t *testing.T) {
	cases := []struct {
		name  string
		sql   string
		names []string
	}{
		{
			name:  "multiple joined tables",
			sql:   "SELECT * FROM a JOIN b JOIN c ON a.id = b.id",
			names: []string{"a", "b", "c"},
		},
		{
			name:  "comma joins",
			sql:   "SELECT * FROM a, b, c",
			names: []string{"a", "b", "c"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rels := make([]dialect.RelRef, len(c.names))
			for i, n := range c.names {
				rels[i].Table = n
			}
			locateRelations(c.sql, rels)
			// Each relation is the LAST bare occurrence of its own name in
			// FROM position; for these shapes the only occurrence in FROM
			// position is the table itself, at ascending offsets.
			prev := -1
			for i, n := range c.names {
				// The table name here also appears in the ON clause for the
				// join case, so pin to the FROM occurrence: the first index
				// at or after the previous relation.
				want := strings.Index(c.sql[prev+1:], n) + prev + 1
				if rels[i].Loc != want {
					t.Errorf("rel %q Loc = %d (%q), want %d (%q)",
						n, rels[i].Loc, sliceAt(c.sql, rels[i].Loc), want, sliceAt(c.sql, want))
				}
				prev = rels[i].Loc
			}
		})
	}
}

// TestLocateRelations_NonReservedKeywordTableName is the regression for
// the PR #76 FROM-context tracking: a bare identifier equal to a clause
// closer keyword (OFFSET/GROUP/…) is a legal UNQUOTED table name in
// MySQL (these are non-reserved). Such a name arrives in FROM position
// and is consumed as a relation, so it must NOT also close the FROM
// region — otherwise the following comma-joined relation loses its ','
// predecessor and runs to EOF (Loc=-1), degrading guard precision in the
// very locator #76 hardened.
func TestLocateRelations_NonReservedKeywordTableName(t *testing.T) {
	t.Run("offset as leading table keeps comma join located", func(t *testing.T) {
		// SELECT * FROM offset o, t2 — pre-fix `t2` got Loc=-1 because
		// `offset` (matched in FROM position) flipped inFrom=false.
		sql := "SELECT * FROM offset o, t2"
		rels := []dialect.RelRef{{Table: "offset"}, {Table: "t2"}}
		locateRelations(sql, rels)
		if want := strings.Index(sql, "offset"); rels[0].Loc != want {
			t.Errorf("offset Loc = %d (%q), want %d", rels[0].Loc, sliceAt(sql, rels[0].Loc), want)
		}
		if want := strings.Index(sql, "t2"); rels[1].Loc != want {
			t.Errorf("t2 Loc = %d (%q), want %d (%q)", rels[1].Loc, sliceAt(sql, rels[1].Loc), want, sliceAt(sql, want))
		}
	})

	t.Run("offset in the middle keeps trailing comma join located", func(t *testing.T) {
		// SELECT * FROM t1, offset o, t2 — pre-fix `t2` got Loc=-1.
		sql := "SELECT * FROM t1, offset o, t2"
		rels := []dialect.RelRef{{Table: "t1"}, {Table: "offset"}, {Table: "t2"}}
		locateRelations(sql, rels)
		for i, n := range []string{"t1", "offset", "t2"} {
			if want := strings.Index(sql, n); rels[i].Loc != want {
				t.Errorf("%s Loc = %d (%q), want %d (%q)", n, rels[i].Loc, sliceAt(sql, rels[i].Loc), want, sliceAt(sql, want))
			}
		}
	})

	t.Run("real clause keyword still closes the from region", func(t *testing.T) {
		// Control: a genuine WHERE (and GROUP) closes the region, so the
		// depth-0 comma inside GROUP BY is NOT a table separator — a name
		// there must NOT be located as a FROM relation.
		sql := "SELECT * FROM t1 WHERE id > 0 GROUP BY a, notarel"
		rels := []dialect.RelRef{{Table: "t1"}, {Table: "notarel"}}
		locateRelations(sql, rels)
		if want := strings.Index(sql, "t1"); rels[0].Loc != want {
			t.Errorf("t1 Loc = %d (%q), want %d", rels[0].Loc, sliceAt(sql, rels[0].Loc), want)
		}
		if rels[1].Loc != -1 {
			t.Errorf("notarel Loc = %d (%q), want -1 (region closed by WHERE)", rels[1].Loc, sliceAt(sql, rels[1].Loc))
		}
	})
}

// TestLocateRelations_KeywordAlias is the regression for the alias twin of
// the PR #76 fix: a table ALIAS that is a non-reserved closer keyword
// (`FROM t1 offset` / `FROM t1 AS offset`) is NOT in FROM position (its
// predecessor is the table it aliases, or `AS`), so it used to run
// isFromCloser and flip inFrom=false — orphaning a following `, t2`
// (Loc=-1). An alias slot must not close the FROM region; a RESERVED
// closer directly after a relation still closes.
func TestLocateRelations_KeywordAlias(t *testing.T) {
	locAll := func(t *testing.T, sql string, names ...string) []dialect.RelRef {
		t.Helper()
		rels := make([]dialect.RelRef, len(names))
		for i, n := range names {
			rels[i].Table = n
		}
		locateRelations(sql, rels)
		return rels
	}
	wantAt := func(t *testing.T, sql string, r dialect.RelRef, name string) {
		t.Helper()
		if want := strings.Index(sql, name); r.Loc != want {
			t.Errorf("%s Loc = %d (%q), want %d (%q)", name, r.Loc, sliceAt(sql, r.Loc), want, sliceAt(sql, want))
		}
	}

	t.Run("bare keyword alias keeps comma join located", func(t *testing.T) {
		// `offset` here ALIASES t1; t1 and t2 are the relations.
		sql := "SELECT * FROM t1 offset, t2"
		rels := locAll(t, sql, "t1", "t2")
		wantAt(t, sql, rels[0], "t1")
		wantAt(t, sql, rels[1], "t2")
	})

	t.Run("AS keyword alias keeps comma join located", func(t *testing.T) {
		sql := "SELECT * FROM t1 AS offset, t2"
		rels := locAll(t, sql, "t1", "t2")
		wantAt(t, sql, rels[0], "t1")
		wantAt(t, sql, rels[1], "t2")
	})

	t.Run("reserved closer after a relation still closes", func(t *testing.T) {
		// `FROM offset GROUP BY …`: offset is the relation and GROUP is a
		// RESERVED closer (never a bare alias), so the region closes and the
		// depth-0 comma in GROUP BY is NOT a table separator — notarel must
		// stay unlocated.
		sql := "SELECT * FROM offset GROUP BY a, notarel"
		rels := locAll(t, sql, "offset", "notarel")
		wantAt(t, sql, rels[0], "offset")
		if rels[1].Loc != -1 {
			t.Errorf("notarel Loc = %d (%q), want -1 (region closed by GROUP)", rels[1].Loc, sliceAt(sql, rels[1].Loc))
		}
	})

	t.Run("real clause after an alias still closes", func(t *testing.T) {
		// `FROM t1 o WHERE …`: o is the alias, WHERE closes; the later
		// GROUP BY comma is not a separator.
		sql := "SELECT * FROM t1 o WHERE id > 0 GROUP BY a, notarel"
		rels := locAll(t, sql, "t1", "notarel")
		wantAt(t, sql, rels[0], "t1")
		if rels[1].Loc != -1 {
			t.Errorf("notarel Loc = %d (%q), want -1 (region closed by WHERE)", rels[1].Loc, sliceAt(sql, rels[1].Loc))
		}
	})
}

// TestLocateRelations_StraightJoinModifier is the M4 regression:
// STRAIGHT_JOIN has TWO positions in MySQL — a join operator
// (`a STRAIGHT_JOIN b`) and a SELECT modifier (`SELECT STRAIGHT_JOIN
// col …`, like SQL_CALC_FOUND_ROWS). The lexical relation scanner used
// to treat every STRAIGHT_JOIN as a FROM-introducer, so a select-option
// occurrence prematurely opened the FROM region over the select list and
// a bare select-list identifier equal to a table name was pinned as the
// relation — mislocating RelRef.Loc (feeds guard derivation, R1
// join-membership, nullability isGuarded). The modifier occurrence must
// be ignored; the join-operator form must still open/continue the FROM.
func TestLocateRelations_StraightJoinModifier(t *testing.T) {
	t.Run("modifier before select list locates FROM t1", func(t *testing.T) {
		sql := "SELECT STRAIGHT_JOIN t1, b FROM t1, t2"
		rels := []dialect.RelRef{{Table: "t1"}, {Table: "t2"}}
		locateRelations(sql, rels)
		// t1 must be the FROM occurrence (the LAST one), not the
		// select-list t1.
		if want := strings.LastIndex(sql, "t1"); rels[0].Loc != want {
			t.Errorf("t1 Loc = %d (%q), want %d (FROM occurrence)", rels[0].Loc, sliceAt(sql, rels[0].Loc), want)
		}
		if want := strings.Index(sql, "t2"); rels[1].Loc != want {
			t.Errorf("t2 Loc = %d (%q), want %d", rels[1].Loc, sliceAt(sql, rels[1].Loc), want)
		}
	})

	t.Run("modifier with several select-list columns locates FROM t1", func(t *testing.T) {
		sql := "SELECT STRAIGHT_JOIN a, t1, b FROM t1, t2"
		rels := []dialect.RelRef{{Table: "t1"}, {Table: "t2"}}
		locateRelations(sql, rels)
		if want := strings.LastIndex(sql, "t1"); rels[0].Loc != want {
			t.Errorf("t1 Loc = %d (%q), want %d (FROM occurrence)", rels[0].Loc, sliceAt(sql, rels[0].Loc), want)
		}
		if want := strings.Index(sql, "t2"); rels[1].Loc != want {
			t.Errorf("t2 Loc = %d (%q), want %d", rels[1].Loc, sliceAt(sql, rels[1].Loc), want)
		}
	})

	t.Run("control SQL_CALC_FOUND_ROWS modifier locates FROM t1", func(t *testing.T) {
		sql := "SELECT SQL_CALC_FOUND_ROWS t1 FROM t1"
		rels := []dialect.RelRef{{Table: "t1"}}
		locateRelations(sql, rels)
		if want := strings.LastIndex(sql, "t1"); rels[0].Loc != want {
			t.Errorf("t1 Loc = %d (%q), want %d (FROM occurrence)", rels[0].Loc, sliceAt(sql, rels[0].Loc), want)
		}
	})

	t.Run("join operator form still locates both relations", func(t *testing.T) {
		sql := "SELECT * FROM t1 STRAIGHT_JOIN t2 ON t1.id=t2.id"
		rels := []dialect.RelRef{{Table: "t1"}, {Table: "t2"}}
		locateRelations(sql, rels)
		if want := strings.Index(sql, "t1"); rels[0].Loc != want {
			t.Errorf("t1 Loc = %d (%q), want %d", rels[0].Loc, sliceAt(sql, rels[0].Loc), want)
		}
		// t2 is the FROM occurrence, before the ON-clause t2.
		if want := strings.Index(sql, "t2"); rels[1].Loc != want {
			t.Errorf("t2 Loc = %d (%q), want %d", rels[1].Loc, sliceAt(sql, rels[1].Loc), want)
		}
	})
}

func sliceAt(s string, off int) string {
	if off < 0 || off >= len(s) {
		return "<none>"
	}
	end := off + 10
	if end > len(s) {
		end = len(s)
	}
	return s[off:end]
}
