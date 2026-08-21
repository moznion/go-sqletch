package sqlite

import (
	"slices"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// TestFrontend_DeepTables_InTableOperand pins finding H5: rqlite parses
// SQLite's `expr IN table-name` form into *Ident / *QualifiedRef, which
// the tableWalker previously treated as a bare column ref (no table).
// A policy-designated table appearing ONLY as an IN-table operand was
// therefore missing from DeepTables, so the weaver neither wove nor
// refused it — a silent unscoped read. DeepTables must now surface it.
// The `IN (subquery)` and `IN (expr-list)` forms must NOT gain a
// spurious table.
func TestFrontend_DeepTables_InTableOperand(t *testing.T) {
	cases := []struct {
		sql  string
		want []string
	}{
		{"SELECT x FROM t WHERE x IN secret_table", []string{"secret_table", "t"}},
		{"SELECT x FROM t WHERE x NOT IN secret_table", []string{"secret_table", "t"}},
		{"SELECT x FROM t WHERE x IN aux2.secret_table", []string{"secret_table", "t"}},
		// negatives: no new base-table ref for these forms.
		{"SELECT x FROM t WHERE x IN (SELECT id FROM users)", []string{"t", "users"}},
		{"SELECT x FROM t WHERE x IN (1, 2, 3)", []string{"t"}},
	}
	for _, tc := range cases {
		tree, err := Frontend{}.Parse(tc.sql)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.sql, err)
		}
		var got []string
		for _, tr := range tree.DeepTables() {
			got = append(got, tr.Name)
		}
		slices.Sort(got)
		if !slices.Equal(got, tc.want) {
			t.Errorf("DeepTables(%q) = %v, want %v", tc.sql, got, tc.want)
		}
	}
}

// TestFrontend_DeepTables_InTableSchemaQualifier checks the schema
// qualifier survives on a db-qualified IN-table operand (coordinates
// with finding M8: a db qualifier disables nullability narrowing).
func TestFrontend_DeepTables_InTableSchemaQualifier(t *testing.T) {
	tree, err := Frontend{}.Parse("SELECT x FROM t WHERE x IN aux2.secret_table")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found *dialect.TableRef
	for _, tr := range tree.DeepTables() {
		if tr.Name == "secret_table" {
			cp := tr
			found = &cp
		}
	}
	if found == nil {
		t.Fatal("secret_table not in DeepTables")
	}
	if found.Schema != "aux2" {
		t.Errorf("secret_table schema = %q, want %q", found.Schema, "aux2")
	}
	if !tree.HasUnresolvableProvenance() {
		t.Error("HasUnresolvableProvenance = false; a db-qualified IN-table operand must disable narrowing")
	}
}

// TestProbe_RejectsCompoundTail pins finding M6: a set-operation
// (UNION/INTERSECT/EXCEPT) tail must not be smuggled into a join-item
// or group-by fragment past R1's one-node-per-slot probe.
func TestProbe_RejectsCompoundTail(t *testing.T) {
	f := Frontend{}
	// Must now be rejected (a whole set-op branch is extra structure).
	if err := f.ProbeJoinItem("JOIN x ON 1 UNION SELECT 2"); err == nil {
		t.Error("ProbeJoinItem accepted a UNION tail")
	}
	if err := f.ProbeGroupBy("GROUP BY a UNION SELECT 2"); err == nil {
		t.Error("ProbeGroupBy accepted a UNION tail")
	}
	// Plain forms must still pass.
	if err := f.ProbeJoinItem("JOIN x ON 1"); err != nil {
		t.Errorf("ProbeJoinItem rejected a plain join item: %v", err)
	}
	if err := f.ProbeGroupBy("GROUP BY a"); err != nil {
		t.Errorf("ProbeGroupBy rejected a plain GROUP BY: %v", err)
	}
}

// TestFrontend_InsertSchemaQualifier pins finding M8: rqlite's
// InsertStatement carries Schema, but Relations() and DeepTables()
// previously emitted the INSERT target with Schema:"". So
// `INSERT INTO temp.t … RETURNING x` lost its db qualifier and
// HasUnresolvableProvenance() returned false, letting the bare name "t"
// cross-resolve to main.t's NOT NULL and narrow the RETURNING column
// unsoundly while the write lands in temp.t.
func TestFrontend_InsertSchemaQualifier(t *testing.T) {
	tree, err := Frontend{}.Parse("INSERT INTO temp.t (x) VALUES (1) RETURNING x")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	rels := tree.Relations()
	if len(rels) != 1 {
		t.Fatalf("Relations() = %d refs, want 1", len(rels))
	}
	if rels[0].Table != "t" || rels[0].Schema != "temp" {
		t.Errorf("Relations()[0] = {Table:%q Schema:%q}, want {t temp}", rels[0].Table, rels[0].Schema)
	}

	var target *dialect.TableRef
	for _, tr := range tree.DeepTables() {
		if tr.Name == "t" {
			cp := tr
			target = &cp
		}
	}
	if target == nil {
		t.Fatal("INSERT target t not in DeepTables")
	}
	if target.Schema != "temp" {
		t.Errorf("DeepTables target schema = %q, want %q", target.Schema, "temp")
	}
	if !tree.HasUnresolvableProvenance() {
		t.Error("HasUnresolvableProvenance = false; the temp. qualifier on the INSERT target was dropped, so narrowing stays enabled")
	}
}
