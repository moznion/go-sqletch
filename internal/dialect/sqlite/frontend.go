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
	r2b := runeToByteTable(sqlText)
	p := rsql.NewParser(strings.NewReader(sqlText))
	stmts, err := p.ParseStatements()
	if err != nil {
		return nil, toParseError(err, r2b)
	}
	return &tree{stmts: stmts, r2b: r2b}, nil
}

// runeToByteTable maps a rune index to its byte offset in s. rqlite/sql
// reports positions as rune counts (its scanner advances the offset once
// per ReadRune), but the dialect contract — dialect.RelRef.Loc and the
// other Loc fields are documented byte offsets — and every downstream
// fragment-range check (nullability skeleton/guard tests, R1/R3 spans,
// the policy weaver) work in bytes against ast.FragRange. Without this
// translation, a relation or conjunct that follows a multibyte rune is
// reported left of its true byte position, so a guarded predicate can
// read as skeleton text and silently narrow a nullable column. The
// table has one entry per rune plus a trailing len(s), so a
// one-past-the-end position maps to EOF.
func runeToByteTable(s string) []int {
	tbl := make([]int, 0, len(s)+1)
	for i := range s { // range over a string yields each rune's start byte
		tbl = append(tbl, i)
	}
	return append(tbl, len(s))
}

// runeToByte translates a single rqlite rune offset into a byte offset.
// The -1 "no position" sentinel passes through unchanged; out-of-range
// indices clamp to EOF (and a nil table degrades to identity so a
// directly-constructed tree cannot panic).
func runeToByte(r2b []int, off int) int {
	if off < 0 || len(r2b) == 0 {
		return off
	}
	if off >= len(r2b) {
		return r2b[len(r2b)-1]
	}
	return r2b[off]
}

func (t *tree) b(off int) int { return runeToByte(t.r2b, off) }

