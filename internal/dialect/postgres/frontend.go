package postgres

import (
	"errors"
	"fmt"
	"strings"

	pgquery "github.com/pganalyze/pg_query_go/v6"
	pgparser "github.com/pganalyze/pg_query_go/v6/parser"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/moznion/sqletch/internal/dialect"
)

// Frontend is the pg_query-backed grammar frontend (design 02 §3).
type Frontend struct{}

var _ dialect.Frontend = Frontend{}

func (Frontend) Parse(sql string) (dialect.Tree, error) {
	res, err := pgquery.Parse(sql)
	if err != nil {
		return nil, toParseError(err)
	}
	return &tree{res: res}, nil
}

func toParseError(err error) error {
	pos := 0
	msg := err.Error()
	var pe *pgparser.Error
	if errors.As(err, &pe) {
		// Cursorpos is 1-based; 0 means unknown.
		if pe.Cursorpos > 0 {
			pos = pe.Cursorpos - 1
		}
		msg = pe.Message
	}
	return &dialect.ParseError{Pos: pos, Msg: msg}
}

// ProbeExpr checks that expr forms exactly one boolean-position
// expression: it must parse when parenthesized as a WHERE qual.
// Balance of parentheses is the caller's (rules') concern via the
// lexer; parse failure here means the body is not one expression.
func (f Frontend) ProbeExpr(expr string) error {
	res, err := pgquery.Parse("SELECT 1 WHERE (" + expr + "\n)")
	if err != nil {
		return toParseError(err)
	}
	sel := singleSelect(res)
	if sel == nil || sel.WhereClause == nil || len(sel.FromClause) != 0 ||
		len(sel.GroupClause) != 0 || len(sel.SortClause) != 0 ||
		sel.LimitCount != nil || sel.LimitOffset != nil || len(sel.LockingClause) != 0 {
		return &dialect.ParseError{Pos: 0, Msg: "fragment is not a single predicate expression"}
	}
	return nil
}

// ProbeJoinItem checks that item is a join item attachable to a
// preceding FROM entry: `SELECT 1 FROM sqletch_probe_t <item>` must
// parse into exactly one FROM tree and contribute nothing else.
func (f Frontend) ProbeJoinItem(item string) error {
	res, err := pgquery.Parse("SELECT 1 FROM sqletch_probe_t " + item)
	if err != nil {
		return toParseError(err)
	}
	sel := singleSelect(res)
	if sel == nil || len(sel.FromClause) != 1 ||
		sel.WhereClause != nil || len(sel.GroupClause) != 0 ||
		len(sel.SortClause) != 0 || sel.LimitCount != nil ||
		len(sel.LockingClause) != 0 {
		return &dialect.ParseError{Pos: 0, Msg: "fragment is not a single join item"}
	}
	if sel.FromClause[0].GetJoinExpr() == nil {
		return &dialect.ParseError{Pos: 0, Msg: "fragment does not join onto the preceding FROM entry"}
	}
	return nil
}

// ProbeOrderBy checks that clause is exactly one statement-level
// ORDER BY clause and nothing more.
func (f Frontend) ProbeOrderBy(clause string) error {
	res, err := pgquery.Parse("SELECT 1 " + clause)
	if err != nil {
		return toParseError(err)
	}
	sel := singleSelect(res)
	if sel == nil || len(sel.SortClause) == 0 ||
		sel.WhereClause != nil || len(sel.FromClause) != 0 ||
		len(sel.GroupClause) != 0 || sel.LimitCount != nil ||
		sel.LimitOffset != nil || len(sel.LockingClause) != 0 ||
		len(sel.DistinctClause) != 0 {
		return &dialect.ParseError{Pos: 0, Msg: "fragment is not a bare ORDER BY clause"}
	}
	return nil
}

// ProbeSetItem checks that item is exactly one UPDATE SET assignment.
func (f Frontend) ProbeSetItem(item string) error {
	res, err := pgquery.Parse("UPDATE sqletch_probe_t SET " + item)
	if err != nil {
		return toParseError(err)
	}
	if len(res.Stmts) != 1 {
		return &dialect.ParseError{Pos: 0, Msg: "fragment is not a single SET assignment"}
	}
	up := res.Stmts[0].Stmt.GetUpdateStmt()
	if up == nil || len(up.TargetList) != 1 || up.WhereClause != nil ||
		len(up.FromClause) != 0 || len(up.ReturningList) != 0 {
		return &dialect.ParseError{Pos: 0, Msg: "fragment is not a single SET assignment"}
	}
	return nil
}

