package sqlite

import (
	"slices"
	"testing"
)

// A table-valued-function FROM source (rqlite/sql parses
// `FROM json_each('[1,2]')` as *sql.QualifiedTableFunctionName) used to
// be DROPPED entirely by every source walker — not even the opaque
// marker derived tables get. The consequence was a silent policy blind
// spot: a designated table reachable only through a TVF argument
// escaped both weaving and the SQLETCH125 subquery-rejection guard,
// because the weaver's DeepTables/Relations per-name-count comparison
// undercounted. A TVF source must be visible (opaquely) to every walker.

func relCount(t *testing.T, sql string) int {
	t.Helper()
	tree, err := Frontend{}.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	return len(tree.Relations())
}

// A lone TVF source must no longer render Relations()/DeepTables()
// silently empty: the TVF is counted opaquely, and its own name is NOT
// mistaken for a base table.
func TestTVF_LoneSourceIsVisibleOpaquely(t *testing.T) {
	const sql = "SELECT value FROM json_each('[1,2]')"

	if got := relCount(t, sql); got != 1 {
		t.Fatalf("Relations() count = %d, want 1 — a TVF FROM source must be counted opaquely, not dropped", got)
	}
	// The opaque relation must carry no base-table name (json_each is a
	// function, not a designated table).
	tree, err := Frontend{}.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, r := range tree.Relations() {
		if r.Table != "" {
			t.Errorf("Relations() surfaced base table %q — a TVF name must never masquerade as a base table", r.Table)
		}
		if r.Loc != -1 {
			t.Errorf("TVF relation Loc = %d, want -1 (opaque marker)", r.Loc)
		}
	}
	if got := deepTableNames(t, sql); len(got) != 0 {
		t.Errorf("DeepTables() = %v, want empty — a TVF name is not a base table", got)
	}
}

// A designated table reachable ONLY through a TVF argument (a subquery
// argument) must appear in DeepTables but NOT in Relations, so the
// weaver's per-name-count comparison sees it as hidden and refuses it
// (SQLETCH125) instead of silently skipping it.
func TestTVF_SubqueryArgReachesDesignatedTable(t *testing.T) {
	const sql = "SELECT value FROM json_each((SELECT tags FROM secrets LIMIT 1))"

	deep := deepTableNames(t, sql)
	if !slices.Contains(deep, "secrets") {
		t.Fatalf("DeepTables() = %v, want to contain \"secrets\" (reachable through the TVF argument subquery)", deep)
	}
	// The designated table must NOT surface as a top-level relation:
	// it lives in an opaque/hidden position, so Relations undercounts
	// relative to DeepTables and the weaver rejects it.
	rels := relTableNames(t, sql)
	if slices.Contains(rels, "secrets") {
		t.Errorf("Relations() = %v — a table reachable only through a TVF argument must not be a top-level relation", rels)
	}
}

// A TVF alongside a normal base table: the base table stays a plain
// designated relation, and the TVF is an extra opaque relation.
func TestTVF_WithNormalTableStillWorks(t *testing.T) {
	const sql = "SELECT u.id, j.value FROM users u, json_each(u.tags) j"

	rels := relTableNames(t, sql)
	if !slices.Equal(rels, []string{"users"}) {
		t.Errorf("Relations() tables = %v, want [users] — the normal base table must remain visible and the TVF must not add a base table", rels)
	}
	// users (base) + json_each (opaque) = two relations total.
	if got := relCount(t, sql); got != 2 {
		t.Errorf("Relations() count = %d, want 2 (users + opaque TVF)", got)
	}
	if got := deepTableNames(t, sql); !slices.Equal(got, []string{"users"}) {
		t.Errorf("DeepTables() = %v, want [users]", got)
	}
}

// The TVF's effective FROM name (alias else function name) participates
// in scope tracking so a qualified ref into it is not re-derived as a
// correlated top-level ref. This asserts sourceNames handles the TVF.
func TestTVF_AliasEntersScope(t *testing.T) {
	// A qualified ref j.value into the TVF alias, nested in a subquery,
	// must carry the TVF alias in its scope so R3 shadowing applies.
	const sql = "SELECT (SELECT j.value FROM json_each(t.tags) j LIMIT 1) FROM things t"
	tree, err := Frontend{}.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, cr := range tree.ColumnRefs() {
		if len(cr.Fields) == 2 && cr.Fields[0] == "j" && cr.Fields[1] == "value" {
			found = true
			if !slices.Contains(cr.ScopeAliases, "j") {
				t.Errorf("ColumnRef j.value ScopeAliases = %v, want to contain \"j\" (the TVF alias must enter scope)", cr.ScopeAliases)
			}
		}
	}
	if !found {
		t.Fatalf("ColumnRefs() did not surface the j.value reference")
	}
}