func toParseError(err error, r2b []int) error {
	if pe, ok := errors.AsType[*rsql.Error](err); ok {
		return &dialect.ParseError{Pos: runeToByte(r2b, pe.Pos.Offset), Msg: pe.Msg}
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
		sel.HavingExpr != nil || len(sel.OrderingTerms) != 0 || sel.LimitExpr != nil ||
		sel.Compound != nil {
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
		len(sel.OrderingTerms) != 0 || sel.LimitExpr != nil || sel.Distinct.IsValid() ||
		sel.Compound != nil {
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
	// r2b maps a rune index (as rqlite reports positions) to a byte
	// offset in the parsed SQL. Sub-facades share the parent's table
	// because rqlite offsets stay absolute into the original SQL.
	r2b []int
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

func (t *tree) HasConflictUpdate() bool {
	ins, ok := t.first().(*rsql.InsertStatement)
	if !ok {
		return false
	}
	// ON CONFLICT … DO UPDATE modifies rows on a conflict; DO NOTHING
	// (DoUpdate position invalid) does not.
	return ins.UpsertClause != nil && ins.UpsertClause.DoUpdate.IsValid()
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
		// UPDATE … FROM src (SQLite 3.33+): the FROM sources are
		// ordinary FROM relations the scoping/weaving logic must see,
		// exactly like a SELECT's FROM.
		if s.Source != nil {
			collectSource(s.Source, dialect.JoinBase, false, &out)
		}
	case *rsql.DeleteStatement:
		if s.Table != nil {
			out = append(out, relFromQTN(s.Table, dialect.JoinBase, false))
		}
	case *rsql.InsertStatement:
		if s.Table != nil {
			out = append(out, dialect.RelRef{
				Table: s.Table.Name, Schema: insertSchema(s), Loc: s.Table.NamePos.Offset, Join: dialect.JoinBase,
			})
		}
	}
	for i := range out {
		out[i].Loc = t.b(out[i].Loc)
	}
	return out
}

func relFromQTN(q *rsql.QualifiedTableName, join dialect.JoinType, nullable bool) dialect.RelRef {
	alias := ""
	if q.Alias != nil {
		alias = q.Alias.Name
	}
	schema := ""
	if q.Schema != nil {
		schema = q.Schema.Name
	}
	return dialect.RelRef{
		Alias: alias, Table: q.Name.Name, Schema: schema, Loc: q.Name.NamePos.Offset,
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
	case *rsql.QualifiedTableFunctionName:
		// A table-valued function (FROM json_each(…)) is an opaque
		// source, like a derived table: it is COUNTED so the weaver does
		// not undercount, but its own name is never a base table. A
		// designated table hiding inside a TVF argument surfaces through
		// DeepTables (which descends into the arguments) but not here, so
		// the weaver's Relations/DeepTables comparison flags it as hidden
		// and refuses it (SQLETCH125) rather than silently skipping it.
		alias := ""
		if v.Alias != nil {
			alias = v.Alias.Name
		}
		*out = append(*out, dialect.RelRef{Alias: alias, Loc: -1, Join: join, NullableSide: nullable})
	case *rsql.SelectStatement:
		*out = append(*out, dialect.RelRef{Loc: -1, Join: join, NullableSide: nullable})
	}
}

// subTree wraps a subquery (a derived-table body or a CTE body) in
// its own facade for recursive analysis. rqlite offsets stay absolute
// into the original sql, so the sub-facade shares the parent's
// rune→byte table and fragment-range checks keep working.
func subTree(sel *rsql.SelectStatement, r2b []int) *tree {
	return &tree{stmts: []rsql.Statement{sel}, r2b: r2b}
}

// HasSetOperation reports a compound select (UNION/INTERSECT/EXCEPT).
func (t *tree) HasSetOperation() bool {
	s := t.sel()
	return s != nil && s.Compound != nil
}

// HasUnresolvableProvenance: SQLite's column-origin attribution
// carries no database qualifier, so any schema-qualified reference
// anywhere in the statement (attached databases) can cross-resolve to
// a same-named main-database table.
func (t *tree) HasUnresolvableProvenance() bool {
	for _, tr := range t.DeepTables() {
		if tr.Schema != "" {
			return true
		}
	}
	return false
}

// DerivedRels collects FROM-reachable derived tables with
// sub-facades, tracking null-extension exactly like collectSource.
func (t *tree) DerivedRels() []dialect.SubRel {
	var out []dialect.SubRel
	switch s := t.first().(type) {
	case *rsql.SelectStatement:
		if s.Source != nil {
			collectDerivedSource(s.Source, false, t.r2b, &out)
		}
	case *rsql.UpdateStatement:
		// UPDATE … FROM sources can be derived tables too; expose them
		// so nullability recursion matches the SELECT path.
		if s.Source != nil {
			collectDerivedSource(s.Source, false, t.r2b, &out)
		}
	}
	return out
}

func collectDerivedSource(src rsql.Source, nullable bool, r2b []int, out *[]dialect.SubRel) {
	switch v := src.(type) {
	case *rsql.JoinClause:
		jt := joinType(v.Operator)
		rightNullable := nullable || jt == dialect.JoinLeft
		collectDerivedSource(v.X, nullable, r2b, out)
		collectDerivedSource(v.Y, rightNullable, r2b, out)
	case *rsql.ParenSource:
		if sub, ok := v.X.(*rsql.SelectStatement); ok {
			alias := ""
			if v.Alias != nil {
				alias = v.Alias.Name
			}
			*out = append(*out, dialect.SubRel{
				Alias: alias, NullableSide: nullable, Tree: subTree(sub, r2b),
			})
			return
		}
		collectDerivedSource(v.X, nullable, r2b, out)
	case *rsql.SelectStatement:
		*out = append(*out, dialect.SubRel{NullableSide: nullable, Tree: subTree(v, r2b)})
	}
}

// withClause returns the statement's top-level WITH clause. All four
// statement kinds carry one (rqlite's UpdateStatement/DeleteStatement/
// InsertStatement each have a WithClause, like SelectStatement), so a
// CTE on a DML statement is visible here rather than being silently
// dropped — the policy weaver compares CTEs()/DeepTables() against
// Relations() and must see every position a designated table can hide
// in (doc 14 §D6).
func (t *tree) withClause() *rsql.WithClause {
	switch s := t.first().(type) {
	case *rsql.SelectStatement:
		return s.WithClause
	case *rsql.UpdateStatement:
		return s.WithClause
	case *rsql.DeleteStatement:
		return s.WithClause
	case *rsql.InsertStatement:
		return s.WithClause
	}
	return nil
}

// CTEs returns the statement's WITH-list definitions. SQLite CTE
// bodies are always selects, so Tree is always non-nil.
func (t *tree) CTEs() []dialect.CTEDef {
	wc := t.withClause()
	if wc == nil {
		return nil
	}
	recursive := wc.Recursive.IsValid()
	var out []dialect.CTEDef
	for _, c := range wc.CTEs {
		def := dialect.CTEDef{Recursive: recursive}
		if c.TableName != nil {
			def.Name = c.TableName.Name
		}
		if c.Select != nil {
			def.Tree = subTree(c.Select, t.r2b)
		}
		out = append(out, def)
	}
	return out
}

// HasGroupingSets is always false: SQLite has no ROLLUP / CUBE /
// GROUPING SETS.
func (t *tree) HasGroupingSets() bool { return false }

// ---- deep table refs -------------------------------------------------------

// tableWalker hand-walks the same positions refWalker does, collecting
// every base-table name — subqueries and CTE bodies included, and
// CTE-name references with them (conservative, design 14 §11.1).
type tableWalker struct {
	out []dialect.TableRef
}

func (t *tree) DeepTables() []dialect.TableRef {
	w := &tableWalker{}
	switch s := t.first().(type) {
	case *rsql.SelectStatement:
		w.walkSelect(s)
	case *rsql.UpdateStatement:
		w.walkWith(s.WithClause)
		if s.Table != nil {
			w.out = append(w.out, dialect.TableRef{Name: s.Table.Name.Name, Schema: qtnSchema(s.Table), Loc: s.Table.Name.NamePos.Offset})
		}
		w.walkSource(s.Source) // UPDATE … FROM sources
		for _, a := range s.Assignments {
			w.walkExpr(a.Expr)
		}
		w.walkExpr(s.WhereExpr)
		w.walkReturning(s.ReturningClause)
	case *rsql.DeleteStatement:
		w.walkWith(s.WithClause)
		if s.Table != nil {
			w.out = append(w.out, dialect.TableRef{Name: s.Table.Name.Name, Schema: qtnSchema(s.Table), Loc: s.Table.Name.NamePos.Offset})
		}
		w.walkExpr(s.WhereExpr)
	case *rsql.InsertStatement:
		w.walkWith(s.WithClause)
		if s.Table != nil {
			w.out = append(w.out, dialect.TableRef{Name: s.Table.Name, Schema: insertSchema(s), Loc: s.Table.NamePos.Offset})
		}
		for _, vl := range s.ValueLists {
			for _, e := range vl.Exprs {
				w.walkExpr(e)
			}
		}
		w.walkSelect(s.Select)
		w.walkReturning(s.ReturningClause)
	}
	for i := range w.out {
		w.out[i].Loc = t.b(w.out[i].Loc)
	}
	return w.out
}

func (w *tableWalker) walkReturning(rc *rsql.ReturningClause) {
	if rc == nil {
		return
	}
	for _, c := range rc.Columns {
		if c != nil {
			w.walkExpr(c.Expr)
		}
	}
}

// walkWith walks every CTE body of a WITH clause (on any statement
// kind). CTE names shadowing a designated table are surfaced by the
// body's own table refs; the weaver treats such names conservatively.
func (w *tableWalker) walkWith(wc *rsql.WithClause) {
	if wc == nil {
		return
	}
	for _, cte := range wc.CTEs {
		w.walkSelect(cte.Select)
	}
}

func (w *tableWalker) walkSelect(s *rsql.SelectStatement) {
	if s == nil {
		return
	}
	w.walkWith(s.WithClause)
	for _, rc := range s.Columns {
		if rc != nil {
			w.walkExpr(rc.Expr)
		}
	}
	w.walkSource(s.Source)
	w.walkExpr(s.WhereExpr)
	for _, e := range s.GroupByExprs {
		w.walkExpr(e)
	}
	w.walkExpr(s.HavingExpr)
	for _, ot := range s.OrderingTerms {
		w.walkExpr(ot.X)
	}
	w.walkExpr(s.LimitExpr)
	w.walkExpr(s.OffsetExpr)
	for _, win := range s.Windows {
		if win != nil {
			w.walkWindowDef(win.Definition)
		}
	}
	if s.Compound != nil {
		w.walkSelect(s.Compound)
	}
}

// walkWindowDef descends the subquery-bearing expressions of a window
// definition (an inline OVER(...) or a named WINDOW). Only Partitions,
// OrderingTerms, and the frame bounds can hold expressions; the Base
// window name is a reference to another WINDOW definition, which is
// itself walked via SelectStatement.Windows.
func (w *tableWalker) walkWindowDef(d *rsql.WindowDefinition) {
	if d == nil {
		return
	}
	for _, p := range d.Partitions {
		w.walkExpr(p)
	}
	for _, ot := range d.OrderingTerms {
		if ot != nil {
			w.walkExpr(ot.X)
		}
	}
	if d.Frame != nil {
		w.walkExpr(d.Frame.X)
		w.walkExpr(d.Frame.Y)
	}
}

func (w *tableWalker) walkSource(src rsql.Source) {
	switch v := src.(type) {
	case nil:
	case *rsql.JoinClause:
		w.walkSource(v.X)
		w.walkSource(v.Y)
		if on, ok := v.Constraint.(*rsql.OnConstraint); ok {
			w.walkExpr(on.X)
		}
	case *rsql.QualifiedTableName:
		w.out = append(w.out, dialect.TableRef{Name: v.Name.Name, Schema: qtnSchema(v), Loc: v.Name.NamePos.Offset})
	case *rsql.ParenSource:
		if sub, ok := v.X.(*rsql.SelectStatement); ok {
			w.walkSelect(sub)
			return
		}
		w.walkSource(v.X)
	case *rsql.QualifiedTableFunctionName:
		// The TVF name is not a base table, but its arguments can hide a
		// designated table (a subquery argument, or a ref through an
		// outer table). Descend into the arguments so such a table is
		// counted in DeepTables — where, absent from Relations, it reads
		// as a hidden occurrence the weaver refuses (SQLETCH125).
		for _, a := range v.Args {
			w.walkExpr(a)
		}
	case *rsql.SelectStatement:
		w.walkSelect(v)
	}
}

func (w *tableWalker) walkExpr(e rsql.Expr) {
	switch v := e.(type) {
	case nil:
	case *rsql.BinaryExpr:
		w.walkExpr(v.X)
		// `expr IN table-name` reads a base table: rqlite shapes the RHS
		// of IN/NOT IN as a bare *Ident (`x IN t`) or a *QualifiedRef
		// (`x IN schema.t`, Table=schema/Column=table). Those forms are
		// table READS the policy weaver must see; without this, a
		// designated table appearing ONLY as an IN operand is missing
		// from DeepTables and silently reads unscoped (finding H5). The
		// `IN (subquery)` / `IN (expr-list)` / `IN tvf(args)` forms are
		// NOT new base-table refs and fall through to the ordinary walk.
		if v.Op == rsql.IN || v.Op == rsql.NOTIN {
			w.walkInRHS(v.Y)
		} else {
			w.walkExpr(v.Y)
		}
	case *rsql.UnaryExpr:
		w.walkExpr(v.X)
	case *rsql.ParenExpr:
		w.walkExpr(v.X)
	case *rsql.ExprList:
		for _, x := range v.Exprs {
			w.walkExpr(x)
		}
	case *rsql.Call:
		for _, a := range v.Args {
			w.walkExpr(a)
		}
		if v.Filter != nil {
			w.walkExpr(v.Filter.X)
		}
		// A window function's OVER(...) can carry subqueries in its
		// PARTITION BY / ORDER BY / frame-bound expressions; without
		// descending them a designated table reached only there is
		// invisible to DeepTables and reads unscoped (audit-13, the H5
		// blind spot relocated to the window clause).
		if v.Over != nil {
			w.walkWindowDef(v.Over.Definition)
		}
	case *rsql.CaseExpr:
		w.walkExpr(v.Operand)
		for _, b := range v.Blocks {
			w.walkExpr(b.Condition)
			w.walkExpr(b.Body)
		}
		w.walkExpr(v.ElseExpr)
	case *rsql.CastExpr:
		w.walkExpr(v.X)
	case *rsql.CollateExpr:
		w.walkExpr(v.X)
	case *rsql.Null:
		w.walkExpr(v.X)
	case *rsql.Range:
		w.walkExpr(v.X)
		w.walkExpr(v.Y)
	case *rsql.Exists:
		w.walkSelect(v.Select)
	case rsql.SelectExpr:
		w.walkSelect(v.SelectStatement)
	}
	// Idents, BindExpr, literals, Raise: no table references.
}

// walkInRHS records the base table an `expr IN table-name` operand reads.
// rqlite's parseInExpr shapes the RHS of IN/NOT IN as: a bare *Ident
// (`x IN t`), a *QualifiedRef with Table=schema and Column=table
// (`x IN schema.t`), an *ExprList (`x IN (…)` — list OR subquery), or a
// *Call (`x IN tvf(args)` — a table-valued function, not a base table).
// Only the Ident/QualifiedRef table-name forms are base-table reads; the
// rest are walked as ordinary expressions (the subquery's own FROM
// tables and the TVF's arguments still surface through walkExpr).
func (w *tableWalker) walkInRHS(y rsql.Expr) {
	switch r := y.(type) {
	case *rsql.Ident:
		w.out = append(w.out, dialect.TableRef{Name: r.Name, Loc: r.NamePos.Offset})
	case *rsql.QualifiedRef:
		// schema.table: Table holds the schema, Column the table name.
		if !r.Star.IsValid() && r.Table != nil && r.Column != nil {
			schema := r.Table.Name
			w.out = append(w.out, dialect.TableRef{
				Name: r.Column.Name, Schema: schema, Loc: r.Column.NamePos.Offset,
			})
			return
		}
		w.walkExpr(y)
	default:
		w.walkExpr(y)
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
		w.walkSelect(s, false, nil)
	case *rsql.UpdateStatement:
		w.walkWith(s.WithClause, nil)
		w.walkSource(s.Source, false, nil, nil) // UPDATE … FROM: resolve its columns
		for _, a := range s.Assignments {
			w.walkExpr(a.Expr, false, nil)
		}
		w.walkExpr(s.WhereExpr, false, nil)
		if s.ReturningClause != nil {
			for _, rc := range s.ReturningClause.Columns {
				w.walkResultColumn(rc, false, nil)
			}
		}
	case *rsql.DeleteStatement:
		w.walkWith(s.WithClause, nil)
		w.walkExpr(s.WhereExpr, false, nil)
	case *rsql.InsertStatement:
		w.walkWith(s.WithClause, nil)
		for _, vl := range s.ValueLists {
			for _, e := range vl.Exprs {
				w.walkExpr(e, false, nil)
			}
		}
		if s.Select != nil {
			w.walkSelect(s.Select, true, nil)
		}
		if s.ReturningClause != nil {
			for _, rc := range s.ReturningClause.Columns {
				w.walkResultColumn(rc, false, nil)
			}
		}
	}
	for i := range w.out {
		w.out[i].Loc = t.b(w.out[i].Loc)
	}
	return w.out
}

// The scope parameter threaded through the walk is the set of effective
// FROM names of the ENCLOSING subquery scopes (dialect.ColRef.ScopeAliases):
// a subquery select adds its own FROM names for its body but passes the
// unextended enclosing scope to non-lateral derived tables (they cannot
// see sibling FROM items) and to compound (set-operation) branches (each
// branch has its own FROM) — under-collecting rather than over-collecting.
// The top-level statement's own FROM is never added.

// walkWith walks every CTE body of a WITH clause; CTE bodies are their
// own scope, so refs inside them carry InSubquery. A CTE body does not
// see the defining select's FROM, so it is entered with the enclosing
// scope only.
func (w *refWalker) walkWith(wc *rsql.WithClause, enclosing []string) {
	if wc == nil {
		return
	}
	for _, cte := range wc.CTEs {
		w.walkSelect(cte.Select, true, enclosing)
	}
}

// walkSelect walks one select. enclosing is the scope from strictly
// outer subqueries; the select's own FROM names are added to the scope
// its own clauses see (its columns/WHERE/…) when it is a subquery.
func (w *refWalker) walkSelect(s *rsql.SelectStatement, inSub bool, enclosing []string) {
	if s == nil {
		return
	}
	scope := enclosing
	if inSub {
		var own []string
		sourceNames(s.Source, &own)
		if len(own) > 0 {
			scope = append(append([]string(nil), enclosing...), own...)
		}
	}
	w.walkWith(s.WithClause, enclosing)
	for _, rc := range s.Columns {
		w.walkResultColumn(rc, inSub, scope)
	}
	w.walkSource(s.Source, inSub, scope, enclosing)
	w.walkExpr(s.WhereExpr, inSub, scope)
	for _, e := range s.GroupByExprs {
		w.walkExpr(e, inSub, scope)
	}
	w.walkExpr(s.HavingExpr, inSub, scope)
	for _, ot := range s.OrderingTerms {
		w.walkExpr(ot.X, inSub, scope)
	}
	w.walkExpr(s.LimitExpr, inSub, scope)
	w.walkExpr(s.OffsetExpr, inSub, scope)
	for _, win := range s.Windows {
		if win != nil {
			w.walkWindowDef(win.Definition, inSub, scope)
		}
	}
	if s.Compound != nil {
		w.walkSelect(s.Compound, inSub, enclosing)
	}
}

// walkWindowDef mirrors tableWalker.walkWindowDef for column-ref
// collection: refs inside a window's PARTITION BY / ORDER BY / frame
// bounds must be resolved (R3) like any other expression.
func (w *refWalker) walkWindowDef(d *rsql.WindowDefinition, inSub bool, scope []string) {
	if d == nil {
		return
	}
	for _, p := range d.Partitions {
		w.walkExpr(p, inSub, scope)
	}
	for _, ot := range d.OrderingTerms {
		if ot != nil {
			w.walkExpr(ot.X, inSub, scope)
		}
	}
	if d.Frame != nil {
		w.walkExpr(d.Frame.X, inSub, scope)
		w.walkExpr(d.Frame.Y, inSub, scope)
	}
}

func (w *refWalker) walkResultColumn(rc *rsql.ResultColumn, inSub bool, scope []string) {
	if rc == nil {
		return
	}
	if rc.Star.IsValid() {
		w.out = append(w.out, dialect.ColRef{Star: true, Loc: rc.Star.Offset, InSubquery: inSub})
		return
	}
	w.walkExpr(rc.Expr, inSub, scope)
}

// walkSource walks a FROM source. ON-constraint expressions reference
// the join's tables (this select's own scope), while a nested derived
// table is entered with the enclosing scope only (non-lateral: it does
// not see its sibling FROM items).
func (w *refWalker) walkSource(src rsql.Source, inSub bool, scope, enclosing []string) {
	switch v := src.(type) {
	case nil:
	case *rsql.JoinClause:
		w.walkSource(v.X, inSub, scope, enclosing)
		w.walkSource(v.Y, inSub, scope, enclosing)
		if on, ok := v.Constraint.(*rsql.OnConstraint); ok {
			w.walkExpr(on.X, inSub, scope)
		}
	case *rsql.ParenSource:
		if sub, ok := v.X.(*rsql.SelectStatement); ok {
			w.walkSelect(sub, true, enclosing)
			return
		}
		w.walkSource(v.X, inSub, scope, enclosing)
	case *rsql.QualifiedTableFunctionName:
		// TVF arguments are ordinary expressions in the current FROM
		// scope (they may reference preceding FROM items, lateral-style),
		// so their column refs must be resolved like any other.
		for _, a := range v.Args {
			w.walkExpr(a, inSub, scope)
		}
	case *rsql.SelectStatement:
		w.walkSelect(v, true, enclosing)
	}
}

func (w *refWalker) walkExpr(e rsql.Expr, inSub bool, scope []string) {
	switch v := e.(type) {
	case nil:
	case *rsql.Ident:
		w.out = append(w.out, dialect.ColRef{
			Fields: []string{v.Name}, Loc: v.NamePos.Offset, InSubquery: inSub,
		})
	case *rsql.QualifiedRef:
		cr := dialect.ColRef{Loc: exprPos(v), InSubquery: inSub}
		if len(scope) > 0 {
			cr.ScopeAliases = append([]string(nil), scope...)
		}
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
		w.walkExpr(v.X, inSub, scope)
		w.walkExpr(v.Y, inSub, scope)
	case *rsql.UnaryExpr:
		w.walkExpr(v.X, inSub, scope)
	case *rsql.ParenExpr:
		w.walkExpr(v.X, inSub, scope)
	case *rsql.ExprList:
		for _, x := range v.Exprs {
			w.walkExpr(x, inSub, scope)
		}
	case *rsql.Call:
		for _, a := range v.Args {
			w.walkExpr(a, inSub, scope)
		}
		if v.Filter != nil {
			w.walkExpr(v.Filter.X, inSub, scope)
		}
		if v.Over != nil {
			w.walkWindowDef(v.Over.Definition, inSub, scope)
		}
	case *rsql.CaseExpr:
		w.walkExpr(v.Operand, inSub, scope)
		for _, b := range v.Blocks {
			w.walkExpr(b.Condition, inSub, scope)
			w.walkExpr(b.Body, inSub, scope)
		}
		w.walkExpr(v.ElseExpr, inSub, scope)
	case *rsql.CastExpr:
		w.walkExpr(v.X, inSub, scope)
	case *rsql.CollateExpr:
		w.walkExpr(v.X, inSub, scope)
	case *rsql.Null:
		w.walkExpr(v.X, inSub, scope)
	case *rsql.Range:
		w.walkExpr(v.X, inSub, scope)
		w.walkExpr(v.Y, inSub, scope)
	case *rsql.Exists:
		w.walkSelect(v.Select, true, scope)
	case rsql.SelectExpr:
		w.walkSelect(v.SelectStatement, true, scope)
	}
	// BindExpr, literals, Raise: no column references.
}

// sourceNames collects the effective FROM names (alias else table) of a
// single select's own FROM source. Nested subquery/derived-table bodies
// are not descended (their names belong to deeper scopes).
func sourceNames(src rsql.Source, out *[]string) {
	switch v := src.(type) {
	case *rsql.JoinClause:
		sourceNames(v.X, out)
		sourceNames(v.Y, out)
	case *rsql.QualifiedTableName:
		if v.Alias != nil {
			*out = append(*out, v.Alias.Name)
		} else if v.Name != nil {
			*out = append(*out, v.Name.Name)
		}
	case *rsql.ParenSource:
		if _, ok := v.X.(*rsql.SelectStatement); ok {
			if v.Alias != nil {
				*out = append(*out, v.Alias.Name)
			}
			return
		}
		sourceNames(v.X, out) // parenthesized join
	case *rsql.QualifiedTableFunctionName:
		// A TVF introduces an effective FROM name (alias else function
		// name) that a qualified ref can target, so it belongs in the
		// scope set exactly like a table's own name.
		if v.Alias != nil {
			*out = append(*out, v.Alias.Name)
		} else if v.Name != nil {
			*out = append(*out, v.Name.Name)
		}
	}
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
				// A plain aggregate over one bare column: no FILTER,
				// no OVER, not the star form.
				if e.Filter == nil && e.Over == nil && !e.Star.IsValid() && len(e.Args) == 1 {
					switch a := e.Args[0].(type) {
					case *rsql.Ident:
						item.AggArg = []string{a.Name}
					case *rsql.QualifiedRef:
						if !a.Star.IsValid() && a.Table != nil && a.Column != nil {
							item.AggArg = []string{a.Table.Name, a.Column.Name}
						}
					}
				}
			}
			item.Total = totalExpr(rc.Expr)
		}
		out = append(out, item)
	}
	for i := range out {
		out[i].Loc = t.b(out[i].Loc)
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
	return t.bLocs(locs)
}

func (t *tree) HavingConjunctLocs() []int {
	sel, ok := t.first().(*rsql.SelectStatement)
	if !ok {
		return nil
	}
	var locs []int
	flattenConjuncts(sel.HavingExpr, &locs)
	return t.bLocs(locs)
}

// bLocs translates a slice of rqlite rune offsets to byte offsets
// in place and returns it.
func (t *tree) bLocs(locs []int) []int {
	for i := range locs {
		locs[i] = t.b(locs[i])
	}
	return locs
}

func (t *tree) HasGroupBy() bool {
	s := t.sel()
	return s != nil && len(s.GroupByExprs) > 0
}

// NotNullConjuncts finds depth-0 WHERE conjuncts of the exact form
// `col IS NOT NULL` (or SQLite's `col NOTNULL`).
func (t *tree) NotNullConjuncts() []dialect.ColRef {
	var where rsql.Expr
	switch s := t.first().(type) {
	case *rsql.SelectStatement:
		where = s.WhereExpr
	case *rsql.UpdateStatement:
		where = s.WhereExpr
	case *rsql.DeleteStatement:
		where = s.WhereExpr
	}
	var nodes []rsql.Expr
	flattenConjunctNodes(where, &nodes)
	var out []dialect.ColRef
	for _, c := range nodes {
		nt, ok := c.(*rsql.Null)
		if !ok || nt.Op != rsql.NOTNULL {
			continue
		}
		ref := dialect.ColRef{Loc: exprPos(c)}
		switch x := nt.X.(type) {
		case *rsql.Ident:
			ref.Fields = []string{x.Name}
		case *rsql.QualifiedRef:
			if x.Star.IsValid() || x.Table == nil || x.Column == nil {
				continue
			}
			ref.Fields = []string{x.Table.Name, x.Column.Name}
		default:
			continue
		}
		ref.Loc = t.b(ref.Loc)
		out = append(out, ref)
	}
	return out
}

func flattenConjunctNodes(e rsql.Expr, out *[]rsql.Expr) {
	switch v := e.(type) {
	case nil:
		return
	case *rsql.ParenExpr:
		flattenConjunctNodes(v.X, out)
	case *rsql.BinaryExpr:
		if v.Op == rsql.AND {
			flattenConjunctNodes(v.X, out)
			flattenConjunctNodes(v.Y, out)
			return
		}
		*out = append(*out, v)
	default:
		*out = append(*out, e)
	}
}

// totalExpr reports a data-independent never-NULL expression (see
// dialect.TargetItem.Total).
func totalExpr(e rsql.Expr) bool {
	switch v := e.(type) {
	case *rsql.StringLit, *rsql.NumberLit, *rsql.BoolLit, *rsql.BlobLit:
		return true
	case *rsql.Exists:
		return true
	case *rsql.Null: // IS NULL / NOTNULL tests yield 0/1
		return true
	case *rsql.ParenExpr:
		return totalExpr(v.X)
	case *rsql.CastExpr:
		// SQLite CAST coerces rather than fails; NULL only from a
		// NULL operand.
		return totalExpr(v.X)
	case *rsql.Call:
		if v.Name != nil && strings.EqualFold(v.Name.Name, "coalesce") {
			for _, arg := range v.Args {
				if totalExpr(arg) {
					return true
				}
			}
		}
		return false
	}
	return false
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
	return t.bLocs(locs)
}

// HasDistinctOn: SQLite has no DISTINCT ON.
func (t *tree) HasDistinctOn() bool { return false }

// HasLockingClause: SQLite has no FOR UPDATE.
func (t *tree) HasLockingClause() bool { return false }

// HasFetchWithTies: SQLite has no FETCH FIRST … WITH TIES.
func (t *tree) HasFetchWithTies() bool { return false }

func qtnSchema(q *rsql.QualifiedTableName) string {
	if q.Schema != nil {
		return q.Schema.Name
	}
	return ""
}

// insertSchema returns the INSERT target's database qualifier (rqlite's
// InsertStatement carries Schema separately from the QualifiedTableName
// the UPDATE/DELETE targets use). Dropping it would make a db-qualified
// write (INSERT INTO temp.t … RETURNING) look unqualified, so
// HasUnresolvableProvenance would not fire and the RETURNING columns
// would narrow through a same-named main-database table (design 05 §2b).
func insertSchema(s *rsql.InsertStatement) string {
	if s.Schema != nil {
		return s.Schema.Name
	}
	return ""
}