// ProbeInsertValue checks that expr is exactly one VALUES row item
// (including the DEFAULT keyword, which is only legal there).
func (f Frontend) ProbeInsertValue(expr string) error {
	res, err := pgquery.Parse("INSERT INTO sqletch_probe_t (c) VALUES (" + expr + "\n)")
	if err != nil {
		return toParseError(err)
	}
	bad := &dialect.ParseError{Pos: 0, Msg: "fragment is not a single VALUES item"}
	if len(res.Stmts) != 1 {
		return bad
	}
	ins := res.Stmts[0].Stmt.GetInsertStmt()
	if ins == nil || len(ins.ReturningList) != 0 || ins.OnConflictClause != nil || ins.SelectStmt == nil {
		return bad
	}
	sel := ins.SelectStmt.GetSelectStmt()
	if sel == nil || len(sel.ValuesLists) != 1 {
		return bad
	}
	row := sel.ValuesLists[0].GetList()
	if row == nil || len(row.Items) != 1 {
		return bad
	}
	return nil
}

func singleSelect(res *pgquery.ParseResult) *pgquery.SelectStmt {
	if len(res.Stmts) != 1 {
		return nil
	}
	return res.Stmts[0].Stmt.GetSelectStmt()
}

// ---- Tree facade -----------------------------------------------------------

type tree struct {
	res *pgquery.ParseResult
}

func (t *tree) StmtCount() int { return len(t.res.Stmts) }

func (t *tree) stmt() *pgquery.Node {
	if len(t.res.Stmts) == 0 {
		return nil
	}
	return t.res.Stmts[0].Stmt
}

func (t *tree) Kind() dialect.StmtKind {
	n := t.stmt()
	switch {
	case n == nil:
		return dialect.StmtOther
	case n.GetSelectStmt() != nil:
		return dialect.StmtSelect
	case n.GetUpdateStmt() != nil:
		return dialect.StmtUpdate
	case n.GetInsertStmt() != nil:
		return dialect.StmtInsert
	case n.GetDeleteStmt() != nil:
		return dialect.StmtDelete
	default:
		return dialect.StmtOther
	}
}

func (t *tree) sel() *pgquery.SelectStmt {
	n := t.stmt()
	if n == nil {
		return nil
	}
	return n.GetSelectStmt()
}

func (t *tree) Relations() []dialect.RelRef {
	var out []dialect.RelRef
	n := t.stmt()
	if n == nil {
		return nil
	}
	switch {
	case n.GetSelectStmt() != nil:
		for _, item := range n.GetSelectStmt().FromClause {
			collectFromItem(item, dialect.JoinBase, false, &out)
		}
	case n.GetUpdateStmt() != nil:
		u := n.GetUpdateStmt()
		if u.Relation != nil {
			out = append(out, relFromRangeVar(u.Relation, dialect.JoinBase, false))
		}
		for _, item := range u.FromClause {
			collectFromItem(item, dialect.JoinBase, false, &out)
		}
	case n.GetDeleteStmt() != nil:
		d := n.GetDeleteStmt()
		if d.Relation != nil {
			out = append(out, relFromRangeVar(d.Relation, dialect.JoinBase, false))
		}
		for _, item := range d.UsingClause {
			collectFromItem(item, dialect.JoinBase, false, &out)
		}
	case n.GetInsertStmt() != nil:
		i := n.GetInsertStmt()
		if i.Relation != nil {
			out = append(out, relFromRangeVar(i.Relation, dialect.JoinBase, false))
		}
	}
	return out
}

func relFromRangeVar(rv *pgquery.RangeVar, join dialect.JoinType, nullable bool) dialect.RelRef {
	alias := ""
	if rv.Alias != nil {
		alias = rv.Alias.Aliasname
	}
	return dialect.RelRef{
		Alias: alias, Table: rv.Relname, Loc: int(rv.Location),
		Join: join, NullableSide: nullable,
	}
}

