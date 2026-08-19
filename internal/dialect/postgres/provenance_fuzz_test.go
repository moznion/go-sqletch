package postgres

import (
	"testing"

	pgquery "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// FuzzProvenanceFlags guards the recursive-provenance facades against
// parser-AST drift: DerivedRels/CTEs/HasSetOperation/HasGroupingSets
// are hand-walks over specific pg_query node shapes, and a pg_query
// upgrade that introduces a new wrapper node in FROM position (the
// way RangeTableSample wraps a RangeVar) could hide a derived table
// from them — which would grant narrowing the analyzer must not have
// (design 05 §2b). The oracle is an independent protobuf-reflection
// scan of each LEVEL with the same boundaries the facades use
// (sublinks skipped; subquery bodies counted, not entered), compared
// against the facade's local enumeration, recursing exactly where the
// analyzer recurses.
func FuzzProvenanceFlags(f *testing.F) {
	for _, seed := range []string{
		"SELECT a FROM t",
		"SELECT t1.a, t2.b FROM t1 LEFT JOIN t2 ON t2.a = t1.a",
		"SELECT s.a FROM t1 LEFT JOIN (SELECT a FROM t2) s ON s.a = t1.a",
		"WITH s AS (SELECT a FROM t2) SELECT s.a FROM s",
		"WITH RECURSIVE r AS (SELECT 1 AS n UNION ALL SELECT n+1 FROM r) SELECT n FROM r",
		"WITH ins AS (INSERT INTO t (a) VALUES (1) RETURNING a) SELECT ins.a FROM ins",
		"SELECT a FROM t1 UNION ALL SELECT b FROM t2",
		"WITH x AS (SELECT 1) SELECT a FROM t1 UNION ALL SELECT b FROM t2",
		"SELECT a, count(*) FROM t GROUP BY ROLLUP(a)",
		"SELECT a FROM t1 TABLESAMPLE SYSTEM (50)",
		"SELECT a FROM t WHERE EXISTS (SELECT 1 FROM (SELECT b FROM u) s)",
		"UPDATE t SET a = s.a FROM (SELECT a FROM t2) s RETURNING t.a",
		"WITH s AS (SELECT a FROM t2) DELETE FROM t USING s WHERE t.a = s.a",
		"SELECT s.a FROM (SELECT a FROM (SELECT a FROM t2) inner2) s",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		tr, err := Frontend{}.Parse(sql)
		if err != nil {
			return
		}
		switch tr.Kind() {
		case dialect.StmtSelect, dialect.StmtUpdate, dialect.StmtDelete:
		default:
			return // INSERT is target-attributed by design; others never narrow
		}
		checkLevel(t, tr.(*tree), sql)
	})
}

// checkLevel compares one level's facade enumeration against the
// reflection oracle, then recurses into the facade-provided subtrees
// exactly like collectPresence does.
func checkLevel(t *testing.T, tr *tree, sql string) {
	n := tr.stmt()
	if n == nil {
		return
	}
	var got levelCounts
	scanLevel(n.ProtoReflect(), true, &got)
	if len(tr.DerivedRels()) != got.derived {
		t.Fatalf("facade sees %d derived tables, reflection %d:\n%s",
			len(tr.DerivedRels()), got.derived, sql)
	}
	if len(tr.CTEs()) != got.ctes {
		t.Fatalf("facade sees %d CTEs, reflection %d:\n%s", len(tr.CTEs()), got.ctes, sql)
	}
	if tr.HasSetOperation() != got.setOp {
		t.Fatalf("HasSetOperation = %v, reflection %v:\n%s", tr.HasSetOperation(), got.setOp, sql)
	}
	if tr.HasGroupingSets() != got.grouping {
		t.Fatalf("HasGroupingSets = %v, reflection %v:\n%s", tr.HasGroupingSets(), got.grouping, sql)
	}
	if tr.HasSetOperation() {
		return // the analyzer poisons below a set operation; so do we
	}
	for _, d := range tr.DerivedRels() {
		checkLevel(t, d.Tree.(*tree), sql)
	}
	for _, c := range tr.CTEs() {
		if c.Tree != nil {
			checkLevel(t, c.Tree.(*tree), sql)
		}
	}
}

type levelCounts struct {
	derived  int
	ctes     int
	setOp    bool
	grouping bool
}

// scanLevel walks one statement level: sublinks are skipped whole,
// RangeSubselect/CommonTableExpr are counted but not entered, and
// nested select statements (set-operation branches) are not entered —
// the exact boundaries the facades observe. allowStmt permits
// descending into the level's own root statement node only.
func scanLevel(m protoreflect.Message, allowStmt bool, out *levelCounts) {
	switch v := m.Interface().(type) {
	case *pgquery.SubLink:
		return
	case *pgquery.RangeSubselect:
		out.derived++
		return
	case *pgquery.CommonTableExpr:
		out.ctes++
		return
	case *pgquery.GroupingSet:
		out.grouping = true
	case *pgquery.SelectStmt:
		if !allowStmt {
			return
		}
		if v.Op != pgquery.SetOperation_SETOP_NONE {
			out.setOp = true
			// Branches are unreachable to the facade; WITH still
			// belongs to this level, so keep walking non-branch
			// fields below with statement descent disabled.
		}
		allowStmt = false
		m.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
			if fd.Name() == "larg" || fd.Name() == "rarg" {
				return true
			}
			scanValue(fd, val, allowStmt, out)
			return true
		})
		return
	case *pgquery.UpdateStmt, *pgquery.DeleteStmt:
		if !allowStmt {
			return
		}
		allowStmt = false
	}
	m.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
		scanValue(fd, val, allowStmt, out)
		return true
	})
}

func scanValue(fd protoreflect.FieldDescriptor, val protoreflect.Value, allowStmt bool, out *levelCounts) {
	switch {
	case fd.IsList() && fd.Kind() == protoreflect.MessageKind:
		l := val.List()
		for i := 0; i < l.Len(); i++ {
			scanLevel(l.Get(i).Message(), allowStmt, out)
		}
	case fd.Kind() == protoreflect.MessageKind && !fd.IsMap():
		scanLevel(val.Message(), allowStmt, out)
	}
}
