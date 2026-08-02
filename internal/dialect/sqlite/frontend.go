package sqlite

import (
	"errors"
	"strings"

	rsql "github.com/rqlite/sql"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// Frontend is the rqlite/sql-backed grammar frontend for SQLite: a
// pure-Go parser with byte offsets on every node (including relation
// names, unlike the TiDB AST). Known parser gaps versus the current
// SQLite grammar — RIGHT/FULL JOIN (3.39+) — surface as parse
// diagnostics; the real-engine oracle backstops everything the
// grammar accepts.
type Frontend struct{}

var _ dialect.Frontend = Frontend{}

func (Frontend) Parse(sqlText string) (dialect.Tree, error) {
	p := rsql.NewParser(strings.NewReader(sqlText))
	stmts, err := p.ParseStatements()
	if err != nil {
		return nil, toParseError(err)
	}
	return &tree{stmts: stmts}, nil
}

func toParseError(err error) error {
	if pe, ok := errors.AsType[*rsql.Error](err); ok {
		return &dialect.ParseError{Pos: pe.Pos.Offset, Msg: pe.Msg}
	}
	return &dialect.ParseError{Pos: 0, Msg: err.Error()}
}

// ---- Probes ----------------------------------------------------------------

func (f Frontend) probeSelect(sqlText string) (*rsql.SelectStatement, error) {
	t, err := f.Parse(sqlText)
	if err != nil {
		return nil, err
	}
	tr := t.(*tree)
	if len(tr.stmts) != 1 {
		return nil, &dialect.ParseError{Pos: 0, Msg: "fragment splits the probe statement"}
	}
	sel, ok := tr.stmts[0].(*rsql.SelectStatement)
	if !ok {
		return nil, &dialect.ParseError{Pos: 0, Msg: "fragment changes the probe statement kind"}
	}
	return sel, nil
}

// ProbeExpr checks that expr forms exactly one boolean-position
// expression. Parenthesis balance is the rules' (lexer-level) concern.
func (f Frontend) ProbeExpr(expr string) error {
	sel, err := f.probeSelect("SELECT 1 WHERE (" + expr + "\n)")
	if err != nil {
		return err
	}
	if sel.WhereExpr == nil || sel.Source != nil || len(sel.GroupByExprs) != 0 ||
		sel.HavingExpr != nil || len(sel.OrderingTerms) != 0 || sel.LimitExpr != nil ||
		sel.Compound != nil {
		return &dialect.ParseError{Pos: 0, Msg: "fragment is not a single predicate expression"}
	}
	return nil
}

// ProbeJoinItem checks that item is a join item attachable to a
// preceding FROM entry.
func (f Frontend) ProbeJoinItem(item string) error {
	sel, err := f.probeSelect("SELECT 1 FROM sqletch_probe_t " + item)
	if err != nil {
		return err
	}
	bad := &dialect.ParseError{Pos: 0, Msg: "fragment is not a single join item"}
	if sel.Source == nil || sel.WhereExpr != nil || len(sel.GroupByExprs) != 0 ||
		sel.HavingExpr != nil || len(sel.OrderingTerms) != 0 || sel.LimitExpr != nil {
		return bad
	}
	var rels []dialect.RelRef
	collectSource(sel.Source, dialect.JoinBase, false, &rels)
	if len(rels) != 2 {
		return bad
	}
	if rels[1].Join == dialect.JoinBase {
		return &dialect.ParseError{Pos: 0, Msg: "fragment does not join onto the preceding FROM entry"}
	}
	return nil
}

// ProbeOrderBy checks that clause is exactly one statement-level
// ORDER BY clause and nothing more.
func (f Frontend) ProbeOrderBy(clause string) error {
	sel, err := f.probeSelect("SELECT 1 " + clause)
	if err != nil {
		return err
	}
	if len(sel.OrderingTerms) == 0 ||
		sel.WhereExpr != nil || sel.Source != nil || len(sel.GroupByExprs) != 0 ||
		sel.HavingExpr != nil || sel.LimitExpr != nil || sel.Distinct.IsValid() {
		return &dialect.ParseError{Pos: 0, Msg: "fragment is not a bare ORDER BY clause"}
	}
	return nil
}

// ProbeOrderByKey checks that expr is exactly one sort key.
func (f Frontend) ProbeOrderByKey(expr string) error {
	sel, err := f.probeSelect("SELECT 1 ORDER BY " + expr + "\n")
	if err != nil {
		return err
	}
	if len(sel.OrderingTerms) != 1 ||
		sel.WhereExpr != nil || sel.Source != nil || len(sel.GroupByExprs) != 0 ||
		sel.LimitExpr != nil {
		return &dialect.ParseError{Pos: 0, Msg: "fragment is not a single sort key"}
	}
	return nil
}

// ProbeGroupBy checks that clause is exactly one statement-level
// GROUP BY clause and nothing more.
func (f Frontend) ProbeGroupBy(clause string) error {
	sel, err := f.probeSelect("SELECT 1 " + clause)
	if err != nil {
		return err
	}
	if len(sel.GroupByExprs) == 0 ||
		sel.WhereExpr != nil || sel.Source != nil || sel.HavingExpr != nil ||
		len(sel.OrderingTerms) != 0 || sel.LimitExpr != nil || sel.Distinct.IsValid() {
		return &dialect.ParseError{Pos: 0, Msg: "fragment is not a bare GROUP BY clause"}
	}
	return nil
}

// ProbeSetItem checks that item is exactly one UPDATE SET assignment.
func (f Frontend) ProbeSetItem(item string) error {
	t, err := f.Parse("UPDATE sqletch_probe_t SET " + item)
	if err != nil {
		return err
	}
	tr := t.(*tree)
	bad := &dialect.ParseError{Pos: 0, Msg: "fragment is not a single SET assignment"}
	if len(tr.stmts) != 1 {
		return bad
	}
	up, ok := tr.stmts[0].(*rsql.UpdateStatement)
	if !ok || len(up.Assignments) != 1 || up.WhereExpr != nil || up.ReturningClause != nil {
		return bad
	}
	return nil
}

// ProbeInsertValue checks that expr is exactly one VALUES row item.
// (SQLite has no per-item DEFAULT keyword; it fails to parse, which is
// the correct dialect answer.)
func (f Frontend) ProbeInsertValue(expr string) error {
	t, err := f.Parse("INSERT INTO sqletch_probe_t (c) VALUES (" + expr + "\n)")
	if err != nil {
		return err
	}
	tr := t.(*tree)
	bad := &dialect.ParseError{Pos: 0, Msg: "fragment is not a single VALUES item"}
	if len(tr.stmts) != 1 {
		return bad
	}
	ins, ok := tr.stmts[0].(*rsql.InsertStatement)
	if !ok || ins.Select != nil || len(ins.ValueLists) != 1 || len(ins.ValueLists[0].Exprs) != 1 {
		return bad
	}
	return nil
}

// ---- Tree facade -----------------------------------------------------------

type tree struct {
	stmts []rsql.Statement
}

func (t *tree) StmtCount() int { return len(t.stmts) }

func (t *tree) first() rsql.Statement {
	if len(t.stmts) == 0 {
		return nil
	}
	return t.stmts[0]
}

func (t *tree) Kind() dialect.StmtKind {
	switch t.first().(type) {
	case *rsql.SelectStatement:
		return dialect.StmtSelect
	case *rsql.UpdateStatement:
		return dialect.StmtUpdate
	case *rsql.InsertStatement:
		return dialect.StmtInsert
	case *rsql.DeleteStatement:
		return dialect.StmtDelete
	default:
		return dialect.StmtOther
	}
}

func (t *tree) sel() *rsql.SelectStatement {
	s, _ := t.first().(*rsql.SelectStatement)
	return s
}

func (t *tree) Relations() []dialect.RelRef {
	var out []dialect.RelRef
	switch s := t.first().(type) {
	case *rsql.SelectStatement:
		if s.Source != nil {
			collectSource(s.Source, dialect.JoinBase, false, &out)
		}
	case *rsql.UpdateStatement:
		if s.Table != nil {
			out = append(out, relFromQTN(s.Table, dialect.JoinBase, false))
		}
	case *rsql.DeleteStatement:
		if s.Table != nil {
			out = append(out, relFromQTN(s.Table, dialect.JoinBase, false))
		}
	case *rsql.InsertStatement:
		if s.Table != nil {
			out = append(out, dialect.RelRef{
				Table: s.Table.Name, Loc: s.Table.NamePos.Offset, Join: dialect.JoinBase,
			})
		}
	}
	return out
}

func relFromQTN(q *rsql.QualifiedTableName, join dialect.JoinType, nullable bool) dialect.RelRef {
	alias := ""
	if q.Alias != nil {
		alias = q.Alias.Name
	}
	return dialect.RelRef{
		Alias: alias, Table: q.Name.Name, Loc: q.Name.NamePos.Offset,
		Join: join, NullableSide: nullable,
	}
}

func joinType(op *rsql.JoinOperator) dialect.JoinType {
	switch {
	case op == nil:
		return dialect.JoinOther
	case op.Left.IsValid():
		return dialect.JoinLeft
	case op.Cross.IsValid(), op.Comma.IsValid():
		return dialect.JoinCross
	default:
		// Plain JOIN / INNER JOIN / NATURAL JOIN.
		return dialect.JoinInner
	}
}

// collectSource flattens the FROM tree. nullable tracks null-extended
// outer-join sides (the right of LEFT; the rqlite/sql grammar predates
// SQLite's RIGHT/FULL JOIN).
func collectSource(src rsql.Source, join dialect.JoinType, nullable bool, out *[]dialect.RelRef) {
	switch v := src.(type) {
	case *rsql.JoinClause:
		jt := joinType(v.Operator)
		rightNullable := nullable || jt == dialect.JoinLeft
		collectSource(v.X, join, nullable, out)
		collectSource(v.Y, jt, rightNullable, out)
	case *rsql.QualifiedTableName:
		*out = append(*out, relFromQTN(v, join, nullable))
	case *rsql.ParenSource:
		if sub, ok := v.X.(*rsql.SelectStatement); ok {
			_ = sub // derived table: opaque, like the other dialects
			alias := ""
			if v.Alias != nil {
				alias = v.Alias.Name
			}
			*out = append(*out, dialect.RelRef{Alias: alias, Loc: -1, Join: join, NullableSide: nullable})
			return
		}
		collectSource(v.X, join, nullable, out) // parenthesized join
	case *rsql.SelectStatement:
		*out = append(*out, dialect.RelRef{Loc: -1, Join: join, NullableSide: nullable})
	}
}

// ---- column refs -----------------------------------------------------------

// refWalker hand-walks expression positions only, so identifiers in
// name positions (table names, aliases, function names) never
// masquerade as column references.
type refWalker struct {
	out []dialect.ColRef
}

func (t *tree) ColumnRefs() []dialect.ColRef {
	w := &refWalker{}
	switch s := t.first().(type) {
	case *rsql.SelectStatement:
		w.walkSelect(s, false)
	case *rsql.UpdateStatement:
		for _, a := range s.Assignments {
			w.walkExpr(a.Expr, false)
		}
		w.walkExpr(s.WhereExpr, false)
		if s.ReturningClause != nil {
			for _, rc := range s.ReturningClause.Columns {
				w.walkResultColumn(rc, false)
			}
		}
	case *rsql.DeleteStatement:
		w.walkExpr(s.WhereExpr, false)
	case *rsql.InsertStatement:
		for _, vl := range s.ValueLists {
			for _, e := range vl.Exprs {
				w.walkExpr(e, false)
			}
		}
		if s.Select != nil {
			w.walkSelect(s.Select, true)
		}
		if s.ReturningClause != nil {
			for _, rc := range s.ReturningClause.Columns {
				w.walkResultColumn(rc, false)
			}
		}
	}
	return w.out
}

func (w *refWalker) walkSelect(s *rsql.SelectStatement, inSub bool) {
	if s == nil {
		return
	}
	if s.WithClause != nil {
		for _, cte := range s.WithClause.CTEs {
			w.walkSelect(cte.Select, true)
		}
	}
	for _, rc := range s.Columns {
		w.walkResultColumn(rc, inSub)
	}
	w.walkSource(s.Source, inSub)
	w.walkExpr(s.WhereExpr, inSub)
	for _, e := range s.GroupByExprs {
		w.walkExpr(e, inSub)
	}
	w.walkExpr(s.HavingExpr, inSub)
	for _, ot := range s.OrderingTerms {
		w.walkExpr(ot.X, inSub)
	}
	w.walkExpr(s.LimitExpr, inSub)
	w.walkExpr(s.OffsetExpr, inSub)
	if s.Compound != nil {
		w.walkSelect(s.Compound, inSub)
	}
}

func (w *refWalker) walkResultColumn(rc *rsql.ResultColumn, inSub bool) {
	if rc == nil {
		return
	}
	if rc.Star.IsValid() {
		w.out = append(w.out, dialect.ColRef{Star: true, Loc: rc.Star.Offset, InSubquery: inSub})
		return
	}
	w.walkExpr(rc.Expr, inSub)
}

func (w *refWalker) walkSource(src rsql.Source, inSub bool) {
	switch v := src.(type) {
	case nil:
	case *rsql.JoinClause:
		w.walkSource(v.X, inSub)
		w.walkSource(v.Y, inSub)
		if on, ok := v.Constraint.(*rsql.OnConstraint); ok {
			w.walkExpr(on.X, inSub)
		}
	case *rsql.ParenSource:
		if sub, ok := v.X.(*rsql.SelectStatement); ok {
			w.walkSelect(sub, true)
			return
		}
		w.walkSource(v.X, inSub)
	case *rsql.SelectStatement:
		w.walkSelect(v, true)
	}
}

func (w *refWalker) walkExpr(e rsql.Expr, inSub bool) {
	switch v := e.(type) {
	case nil:
	case *rsql.Ident:
		w.out = append(w.out, dialect.ColRef{
			Fields: []string{v.Name}, Loc: v.NamePos.Offset, InSubquery: inSub,
		})
	case *rsql.QualifiedRef:
		cr := dialect.ColRef{Loc: exprPos(v), InSubquery: inSub}
		if v.Schema != nil {
			cr.Fields = append(cr.Fields, v.Schema.Name)
		}
		if v.Table != nil {
			cr.Fields = append(cr.Fields, v.Table.Name)
		}
		if v.Star.IsValid() {
			cr.Star = true
		} else if v.Column != nil {
			cr.Fields = append(cr.Fields, v.Column.Name)
		}
		w.out = append(w.out, cr)
	case *rsql.BinaryExpr:
		w.walkExpr(v.X, inSub)
		w.walkExpr(v.Y, inSub)
	case *rsql.UnaryExpr:
		w.walkExpr(v.X, inSub)
	case *rsql.ParenExpr:
		w.walkExpr(v.X, inSub)
	case *rsql.ExprList:
		for _, x := range v.Exprs {
			w.walkExpr(x, inSub)
		}
	case *rsql.Call:
		for _, a := range v.Args {
			w.walkExpr(a, inSub)
		}
		if v.Filter != nil {
			w.walkExpr(v.Filter.X, inSub)
		}
	case *rsql.CaseExpr:
		w.walkExpr(v.Operand, inSub)
		for _, b := range v.Blocks {
			w.walkExpr(b.Condition, inSub)
			w.walkExpr(b.Body, inSub)
		}
		w.walkExpr(v.ElseExpr, inSub)
	case *rsql.CastExpr:
		w.walkExpr(v.X, inSub)
	case *rsql.CollateExpr:
		w.walkExpr(v.X, inSub)
	case *rsql.Null:
		w.walkExpr(v.X, inSub)
	case *rsql.Range:
		w.walkExpr(v.X, inSub)
		w.walkExpr(v.Y, inSub)
	case *rsql.Exists:
		w.walkSelect(v.Select, true)
	case rsql.SelectExpr:
		w.walkSelect(v.SelectStatement, true)
	}
	// BindExpr, literals, Raise: no column references.
}

func exprPos(e rsql.Expr) int {
	switch v := e.(type) {
	case *rsql.Ident:
		return v.NamePos.Offset
	case *rsql.QualifiedRef:
		if v.Schema != nil {
			return v.Schema.NamePos.Offset
		}
		if v.Table != nil {
			return v.Table.NamePos.Offset
		}
		return -1
	case *rsql.BinaryExpr:
		return exprPos(v.X)
	case *rsql.UnaryExpr:
		return v.OpPos.Offset
	case *rsql.ParenExpr:
		return v.Lparen.Offset
	case *rsql.Call:
		return v.Name.NamePos.Offset
	case *rsql.CaseExpr:
		return v.Case.Offset
	case *rsql.CastExpr:
		return v.Cast.Offset
	case *rsql.CollateExpr:
		return exprPos(v.X)
	case *rsql.Null:
		return exprPos(v.X)
	case *rsql.Range:
		return exprPos(v.X)
	case *rsql.Exists:
		if v.Not.IsValid() {
			return v.Not.Offset
		}
		return v.Exists.Offset
	case *rsql.BindExpr:
		return v.NamePos.Offset
	case *rsql.StringLit:
		return v.ValuePos.Offset
	case *rsql.NumberLit:
		return v.ValuePos.Offset
	case *rsql.BoolLit:
		return v.ValuePos.Offset
	case *rsql.NullLit:
		return v.Pos.Offset
	case *rsql.BlobLit:
		return v.ValuePos.Offset
	case *rsql.TimestampLit:
		return v.ValuePos.Offset
	default:
		return -1
	}
}

func (t *tree) TargetItems() []dialect.TargetItem {
	sel := t.sel()
	if sel == nil {
		return nil
	}
	var out []dialect.TargetItem
	for _, rc := range sel.Columns {
		item := dialect.TargetItem{}
		if rc.Alias != nil {
			item.Name = rc.Alias.Name
		}
		switch {
		case rc.Star.IsValid():
			item.Star = true
			item.Loc = rc.Star.Offset
		default:
			item.Loc = exprPos(rc.Expr)
			switch e := rc.Expr.(type) {
			case *rsql.QualifiedRef:
				if e.Star.IsValid() {
					item.Star = true
					if e.Table != nil {
						item.Qualifier = e.Table.Name
					}
				}
			case *rsql.Call:
				item.FuncName = strings.ToLower(e.Name.Name)
			}
		}
		out = append(out, item)
	}
	return out
}

func (t *tree) TopConjunctLocs() []int {
	var where rsql.Expr
	switch s := t.first().(type) {
	case *rsql.SelectStatement:
		where = s.WhereExpr
	case *rsql.UpdateStatement:
		where = s.WhereExpr
	case *rsql.DeleteStatement:
		where = s.WhereExpr
	}
	var locs []int
	flattenConjuncts(where, &locs)
	return locs
}

func (t *tree) HavingConjunctLocs() []int {
	sel, ok := t.first().(*rsql.SelectStatement)
	if !ok {
		return nil
	}
	var locs []int
	flattenConjuncts(sel.HavingExpr, &locs)
	return locs
}

// flattenConjuncts mirrors the other facades: parens are transparent
// and nested ANDs flatten.
func flattenConjuncts(e rsql.Expr, out *[]int) {
	switch v := e.(type) {
	case nil:
		return
	case *rsql.ParenExpr:
		flattenConjuncts(v.X, out)
	case *rsql.BinaryExpr:
		if v.Op == rsql.AND {
			flattenConjuncts(v.X, out)
			flattenConjuncts(v.Y, out)
			return
		}
		*out = append(*out, exprPos(v))
	default:
		*out = append(*out, exprPos(e))
	}
}

func (t *tree) OrderByLocs() []int {
	sel := t.sel()
	if sel == nil {
		return nil
	}
	var locs []int
	for _, ot := range sel.OrderingTerms {
		e := ot.X
		if p, ok := e.(*rsql.ParenExpr); ok {
			e = p.X
		}
		locs = append(locs, exprPos(e))
	}
	return locs
}

// HasDistinctOn: SQLite has no DISTINCT ON.
func (t *tree) HasDistinctOn() bool { return false }

// HasLockingClause: SQLite has no FOR UPDATE.
func (t *tree) HasLockingClause() bool { return false }

// HasFetchWithTies: SQLite has no FETCH FIRST … WITH TIES.
func (t *tree) HasFetchWithTies() bool { return false }
