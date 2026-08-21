package postgres

import (
	"errors"
	"fmt"
	"strings"

	pgquery "github.com/pganalyze/pg_query_go/v6"
	pgparser "github.com/pganalyze/pg_query_go/v6/parser"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/moznion/go-sqletch/internal/dialect"
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

// subTree wraps a subquery node (a RangeSubselect body or a CTE body)
// in its own facade for recursive analysis. Node locations stay
// absolute offsets into the ORIGINAL sql string, so fragment-range
// checks keep working at any depth.
func subTree(node *pgquery.Node) *tree { return &tree{node: node} }

func toParseError(err error) error {
	pos := 0
	msg := err.Error()
	if pe, ok := errors.AsType[*pgparser.Error](err); ok {
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
	bad := &dialect.ParseError{Pos: 0, Msg: "fragment is not a single join item"}
	if sel == nil || len(sel.FromClause) != 1 ||
		sel.WhereClause != nil || len(sel.GroupClause) != 0 ||
		sel.HavingClause != nil || len(sel.SortClause) != 0 ||
		sel.LimitCount != nil || sel.LimitOffset != nil ||
		len(sel.WindowClause) != 0 || len(sel.DistinctClause) != 0 ||
		len(sel.LockingClause) != 0 {
		return bad
	}
	if sel.FromClause[0].GetJoinExpr() == nil {
		return &dialect.ParseError{Pos: 0, Msg: "fragment does not join onto the preceding FROM entry"}
	}
	// The fragment must introduce exactly ONE joined relation onto the
	// probe table: the flattened FROM tree therefore holds exactly two
	// relations (probe + join). A single JoinExpr can nest a whole CHAIN
	// of joins (`JOIN a ON … JOIN (SELECT …) AS d ON …`); such a chain
	// smuggles extra — possibly derived (Loc==-1), guard-detached —
	// relations past R2/R3, so reject anything but a two-relation join
	// (mirrors the mysql/sqlite probes).
	var rels []dialect.RelRef
	collectFromItem(sel.FromClause[0], dialect.JoinBase, false, &rels)
	if len(rels) != 2 {
		return bad
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

// ProbeOrderByKey checks that expr is exactly one sort key.
func (f Frontend) ProbeOrderByKey(expr string) error {
	res, err := pgquery.Parse("SELECT 1 ORDER BY " + expr + "\n")
	if err != nil {
		return toParseError(err)
	}
	sel := singleSelect(res)
	if sel == nil || len(sel.SortClause) != 1 ||
		sel.WhereClause != nil || len(sel.FromClause) != 0 ||
		len(sel.GroupClause) != 0 || sel.HavingClause != nil ||
		sel.LimitCount != nil || sel.LimitOffset != nil ||
		len(sel.WindowClause) != 0 || len(sel.DistinctClause) != 0 ||
		len(sel.LockingClause) != 0 {
		return &dialect.ParseError{Pos: 0, Msg: "fragment is not a single sort key"}
	}
	return nil
}

// ProbeGroupBy checks that clause is exactly one statement-level
// GROUP BY clause and nothing more.
func (f Frontend) ProbeGroupBy(clause string) error {
	res, err := pgquery.Parse("SELECT 1 " + clause)
	if err != nil {
		return toParseError(err)
	}
	sel := singleSelect(res)
	if sel == nil || len(sel.GroupClause) == 0 ||
		sel.WhereClause != nil || len(sel.FromClause) != 0 ||
		len(sel.SortClause) != 0 || sel.LimitCount != nil ||
		sel.LimitOffset != nil || len(sel.LockingClause) != 0 ||
		sel.HavingClause != nil || len(sel.DistinctClause) != 0 {
		return &dialect.ParseError{Pos: 0, Msg: "fragment is not a bare GROUP BY clause"}
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
	res  *pgquery.ParseResult // top-level parse; nil for subtrees
	node *pgquery.Node        // subtree root when res is nil
}

func (t *tree) StmtCount() int {
	if t.res == nil {
		return 1
	}
	return len(t.res.Stmts)
}

func (t *tree) stmt() *pgquery.Node {
	if t.res == nil {
		return t.node
	}
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
		Alias: alias, Table: rv.Relname, Schema: rv.Schemaname,
		Only: !rv.Inh, Loc: int(rv.Location),
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
	case node.GetRangeTableSample() != nil:
		// TABLESAMPLE wraps a plain relation; its rows are genuine
		// table rows, so the relation participates normally.
		if r := node.GetRangeTableSample().Relation; r != nil {
			collectFromItem(r, join, nullable, out)
		}
	}
}

// HasSetOperation reports a statement-level UNION/INTERSECT/EXCEPT.
func (t *tree) HasSetOperation() bool {
	sel := t.sel()
	return sel != nil && sel.Op != pgquery.SetOperation_SETOP_NONE
}

// HasUnresolvableProvenance is always false: PostgreSQL attributes
// result columns by OID (resorigtbl), immune to name collisions.
func (t *tree) HasUnresolvableProvenance() bool { return false }

// fromItems returns the statement's FROM-position item list.
func (t *tree) fromItems() []*pgquery.Node {
	n := t.stmt()
	switch {
	case n == nil:
		return nil
	case n.GetSelectStmt() != nil:
		return n.GetSelectStmt().FromClause
	case n.GetUpdateStmt() != nil:
		return n.GetUpdateStmt().FromClause
	case n.GetDeleteStmt() != nil:
		return n.GetDeleteStmt().UsingClause
	}
	return nil
}

// DerivedRels collects FROM-reachable derived tables with sub-facades,
// tracking null-extension exactly like collectFromItem.
func (t *tree) DerivedRels() []dialect.SubRel {
	var out []dialect.SubRel
	for _, item := range t.fromItems() {
		collectDerived(item, false, &out)
	}
	return out
}

func collectDerived(node *pgquery.Node, nullable bool, out *[]dialect.SubRel) {
	switch {
	case node == nil:
		return
	case node.GetRangeSubselect() != nil:
		rs := node.GetRangeSubselect()
		alias := ""
		if rs.Alias != nil {
			alias = rs.Alias.Aliasname
		}
		*out = append(*out, dialect.SubRel{
			Alias: alias, NullableSide: nullable, Tree: subTree(rs.Subquery),
		})
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
		collectDerived(je.Larg, leftNullable, out)
		collectDerived(je.Rarg, rightNullable, out)
	case node.GetRangeTableSample() != nil:
		collectDerived(node.GetRangeTableSample().Relation, nullable, out)
	}
}

// CTEs returns the statement's WITH-list definitions; a
// data-modifying body yields a nil Tree.
func (t *tree) CTEs() []dialect.CTEDef {
	var wc *pgquery.WithClause
	n := t.stmt()
	switch {
	case n == nil:
		return nil
	case n.GetSelectStmt() != nil:
		wc = n.GetSelectStmt().WithClause
	case n.GetUpdateStmt() != nil:
		wc = n.GetUpdateStmt().WithClause
	case n.GetDeleteStmt() != nil:
		wc = n.GetDeleteStmt().WithClause
	case n.GetInsertStmt() != nil:
		wc = n.GetInsertStmt().WithClause
	}
	if wc == nil {
		return nil
	}
	var out []dialect.CTEDef
	for _, c := range wc.Ctes {
		cte := c.GetCommonTableExpr()
		if cte == nil {
			continue
		}
		def := dialect.CTEDef{Name: cte.Ctename, Recursive: wc.Recursive}
		switch {
		case cte.Ctequery == nil:
		case cte.Ctequery.GetSelectStmt() != nil:
			def.Tree = subTree(cte.Ctequery)
		default:
			// A data-modifying body (DELETE/UPDATE/INSERT … RETURNING):
			// no plain-query Tree. PostgreSQL attributes its wCTE columns
			// through the RETURNING list to the base tables the DML reads,
			// and one of those may sit on a null-extended join side inside
			// the DML. Expose every table the body mentions so the analyzer
			// poisons them (design 05 §2b) — granting nothing is not enough
			// when a clean outer instance can vouch for the OID.
			var refs []dialect.TableRef
			collectRangeVars(cte.Ctequery.ProtoReflect(), &refs)
			def.PoisonTables = refs
		}
		out = append(out, def)
	}
	return out
}

func (t *tree) HasGroupingSets() bool {
	sel := t.sel()
	if sel == nil {
		return false
	}
	for _, g := range sel.GroupClause {
		if g.GetGroupingSet() != nil {
			return true
		}
	}
	return false
}

// DeepTables walks the whole statement (protobuf reflection) and
// collects every RangeVar name — subqueries and CTE bodies included,
// and CTE-name references with them (conservative, design 14 §11.1).
func (t *tree) DeepTables() []dialect.TableRef {
	n := t.stmt()
	if n == nil {
		return nil
	}
	var out []dialect.TableRef
	collectRangeVars(n.ProtoReflect(), &out)
	return out
}

func collectRangeVars(m protoreflect.Message, out *[]dialect.TableRef) {
	if rv, ok := m.Interface().(*pgquery.RangeVar); ok {
		*out = append(*out, dialect.TableRef{
			Name: rv.Relname, Schema: rv.Schemaname, Loc: int(rv.Location),
		})
		return
	}
	m.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Kind() == protoreflect.MessageKind:
			l := val.List()
			for i := 0; i < l.Len(); i++ {
				collectRangeVars(l.Get(i).Message(), out)
			}
		case fd.Kind() == protoreflect.MessageKind && !fd.IsMap():
			collectRangeVars(val.Message(), out)
		}
		return true
	})
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
	collectColRefs(n.ProtoReflect(), nil, nil, false, &out)
	return out
}

// collectColRefs walks the statement collecting every ColumnRef.
// InSubquery is set (coarsely) for every descendant of a SubLink /
// derived table / CTE. scopes carries the effective FROM names of the
// enclosing subquery scopes (ScopeAliases): at a subquery boundary the
// names are appended ONLY when descending into that subquery's own
// select body (pointer-identity match), never into a sibling field such
// as a SubLink test expression, whose references belong to the enclosing
// scope and must stay checkable.
//
// enclosing carries the scope that surrounds the CURRENT select before
// its own FROM names were added, i.e. what a non-lateral WITH-clause body
// may legally see. A CTE body cannot reference the FROM items of the
// select that uses it, so descending into a select's with_clause resets
// the scope to enclosing — otherwise the using-select's FROM names (e.g.
// the CTE name itself) would leak in and wrongly shadow a same-named
// guarded top-level relation for a correlated CTE-body reference.
func collectColRefs(m protoreflect.Message, scopes, enclosing []string, inSub bool, out *[]dialect.ColRef) {
	if v, ok := m.Interface().(*pgquery.ColumnRef); ok {
		cr := dialect.ColRef{Loc: int(v.Location), InSubquery: inSub}
		if len(scopes) > 0 {
			cr.ScopeAliases = append([]string(nil), scopes...)
		}
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
	}
	var innerSelect *pgquery.Node
	var withClause *pgquery.WithClause
	switch v := m.Interface().(type) {
	case *pgquery.SubLink:
		innerSelect = v.Subselect
	case *pgquery.RangeSubselect:
		innerSelect = v.Subquery
	case *pgquery.CommonTableExpr:
		innerSelect = v.Ctequery
	case *pgquery.SelectStmt:
		withClause = v.WithClause
	}
	m.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind || fd.IsMap() {
			return true
		}
		childScopes, childEnclosing := scopes, enclosing
		// InSubquery is per-child, not per-node: a SubLink's Testexpr (the
		// IN/ANY/ALL left operand) lives in the OUTER scope even though it
		// is a sibling field of the subquery body, so only the child that
		// IS the subquery body (innerSelect) crosses the boundary. Marking
		// every child in-subquery would hide the LHS from R3, which skips
		// in-subquery references (see rules/resolved.go).
		childInSub := inSub
		if !fd.IsList() {
			msg := val.Message().Interface()
			if innerSelect != nil {
				if n, ok := msg.(*pgquery.Node); ok && n == innerSelect {
					// Descending into the subquery's own body: its FROM
					// names shadow same-named enclosing relations, and the
					// scope that enclosed this boundary becomes the new
					// with-clause visibility floor.
					childScopes = appendFromNames(scopes, innerSelect)
					childEnclosing = scopes
					childInSub = true
				}
			}
			if withClause != nil {
				if w, ok := msg.(*pgquery.WithClause); ok && w == withClause {
					// CTE bodies see only the enclosing scope, never this
					// select's own FROM names.
					childScopes = enclosing
					childEnclosing = enclosing
				}
			}
		}
		if fd.IsList() {
			l := val.List()
			for i := 0; i < l.Len(); i++ {
				collectColRefs(l.Get(i).Message(), childScopes, childEnclosing, childInSub, out)
			}
		} else {
			collectColRefs(val.Message(), childScopes, childEnclosing, childInSub, out)
		}
		return true
	})
}

