package postgres

import (
	"testing"

	pgquery "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// FuzzProvenanceFlags guards the nullability kill-switches against
// parser-AST drift: HasOpaqueProvenance/HasGroupingSets are
// hand-walks over specific node shapes, and a pg_query upgrade that
// introduces a new wrapper node in FROM position (the way
// RangeTableSample wraps a RangeVar) could hide a derived table from
// them. The oracle here is an independent protobuf-reflection sweep:
// whenever BOTH flags are false — i.e. Analyze would trust SrcRel
// narrowing — the whole statement must contain no derived table, CTE,
// set operation, or grouping set outside sublink scopes (sublinks are
// not FROM-reachable and carry no column origin).
func FuzzProvenanceFlags(f *testing.F) {
	for _, seed := range []string{
		"SELECT a FROM t",
		"SELECT t1.a, t2.b FROM t1 LEFT JOIN t2 ON t2.a = t1.a",
		"SELECT s.a FROM t1 LEFT JOIN (SELECT a FROM t2) s ON s.a = t1.a",
		"WITH s AS (SELECT a FROM t2) SELECT s.a FROM s",
		"SELECT a FROM t1 UNION ALL SELECT b FROM t2",
		"SELECT a, count(*) FROM t GROUP BY ROLLUP(a)",
		"SELECT a FROM t1 TABLESAMPLE SYSTEM (50)",
		"SELECT a FROM t WHERE EXISTS (SELECT 1 FROM (SELECT b FROM u) s)",
		"UPDATE t SET a = s.a FROM (SELECT a FROM t2) s RETURNING t.a",
		"WITH s AS (SELECT a FROM t2) DELETE FROM t USING s WHERE t.a = s.a",
		"SELECT (SELECT max(b) FROM u), a FROM t GROUP BY a",
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
		if tr.HasOpaqueProvenance() || tr.HasGroupingSets() {
			return // narrowing is off; nothing to guard
		}
		pt := tr.(*tree)
		n := pt.stmt()
		if n == nil {
			return
		}
		if hazardOutsideSublink(n.ProtoReflect(), false) {
			t.Fatalf("flags report trustworthy provenance but the tree contains an opaque construct:\n%s", sql)
		}
	})
}

// hazardOutsideSublink is the reflection oracle: any CommonTableExpr,
// RangeSubselect, set-operation SelectStmt, or GroupingSet reachable
// without crossing a SubLink boundary.
func hazardOutsideSublink(m protoreflect.Message, inSub bool) bool {
	switch v := m.Interface().(type) {
	case *pgquery.SubLink:
		inSub = true
	case *pgquery.CommonTableExpr, *pgquery.RangeSubselect, *pgquery.GroupingSet:
		if !inSub {
			return true
		}
	case *pgquery.SelectStmt:
		if !inSub && v.Op != pgquery.SetOperation_SETOP_NONE {
			return true
		}
	}
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Kind() == protoreflect.MessageKind:
			l := val.List()
			for i := 0; i < l.Len(); i++ {
				if hazardOutsideSublink(l.Get(i).Message(), inSub) {
					found = true
					return false
				}
			}
		case fd.Kind() == protoreflect.MessageKind && !fd.IsMap():
			if hazardOutsideSublink(val.Message(), inSub) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}