func mapJoinType(jt pgquery.JoinType, isNatural bool, quals *pgquery.Node) dialect.JoinType {
	switch jt {
	case pgquery.JoinType_JOIN_INNER:
		// pg_query models CROSS JOIN as inner join without quals.
		if quals == nil && !isNatural {
			return dialect.JoinCross
		}
		return dialect.JoinInner
	case pgquery.JoinType_JOIN_LEFT:
		return dialect.JoinLeft
	case pgquery.JoinType_JOIN_RIGHT:
		return dialect.JoinRight
	case pgquery.JoinType_JOIN_FULL:
		return dialect.JoinFull
	default:
		return dialect.JoinOther
	}
}

// collectFromItem flattens the FROM tree. nullable tracks whether the
// current subtree is on a null-extended side of an enclosing outer
// join: LEFT null-extends its right operand, RIGHT its left, FULL
// both.
func collectFromItem(node *pgquery.Node, join dialect.JoinType, nullable bool, out *[]dialect.RelRef) {
	switch {
	case node.GetRangeVar() != nil:
		*out = append(*out, relFromRangeVar(node.GetRangeVar(), join, nullable))
	case node.GetJoinExpr() != nil:
		je := node.GetJoinExpr()
		leftNullable, rightNullable := nullable, nullable
		switch je.Jointype {
		case pgquery.JoinType_JOIN_LEFT:
			rightNullable = true
		case pgquery.JoinType_JOIN_RIGHT:
			leftNullable = true
		case pgquery.JoinType_JOIN_FULL:
			leftNullable, rightNullable = true, true
		}
		collectFromItem(je.Larg, join, leftNullable, out)
		collectFromItem(je.Rarg, mapJoinType(je.Jointype, je.IsNatural, je.Quals), rightNullable, out)
	case node.GetRangeSubselect() != nil:
		rs := node.GetRangeSubselect()
		alias := ""
		if rs.Alias != nil {
			alias = rs.Alias.Aliasname
		}
		*out = append(*out, dialect.RelRef{Alias: alias, Loc: -1, Join: join, NullableSide: nullable})
	case node.GetRangeFunction() != nil:
		rf := node.GetRangeFunction()
		alias := ""
		if rf.Alias != nil {
			alias = rf.Alias.Aliasname
		}
		*out = append(*out, dialect.RelRef{Alias: alias, Loc: -1, Join: join, NullableSide: nullable})
	}
}

// ColumnRefs walks the whole statement (protobuf reflection) and
// collects every ColumnRef, marking those inside subquery scopes
// (SubLink, derived tables, CTEs) — the resolver treats those
// conservatively (design 03 §6).
func (t *tree) ColumnRefs() []dialect.ColRef {
	n := t.stmt()
	if n == nil {
		return nil
	}
	var out []dialect.ColRef
	collectColRefs(n.ProtoReflect(), false, &out)
	return out
}

func collectColRefs(m protoreflect.Message, inSub bool, out *[]dialect.ColRef) {
	switch v := m.Interface().(type) {
	case *pgquery.ColumnRef:
		cr := dialect.ColRef{Loc: int(v.Location), InSubquery: inSub}
		for _, f := range v.Fields {
			if s := f.GetString_(); s != nil {
				cr.Fields = append(cr.Fields, s.Sval)
			}
			if f.GetAStar() != nil {
				cr.Star = true
			}
		}
		*out = append(*out, cr)
		return
	case *pgquery.SubLink, *pgquery.RangeSubselect, *pgquery.CommonTableExpr:
		inSub = true
	}
	m.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Kind() == protoreflect.MessageKind:
			l := val.List()
			for i := 0; i < l.Len(); i++ {
				collectColRefs(l.Get(i).Message(), inSub, out)
			}
		case fd.Kind() == protoreflect.MessageKind && !fd.IsMap():
			collectColRefs(val.Message(), inSub, out)
		}
		return true
	})
}