// appendFromNames returns scopes extended with the effective FROM names
// (alias else relation name) of node's SELECT, if any. Set-operation
// branch FROMs are deliberately not descended: under-collecting a name
// only leaves the pre-existing sound check in place, whereas an extra
// name would wrongly suppress one.
func appendFromNames(scopes []string, node *pgquery.Node) []string {
	sel := node.GetSelectStmt()
	if sel == nil {
		return scopes
	}
	var names []string
	for _, item := range sel.FromClause {
		fromNames(item, &names)
	}
	if len(names) == 0 {
		return scopes
	}
	return append(append([]string(nil), scopes...), names...)
}

func fromNames(node *pgquery.Node, out *[]string) {
	switch {
	case node.GetRangeVar() != nil:
		rv := node.GetRangeVar()
		if rv.Alias != nil && rv.Alias.Aliasname != "" {
			*out = append(*out, rv.Alias.Aliasname)
		} else if rv.Relname != "" {
			*out = append(*out, rv.Relname)
		}
	case node.GetJoinExpr() != nil:
		je := node.GetJoinExpr()
		// A join alias renames the whole join and HIDES the inner
		// relation names (PostgreSQL §7.2.1.2): `(a JOIN b) AS j` exposes
		// only `j`, so a reference qualified by `a` inside a subquery over
		// this join is correlated outward and must stay checkable. Emitting
		// the inner names here would over-collect and wrongly suppress the
		// R3 guard for a same-named top-level relation.
		if je.Alias != nil && je.Alias.Aliasname != "" {
			*out = append(*out, je.Alias.Aliasname)
			return
		}
		fromNames(je.Larg, out)
		fromNames(je.Rarg, out)
	case node.GetRangeSubselect() != nil:
		if a := node.GetRangeSubselect().Alias; a != nil && a.Aliasname != "" {
			*out = append(*out, a.Aliasname)
		}
	case node.GetRangeFunction() != nil:
		if a := node.GetRangeFunction().Alias; a != nil && a.Aliasname != "" {
			*out = append(*out, a.Aliasname)
		}
	case node.GetRangeTableSample() != nil:
		fromNames(node.GetRangeTableSample().Relation, out)
	}
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
			if name, ok := builtinFuncName(fc.Funcname); ok {
				item.FuncName = name
			}
			item.AggArg = aggArg(fc)
		}
		item.Total = totalNode(rt.Val)
		out = append(out, item)
	}
	return out
}

