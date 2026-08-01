package postgres

import (
	"errors"
	"fmt"
	"strings"

	pgquery "github.com/pganalyze/pg_query_go/v6"
	pgparser "github.com/pganalyze/pg_query_go/v6/parser"

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
			collectFromItem(item, dialect.JoinBase, &out)
		}
	case n.GetUpdateStmt() != nil:
		u := n.GetUpdateStmt()
		if u.Relation != nil {
			out = append(out, relFromRangeVar(u.Relation, dialect.JoinBase))
		}
		for _, item := range u.FromClause {
			collectFromItem(item, dialect.JoinBase, &out)
		}
	case n.GetDeleteStmt() != nil:
		d := n.GetDeleteStmt()
		if d.Relation != nil {
			out = append(out, relFromRangeVar(d.Relation, dialect.JoinBase))
		}
		for _, item := range d.UsingClause {
			collectFromItem(item, dialect.JoinBase, &out)
		}
	case n.GetInsertStmt() != nil:
		i := n.GetInsertStmt()
		if i.Relation != nil {
			out = append(out, relFromRangeVar(i.Relation, dialect.JoinBase))
		}
	}
	return out
}

func relFromRangeVar(rv *pgquery.RangeVar, join dialect.JoinType) dialect.RelRef {
	alias := ""
	if rv.Alias != nil {
		alias = rv.Alias.Aliasname
	}
	return dialect.RelRef{Alias: alias, Table: rv.Relname, Loc: int(rv.Location), Join: join}
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

func collectFromItem(node *pgquery.Node, join dialect.JoinType, out *[]dialect.RelRef) {
	switch {
	case node.GetRangeVar() != nil:
		*out = append(*out, relFromRangeVar(node.GetRangeVar(), join))
	case node.GetJoinExpr() != nil:
		je := node.GetJoinExpr()
		collectFromItem(je.Larg, join, out)
		collectFromItem(je.Rarg, mapJoinType(je.Jointype, je.IsNatural, je.Quals), out)
	case node.GetRangeSubselect() != nil:
		rs := node.GetRangeSubselect()
		alias := ""
		if rs.Alias != nil {
			alias = rs.Alias.Aliasname
		}
		*out = append(*out, dialect.RelRef{Alias: alias, Loc: -1, Join: join})
	case node.GetRangeFunction() != nil:
		rf := node.GetRangeFunction()
		alias := ""
		if rf.Alias != nil {
			alias = rf.Alias.Aliasname
		}
		*out = append(*out, dialect.RelRef{Alias: alias, Loc: -1, Join: join})
	}
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