func (t *tree) TargetItems() []dialect.TargetItem {
	sel := t.sel()
	if sel == nil {
		return nil
	}
	var out []dialect.TargetItem
	for _, n := range sel.TargetList {
		rt := n.GetResTarget()
		if rt == nil {
			continue
		}
		item := dialect.TargetItem{Name: rt.Name, Loc: int(rt.Location)}
		if cr := rt.Val.GetColumnRef(); cr != nil {
			var quals []string
			for _, f := range cr.Fields {
				if s := f.GetString_(); s != nil {
					quals = append(quals, s.Sval)
				}
				if f.GetAStar() != nil {
					item.Star = true
				}
			}
			if item.Star && len(quals) > 0 {
				item.Qualifier = quals[len(quals)-1]
			}
		}
		if fc := rt.Val.GetFuncCall(); fc != nil {
			for _, fn := range fc.Funcname {
				if s := fn.GetString_(); s != nil {
					item.FuncName = strings.ToLower(s.Sval)
				}
			}
		}
		out = append(out, item)
	}
	return out
}

func (t *tree) TopConjunctLocs() []int {
	var where *pgquery.Node
	n := t.stmt()
	switch {
	case n == nil:
		return nil
	case n.GetSelectStmt() != nil:
		where = n.GetSelectStmt().WhereClause
	case n.GetUpdateStmt() != nil:
		where = n.GetUpdateStmt().WhereClause
	case n.GetDeleteStmt() != nil:
		where = n.GetDeleteStmt().WhereClause
	}
	var locs []int
	flattenConjuncts(where, &locs)
	return locs
}

func flattenConjuncts(node *pgquery.Node, out *[]int) {
	if node == nil {
		return
	}
	if be := node.GetBoolExpr(); be != nil && be.Boolop == pgquery.BoolExprType_AND_EXPR {
		for _, arg := range be.Args {
			flattenConjuncts(arg, out)
		}
		return
	}
	*out = append(*out, nodeLoc(node))
}

// nodeLoc extracts the location of the AST node kinds that can appear
// as conjuncts or sort keys; -1 when unknown.
func nodeLoc(node *pgquery.Node) int {
	switch {
	case node == nil:
		return -1
	case node.GetAExpr() != nil:
		return int(node.GetAExpr().Location)
	case node.GetColumnRef() != nil:
		return int(node.GetColumnRef().Location)
	case node.GetAConst() != nil:
		return int(node.GetAConst().Location)
	case node.GetFuncCall() != nil:
		return int(node.GetFuncCall().Location)
	case node.GetBoolExpr() != nil:
		return int(node.GetBoolExpr().Location)
	case node.GetSubLink() != nil:
		return int(node.GetSubLink().Location)
	case node.GetNullTest() != nil:
		return int(node.GetNullTest().Location)
	case node.GetBooleanTest() != nil:
		return int(node.GetBooleanTest().Location)
	case node.GetTypeCast() != nil:
		return int(node.GetTypeCast().Location)
	case node.GetParamRef() != nil:
		return int(node.GetParamRef().Location)
	case node.GetCaseExpr() != nil:
		return int(node.GetCaseExpr().Location)
	case node.GetCoalesceExpr() != nil:
		return int(node.GetCoalesceExpr().Location)
	default:
		return -1
	}
}

func (t *tree) OrderByLocs() []int {
	sel := t.sel()
	if sel == nil {
		return nil
	}
	var locs []int
	for _, sb := range sel.SortClause {
		if s := sb.GetSortBy(); s != nil {
			locs = append(locs, nodeLoc(s.Node))
		}
	}
	return locs
}

func (t *tree) HasDistinctOn() bool {
	sel := t.sel()
	if sel == nil {
		return false
	}
	for _, d := range sel.DistinctClause {
		// Plain DISTINCT is a single nil-content node; DISTINCT ON
		// carries real expressions.
		if d != nil && d.Node != nil {
			return true
		}
	}
	return false
}

func (t *tree) HasLockingClause() bool {
	sel := t.sel()
	return sel != nil && len(sel.LockingClause) > 0
}

// DebugString aids test failures.
func (t *tree) DebugString() string {
	var b strings.Builder
	fmt.Fprintf(&b, "stmts=%d kind=%v rels=%v", t.StmtCount(), t.Kind(), t.Relations())
	return b.String()
}