// builtinFuncName returns the lowercased builtin name of a function
// call ONLY when the call names a builtin unambiguously: an UNqualified
// call (a single name part) or one explicitly qualified with the
// pg_catalog schema. A user-schema-qualified call — `myschema.count(x)`,
// `myschema.now()` — names a user function that merely shares a
// builtin's spelling and must NOT inherit the builtin's totality or
// strict-aggregate treatment in the nullability analyzer. Returning
// ok=false there leaves TargetItem.FuncName empty so neither the
// funcWhitelist nor the strictAggs lookup can match. The pg_catalog
// comparison is exact (not case-folded): the parser already downcases
// unquoted identifiers to `pg_catalog`, while a quoted `"PG_CATALOG"`
// is a different, non-catalog schema that must not be blessed.
func builtinFuncName(funcname []*pgquery.Node) (string, bool) {
	parts := make([]string, 0, len(funcname))
	for _, fn := range funcname {
		s := fn.GetString_()
		if s == nil {
			return "", false
		}
		parts = append(parts, s.Sval)
	}
	switch len(parts) {
	case 1:
		return strings.ToLower(parts[0]), true
	case 2:
		if parts[0] == "pg_catalog" {
			return strings.ToLower(parts[1]), true
		}
	}
	return "", false
}

// aggArg extracts the single bare-column argument of a plain call —
// no FILTER, no OVER, exactly one ColumnRef argument.
func aggArg(fc *pgquery.FuncCall) []string {
	if fc.AggFilter != nil || fc.Over != nil || fc.AggStar || len(fc.Args) != 1 {
		return nil
	}
	cr := fc.Args[0].GetColumnRef()
	if cr == nil {
		return nil
	}
	var fields []string
	for _, f := range cr.Fields {
		s := f.GetString_()
		if s == nil {
			return nil // a star or other non-ident field
		}
		fields = append(fields, s.Sval)
	}
	return fields
}

