package mysql

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/opcode"

	// The parser requires a value-expression driver; the test_driver is
	// the standalone one (the same choice sqlc makes). sqletch only
	// parses — it never evaluates expressions — so the driver's
	// restricted type support is irrelevant.
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// Frontend is the TiDB-parser-backed grammar frontend for MySQL.
//
// Unlike pg_query, the TiDB AST carries byte offsets on expression
// nodes but NOT on relation nodes (TableName/TableSource). Relation
// locations are recovered lexically: in MySQL a relation name in FROM
// position is always preceded by FROM/JOIN/','/'('/'.'/INTO/UPDATE,
// so a monotonic token scan that skips subqueries pins each relation
// to its name token. See locateRelations.
type Frontend struct{}

var _ dialect.Frontend = Frontend{}

func (Frontend) Parse(sql string) (dialect.Tree, error) {
	stmts, _, err := parser.New().ParseSQL(sql)
	if err != nil {
		return nil, toParseError(sql, err)
	}
	return &tree{sql: sql, stmts: stmts}, nil
}

var lineColRe = regexp.MustCompile(`line (\d+) column (\d+)`)

func toParseError(sql string, err error) error {
	msg := err.Error()
	pos := 0
	if m := lineColRe.FindStringSubmatch(msg); m != nil {
		line, _ := strconv.Atoi(m[1])
		col, _ := strconv.Atoi(m[2])
		pos = lineColOffset(sql, line, col)
	}
	return &dialect.ParseError{Pos: pos, Msg: msg}
}

// lineColOffset converts a 1-based line/column pair to a byte offset,
// clamped to the statement.
func lineColOffset(sql string, line, col int) int {
	off := 0
	for l := 1; l < line; l++ {
		nl := strings.IndexByte(sql[off:], '\n')
		if nl < 0 {
			break
		}
		off += nl + 1
	}
	off += col - 1
	if off < 0 {
		return 0
	}
	if off >= len(sql) {
		return len(sql) - 1
	}
	return off
}

// ---- Probes ----------------------------------------------------------------

func (f Frontend) probeSelect(sql string) (*ast.SelectStmt, error) {
	t, err := f.Parse(sql)
	if err != nil {
		return nil, err
	}
	tr := t.(*tree)
	if len(tr.stmts) != 1 {
		return nil, &dialect.ParseError{Pos: 0, Msg: "fragment splits the probe statement"}
	}
	sel, ok := tr.stmts[0].(*ast.SelectStmt)
	if !ok {
		return nil, &dialect.ParseError{Pos: 0, Msg: "fragment changes the probe statement kind"}
	}
	return sel, nil
}