// totalNode reports a data-independent never-NULL expression. Column
// references are deliberately not total — their nullability is the
// analyzer's catalog problem, not a syntactic fact.
func totalNode(node *pgquery.Node) bool {
	switch {
	case node == nil:
		return false
	case node.GetAConst() != nil:
		return !node.GetAConst().Isnull
	case node.GetSubLink() != nil:
		return node.GetSubLink().SubLinkType == pgquery.SubLinkType_EXISTS_SUBLINK
	case node.GetNullTest() != nil, node.GetBooleanTest() != nil:
		return true // IS [NOT] NULL / IS [NOT] TRUE… always yield a boolean
	case node.GetSqlvalueFunction() != nil:
		// current_schema is NULL with an empty search_path; every
		// other value function is total.
		return node.GetSqlvalueFunction().Op != pgquery.SQLValueFunctionOp_SVFOP_CURRENT_SCHEMA
	case node.GetTypeCast() != nil:
		return totalNode(node.GetTypeCast().Arg)
	case node.GetCoalesceExpr() != nil:
		for _, arg := range node.GetCoalesceExpr().Args {
			if totalNode(arg) {
				return true
			}
		}
		return false
	}
	return false
}

func (t *tree) HasGroupBy() bool {
	sel := t.sel()
	return sel != nil && len(sel.GroupClause) > 0
}

// NotNullConjuncts finds depth-0 WHERE conjuncts of the exact form
// `col IS NOT NULL`.
func (t *tree) NotNullConjuncts() []dialect.ColRef {
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
	var nodes []*pgquery.Node
	flattenConjunctNodes(where, &nodes)
	var out []dialect.ColRef
	for _, c := range nodes {
		nt := c.GetNullTest()
		if nt == nil || nt.Nulltesttype != pgquery.NullTestType_IS_NOT_NULL {
			continue
		}
		cr := nt.Arg.GetColumnRef()
		if cr == nil {
			continue
		}
		ref := dialect.ColRef{Loc: int(nt.Location)}
		ok := true
		for _, f := range cr.Fields {
			s := f.GetString_()
			if s == nil {
				ok = false
				break
			}
			ref.Fields = append(ref.Fields, s.Sval)
		}
		if ok && len(ref.Fields) > 0 {
			out = append(out, ref)
		}
	}
	return out
}

func flattenConjunctNodes(node *pgquery.Node, out *[]*pgquery.Node) {
	if node == nil {
		return
	}
	if be := node.GetBoolExpr(); be != nil && be.Boolop == pgquery.BoolExprType_AND_EXPR {
		for _, arg := range be.Args {
			flattenConjunctNodes(arg, out)
		}
		return
	}
	*out = append(*out, node)
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

func (t *tree) HavingConjunctLocs() []int {
	n := t.stmt()
	if n == nil || n.GetSelectStmt() == nil {
		return nil
	}
	var locs []int
	flattenConjuncts(n.GetSelectStmt().HavingClause, &locs)
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

func (t *tree) HasFetchWithTies() bool {
	sel := t.sel()
	return sel != nil && sel.LimitOption == pgquery.LimitOption_LIMIT_OPTION_WITH_TIES
}

// DebugString aids test failures.
func (t *tree) DebugString() string {
	var b strings.Builder
	fmt.Fprintf(&b, "stmts=%d kind=%v rels=%v", t.StmtCount(), t.Kind(), t.Relations())
	return b.String()
}