// ProbeExpr checks that expr forms exactly one boolean-position
// expression: it must parse when parenthesized as a WHERE condition
// and contribute nothing beyond it. Parenthesis balance is the rules'
// (lexer-level) concern.
func (f Frontend) ProbeExpr(expr string) error {
	sel, err := f.probeSelect("SELECT 1 WHERE (" + expr + "\n)")
	if err != nil {
		return err
	}
	if sel.Where == nil || sel.From != nil || sel.GroupBy != nil || sel.Having != nil ||
		sel.OrderBy != nil || sel.Limit != nil || hasLock(sel) {
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
	if sel.From == nil || sel.Where != nil || sel.GroupBy != nil || sel.Having != nil ||
		sel.OrderBy != nil || sel.Limit != nil || hasLock(sel) {
		return bad
	}
	var rels []dialect.RelRef
	collectJoin(sel.From.TableRefs, dialect.JoinBase, false, &rels)
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
	if sel.OrderBy == nil || len(sel.OrderBy.Items) == 0 ||
		sel.Where != nil || sel.From != nil || sel.GroupBy != nil || sel.Having != nil ||
		sel.Limit != nil || hasLock(sel) || sel.Distinct {
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
	if sel.OrderBy == nil || len(sel.OrderBy.Items) != 1 ||
		sel.Where != nil || sel.From != nil || sel.GroupBy != nil ||
		sel.Limit != nil || hasLock(sel) {
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
	if sel.GroupBy == nil || len(sel.GroupBy.Items) == 0 ||
		sel.Where != nil || sel.From != nil || sel.Having != nil ||
		sel.OrderBy != nil || sel.Limit != nil || hasLock(sel) || sel.Distinct {
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
	up, ok := tr.stmts[0].(*ast.UpdateStmt)
	if !ok || len(up.List) != 1 || up.Where != nil || up.Order != nil || up.Limit != nil {
		return bad
	}
	return nil
}

// ProbeInsertValue checks that expr is exactly one VALUES row item
// (including the DEFAULT keyword, which is only legal there).
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
	ins, ok := tr.stmts[0].(*ast.InsertStmt)
	if !ok || ins.Select != nil || ins.OnDuplicate != nil ||
		len(ins.Lists) != 1 || len(ins.Lists[0]) != 1 {
		return bad
	}
	return nil
}

func hasLock(sel *ast.SelectStmt) bool {
	return sel.LockInfo != nil && sel.LockInfo.LockType != ast.SelectLockNone
}

// ---- Tree facade -----------------------------------------------------------

type tree struct {
	sql   string
	stmts []ast.StmtNode
}

func (t *tree) StmtCount() int { return len(t.stmts) }

func (t *tree) first() ast.StmtNode {
	if len(t.stmts) == 0 {
		return nil
	}
	return t.stmts[0]
}

func (t *tree) Kind() dialect.StmtKind {
	switch t.first().(type) {
	case *ast.SelectStmt:
		return dialect.StmtSelect
	case *ast.UpdateStmt:
		return dialect.StmtUpdate
	case *ast.InsertStmt:
		return dialect.StmtInsert
	case *ast.DeleteStmt:
		return dialect.StmtDelete
	default:
		return dialect.StmtOther
	}
}

func (t *tree) sel() *ast.SelectStmt {
	s, _ := t.first().(*ast.SelectStmt)
	return s
}

func (t *tree) Relations() []dialect.RelRef {
	var out []dialect.RelRef
	switch s := t.first().(type) {
	case *ast.SelectStmt:
		if s.From != nil {
			collectJoin(s.From.TableRefs, dialect.JoinBase, false, &out)
		}
	case *ast.UpdateStmt:
		if s.TableRefs != nil {
			collectJoin(s.TableRefs.TableRefs, dialect.JoinBase, false, &out)
		}
	case *ast.DeleteStmt:
		if s.TableRefs != nil {
			collectJoin(s.TableRefs.TableRefs, dialect.JoinBase, false, &out)
		}
	case *ast.InsertStmt:
		if s.Table != nil {
			collectJoin(s.Table.TableRefs, dialect.JoinBase, false, &out)
		}
	}
	locateRelations(t.sql, out)
	return out
}

func mapJoin(j *ast.Join) dialect.JoinType {
	switch j.Tp {
	case ast.CrossJoin:
		// TiDB models INNER JOIN as CrossJoin with a condition.
		if j.On == nil && len(j.Using) == 0 && !j.NaturalJoin {
			return dialect.JoinCross
		}
		return dialect.JoinInner
	case ast.LeftJoin:
		return dialect.JoinLeft
	case ast.RightJoin:
		return dialect.JoinRight
	default:
		return dialect.JoinOther
	}
}

// collectJoin flattens the FROM tree. nullable tracks null-extended
// outer-join sides (right of LEFT, left of RIGHT); MySQL has no FULL
// JOIN.
func collectJoin(node ast.ResultSetNode, join dialect.JoinType, nullable bool, out *[]dialect.RelRef) {
	switch v := node.(type) {
	case *ast.Join:
		leftNullable, rightNullable := nullable, nullable
		switch v.Tp {
		case ast.LeftJoin:
			rightNullable = true
		case ast.RightJoin:
			leftNullable = true
		}
		collectJoin(v.Left, join, leftNullable, out)
		if v.Right != nil {
			collectJoin(v.Right, mapJoin(v), rightNullable, out)
		}
	case *ast.TableSource:
		switch s := v.Source.(type) {
		case *ast.TableName:
			*out = append(*out, dialect.RelRef{
				Alias: v.AsName.O, Table: s.Name.O, Loc: -1,
				Join: join, NullableSide: nullable,
			})
		case *ast.Join: // parenthesized join
			collectJoin(s, join, nullable, out)
		default: // derived table (SELECT …) AS alias
			*out = append(*out, dialect.RelRef{
				Alias: v.AsName.O, Loc: -1, Join: join, NullableSide: nullable,
			})
		}
	case *ast.TableName:
		*out = append(*out, dialect.RelRef{
			Table: v.Name.O, Loc: -1, Join: join, NullableSide: nullable,
		})
	}
}

// locateRelations assigns Loc to each named relation by scanning the
// SQL token stream left to right. A relation name in FROM position is
// preceded by FROM/JOIN/STRAIGHT_JOIN/','/'('/'.'/INTO/UPDATE, which
// distinguishes it from column references of the same name; subqueries
// (parens whose first token is SELECT/WITH) are skipped whole so
// derived-table internals never match.
func locateRelations(sql string, rels []dialect.RelRef) {
	src := []byte(sql)
	profile := Profile{}
	pos := 0
	prev := "" // previous significant token, uppercased for idents

	next := func() (dialect.Token, bool) {
		for {
			tok, err := profile.NextToken(src, pos)
			if err != nil || tok.Kind == dialect.KindEOF {
				return tok, false
			}
			pos = tok.End
			switch tok.Kind {
			case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment:
				continue
			}
			return tok, true
		}
	}
	// skipParen consumes tokens until the matching ')' (the '(' has
	// already been consumed).
	var skipParen func()
	skipParen = func() {
		for {
			tok, ok := next()
			if !ok {
				return
			}
			switch tok.Kind {
			case dialect.KindLParen:
				skipParen()
			case dialect.KindRParen:
				return
			}
		}
	}

	// scanFor advances until an ident token equal to name in FROM
	// position; returns its start offset or -1 when the stream ends.
	scanFor := func(name string) int {
		for {
			tok, ok := next()
			if !ok {
				return -1
			}
			switch tok.Kind {
			case dialect.KindLParen:
				// Peek: subqueries are opaque; parenthesized joins are
				// transparent (the '(' itself is a valid predecessor).
				save := pos
				peek, ok2 := next()
				if ok2 && peek.Kind == dialect.KindIdent {
					if u := strings.ToUpper(peek.Text); u == "SELECT" || u == "WITH" {
						skipParen()
						prev = ")"
						continue
					}
				}
				pos = save
				prev = "("
			case dialect.KindIdent, dialect.KindQuotedIdent:
				tokName := tok.Text
				if tok.Kind == dialect.KindQuotedIdent {
					tokName = strings.ReplaceAll(tokName[1:len(tokName)-1], "``", "`")
				}
				match := tokName == name && fromPosition(prev)
				if tok.Kind == dialect.KindIdent {
					prev = strings.ToUpper(tok.Text)
				} else {
					prev = tokName
				}
				if match {
					return tok.Start
				}
			default:
				prev = tok.Text
			}
		}
	}

	for i := range rels {
		if rels[i].Table == "" {
			continue
		}
		rels[i].Loc = scanFor(rels[i].Table)
	}
}

func fromPosition(prev string) bool {
	switch prev {
	case "FROM", "JOIN", "STRAIGHT_JOIN", ",", "(", ".", "INTO", "UPDATE":
		return true
	}
	return false
}

// ColumnRefs walks the statement collecting every column reference,
// marking those inside subquery scopes (derived tables, CTEs,
// sub-selects) for the resolver's conservative treatment.
func (t *tree) ColumnRefs() []dialect.ColRef {
	n := t.first()
	if n == nil {
		return nil
	}
	v := &colRefVisitor{root: n}
	n.Accept(v)
	return v.out
}

type colRefVisitor struct {
	root     ast.Node
	subDepth int
	out      []dialect.ColRef
}

func (v *colRefVisitor) Enter(n ast.Node) (ast.Node, bool) {
	switch x := n.(type) {
	case *ast.SubqueryExpr:
		v.subDepth++
	case *ast.SelectStmt:
		if n != v.root {
			v.subDepth++
		}
	case *ast.ColumnNameExpr:
		cr := dialect.ColRef{Loc: x.OriginTextPosition(), InSubquery: v.subDepth > 0}
		if x.Name.Schema.O != "" {
			cr.Fields = append(cr.Fields, x.Name.Schema.O)
		}
		if x.Name.Table.O != "" {
			cr.Fields = append(cr.Fields, x.Name.Table.O)
		}
		cr.Fields = append(cr.Fields, x.Name.Name.O)
		v.out = append(v.out, cr)
	case *ast.SelectField:
		if x.WildCard != nil && v.subDepth == 0 {
			cr := dialect.ColRef{Star: true, Loc: x.Offset}
			if x.WildCard.Table.O != "" {
				cr.Fields = append(cr.Fields, x.WildCard.Table.O)
			}
			v.out = append(v.out, cr)
		}
	}
	return n, false
}

func (v *colRefVisitor) Leave(n ast.Node) (ast.Node, bool) {
	switch n.(type) {
	case *ast.SubqueryExpr:
		v.subDepth--
	case *ast.SelectStmt:
		if n != v.root {
			v.subDepth--
		}
	}
	return n, true
}

// DeepTables walks the whole statement and collects every TableName —
// subqueries and CTE bodies included, and CTE-name references with
// them (conservative, design 14 §11.1). TiDB relation nodes carry no
// byte offsets, so Loc is -1 throughout.
func (t *tree) DeepTables() []dialect.TableRef {
	n := t.first()
	if n == nil {
		return nil
	}
	v := &tableNameVisitor{}
	n.Accept(v)
	return v.out
}

type tableNameVisitor struct {
	out []dialect.TableRef
}

func (v *tableNameVisitor) Enter(n ast.Node) (ast.Node, bool) {
	if tn, ok := n.(*ast.TableName); ok {
		v.out = append(v.out, dialect.TableRef{Name: tn.Name.O, Loc: -1})
	}
	return n, false
}

func (v *tableNameVisitor) Leave(n ast.Node) (ast.Node, bool) { return n, true }

func (t *tree) TargetItems() []dialect.TargetItem {
	sel := t.sel()
	if sel == nil || sel.Fields == nil {
		return nil
	}
	var out []dialect.TargetItem
	for _, f := range sel.Fields.Fields {
		item := dialect.TargetItem{Name: f.AsName.O, Loc: f.Offset}
		if f.WildCard != nil {
			item.Star = true
			item.Qualifier = f.WildCard.Table.O
		}
		switch e := f.Expr.(type) {
		case *ast.FuncCallExpr:
			item.FuncName = e.FnName.L
		case *ast.AggregateFuncExpr:
			item.FuncName = strings.ToLower(e.F)
		}
		out = append(out, item)
	}
	return out
}

func (t *tree) TopConjunctLocs() []int {
	var where ast.ExprNode
	switch s := t.first().(type) {
	case *ast.SelectStmt:
		where = s.Where
	case *ast.UpdateStmt:
		where = s.Where
	case *ast.DeleteStmt:
		where = s.Where
	}
	var locs []int
	flattenConjuncts(where, &locs)
	return locs
}

func (t *tree) HavingConjunctLocs() []int {
	sel, ok := t.first().(*ast.SelectStmt)
	if !ok || sel.Having == nil {
		return nil
	}
	var locs []int
	flattenConjuncts(sel.Having.Expr, &locs)
	return locs
}

// flattenConjuncts mirrors the PostgreSQL facade's semantics: parens
// are transparent (pg_query drops them from the AST), and nested ANDs
// flatten.
func flattenConjuncts(e ast.ExprNode, out *[]int) {
	switch v := e.(type) {
	case nil:
		return
	case *ast.ParenthesesExpr:
		flattenConjuncts(v.Expr, out)
	case *ast.BinaryOperationExpr:
		if v.Op == opcode.LogicAnd {
			flattenConjuncts(v.L, out)
			flattenConjuncts(v.R, out)
			return
		}
		*out = append(*out, v.OriginTextPosition())
	default:
		*out = append(*out, e.OriginTextPosition())
	}
}

func (t *tree) OrderByLocs() []int {
	sel := t.sel()
	if sel == nil || sel.OrderBy == nil {
		return nil
	}
	var locs []int
	for _, it := range sel.OrderBy.Items {
		loc := it.Expr.OriginTextPosition()
		if p, ok := it.Expr.(*ast.ParenthesesExpr); ok {
			loc = p.Expr.OriginTextPosition()
		}
		locs = append(locs, loc)
	}
	return locs
}

// HasDistinctOn: MySQL has no DISTINCT ON.
func (t *tree) HasDistinctOn() bool { return false }

func (t *tree) HasLockingClause() bool {
	sel := t.sel()
	return sel != nil && hasLock(sel)
}

// HasFetchWithTies: MySQL has no FETCH FIRST … WITH TIES.
func (t *tree) HasFetchWithTies() bool { return false }
