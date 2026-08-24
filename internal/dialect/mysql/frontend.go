package mysql

import (
	"fmt"
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
	stmts, err := parseSQL(sql)
	if err != nil {
		return nil, toParseError(sql, err)
	}
	return &tree{sql: sql, stmts: stmts}, nil
}

// parseSQL wraps parser.ParseSQL with a panic guard. The test_driver
// value backend the parser requires (see the blank import) panics on
// literals its stub arithmetic does not model — MyDecimal.FromString
// on an integer literal wide enough to overflow into the decimal
// path, for one — and the parser's lexer folds such literals during
// scanning, so the panic is reachable from ANY parse of adversarial
// input. It must surface as an ordinary parse error, never a crash.
func parseSQL(sql string) (stmts []ast.StmtNode, err error) {
	defer func() {
		if r := recover(); r != nil {
			stmts = nil
			err = fmt.Errorf("parser panic: %v", r)
		}
	}()
	stmts, _, err = parser.New().ParseSQL(sql)
	return stmts, err
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
				Alias: v.AsName.O, Table: s.Name.O, Schema: s.Schema.O, Loc: -1,
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
			Table: v.Name.O, Schema: v.Schema.O, Loc: -1, Join: join, NullableSide: nullable,
		})
	}
}

// subTree wraps a subquery node (a derived-table body or a CTE body)
// in its own facade for recursive analysis. The sql text is dropped:
// relation locations inside subtrees stay -1, which the analyzer
// treats as "no fragment-range information" (guard skipping is a
// precision aid there, not a soundness requirement — R2 keeps guarded
// join columns out of the result).
func subTree(node ast.StmtNode) *tree { return &tree{stmts: []ast.StmtNode{node}} }

// HasSetOperation reports a statement-level set operation.
func (t *tree) HasSetOperation() bool {
	_, ok := t.first().(*ast.SetOprStmt)
	return ok
}

// HasUnresolvableProvenance: the protocol's org_table carries no
// database qualifier, so ANY db-qualified reference anywhere in the
// statement can be attributed to a same-named table of the connected
// database — every name-keyed attribution becomes untrustworthy.
func (t *tree) HasUnresolvableProvenance() bool {
	for _, tr := range t.DeepTables() {
		if tr.Schema != "" {
			return true
		}
	}
	return false
}

// DerivedRels collects FROM-reachable derived tables with
// sub-facades, tracking null-extension exactly like collectJoin.
func (t *tree) DerivedRels() []dialect.SubRel {
	var out []dialect.SubRel
	switch s := t.first().(type) {
	case *ast.SelectStmt:
		if s.From != nil {
			collectDerived(s.From.TableRefs, false, &out)
		}
	case *ast.UpdateStmt:
		if s.TableRefs != nil {
			collectDerived(s.TableRefs.TableRefs, false, &out)
		}
	case *ast.DeleteStmt:
		if s.TableRefs != nil {
			collectDerived(s.TableRefs.TableRefs, false, &out)
		}
	}
	return out
}

func collectDerived(node ast.ResultSetNode, nullable bool, out *[]dialect.SubRel) {
	switch v := node.(type) {
	case *ast.Join:
		leftNullable, rightNullable := nullable, nullable
		switch v.Tp {
		case ast.LeftJoin:
			rightNullable = true
		case ast.RightJoin:
			leftNullable = true
		}
		if v.Left != nil {
			collectDerived(v.Left, leftNullable, out)
		}
		if v.Right != nil {
			collectDerived(v.Right, rightNullable, out)
		}
	case *ast.TableSource:
		switch s := v.Source.(type) {
		case *ast.TableName:
		case *ast.Join: // parenthesized join
			collectDerived(s, nullable, out)
		case ast.StmtNode: // derived table (SELECT …/set op) AS alias
			*out = append(*out, dialect.SubRel{
				Alias: v.AsName.O, NullableSide: nullable, Tree: subTree(s),
			})
		}
	}
}

// CTEs returns the statement's WITH-list definitions. MySQL CTE
// bodies are always queries, so Tree is always non-nil.
func (t *tree) CTEs() []dialect.CTEDef {
	var wc *ast.WithClause
	switch s := t.first().(type) {
	case *ast.SelectStmt:
		wc = s.With
	case *ast.SetOprStmt:
		wc = s.With
	case *ast.UpdateStmt:
		wc = s.With
	case *ast.DeleteStmt:
		wc = s.With
	}
	if wc == nil {
		return nil
	}
	var out []dialect.CTEDef
	for _, c := range wc.CTEs {
		def := dialect.CTEDef{Name: c.Name.O, Recursive: wc.IsRecursive || c.IsRecursive}
		if c.Query != nil {
			if body, ok := c.Query.Query.(ast.StmtNode); ok {
				def.Tree = subTree(body)
			}
		}
		out = append(out, def)
	}
	return out
}

// HasGroupingSets reports MySQL's GROUP BY … WITH ROLLUP, which nulls
// grouping columns in super-aggregate rows.
func (t *tree) HasGroupingSets() bool {
	s := t.sel()
	return s != nil && s.GroupBy != nil && s.GroupBy.Rollup
}

// locateRelations assigns Loc to each named relation by scanning the
// SQL token stream left to right, tracking FROM-clause context. A
// relation name in FROM position is preceded by
// FROM/JOIN/STRAIGHT_JOIN/INTO/UPDATE (keyword predecessors, always
// valid), or by ','/'(' — but those two are structural ONLY inside a
// FROM/JOIN region: a comma in the SELECT list or a function-argument
// list, and parens introducing a SELECT-list/expression/function-call
// value or an index hint, are NOT FROM openings. A '.' predecessor
// introduces the table half of a db-qualified name whose qualifier was
// itself in FROM position (`db.t`). Subqueries and (VALUES …) table
// constructors (parens whose first token is SELECT/WITH/VALUES) are
// skipped whole; so are opaque parens outside table-factor position.
//
// The scan is a single forward pass: scanFor resumes the shared position
// and context, so the relations must be supplied in source order
// (collectJoin produces them left to right).
func locateRelations(sql string, rels []dialect.RelRef) {
	src := []byte(sql)
	profile := Profile{}
	pos := 0
	prev := ""                  // previous significant token, uppercased for idents
	inFrom := false             // are we inside a FROM/JOIN table-reference region
	prevIdentInFrom := false    // was the previous ident in FROM position
	dotQualifierInFrom := false // set when a '.' follows such an ident
	aliasSlot := false          // an optional table-alias slot is open here

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
				// Peek: subqueries and (VALUES …) table constructors are
				// opaque and skipped whole. A parenthesized table-references
				// group is transparent ONLY when the '(' sits at a
				// table-factor start inside a FROM region; every other paren
				// (function call, expression, index/partition hint, ON
				// condition) is opaque so its contents never match.
				aliasSlot = false
				save := pos
				peek, ok2 := next()
				if ok2 && peek.Kind == dialect.KindIdent {
					if u := strings.ToUpper(peek.Text); u == "SELECT" || u == "WITH" || u == "VALUES" {
						skipParen()
						prev = ")"
						prevIdentInFrom = false
						continue
					}
				}
				pos = save
				if inFrom && tableFactorStart(prev) {
					// Transparent grouping paren: the '(' is a valid
					// predecessor for the table factor that follows.
					prev = "("
					prevIdentInFrom = false
				} else {
					skipParen()
					prev = ")"
					prevIdentInFrom = false
				}
			case dialect.KindIdent, dialect.KindQuotedIdent:
				tokName := tok.Text
				if tok.Kind == dialect.KindQuotedIdent {
					tokName = strings.ReplaceAll(tokName[1:len(tokName)-1], "``", "`")
				}
				inPos := keywordPred(prev) ||
					(prev == "," && inFrom) ||
					prev == "(" || // set only for a transparent grouping paren
					(prev == "." && dotQualifierInFrom)
				match := tokName == name && inPos
				// aliasSlot: the previous token left an optional table-alias
				// slot open — either a relation in FROM position, or a
				// `relation AS` pair. A NON-RESERVED keyword landing here is
				// that bare alias, not a clause, so it must not close the
				// FROM region (`FROM t1 offset, t2` / `FROM t1 AS offset,
				// t2`: `offset` aliases t1, and closing here would orphan t2
				// at Loc=-1). A RESERVED closer (WHERE/GROUP/…) can never be
				// a bare alias, so it still closes even in this slot.
				curAlias := aliasSlot
				aliasSlot = false
				if tok.Kind == dialect.KindIdent {
					u := strings.ToUpper(tok.Text)
					// STRAIGHT_JOIN has TWO positions in MySQL: a join
					// operator (`a STRAIGHT_JOIN b`, always inside an already
					// open FROM region) and a SELECT modifier (`SELECT
					// STRAIGHT_JOIN col …`, among the select options like
					// SQL_CALC_FOUND_ROWS/DISTINCT). A modifier occurrence —
					// reached with NO FROM region open and not itself in
					// relation position — must NOT open the region or
					// introduce the following identifier as a relation, or a
					// bare select-list column equal to a table name would be
					// pinned at its select-list offset. Neutralize prev so it
					// is not a keyword predecessor for the next token; the
					// join-operator form (inFrom already true) falls through
					// and opens/continues the region as before.
					if u == "STRAIGHT_JOIN" && !inFrom && !inPos {
						prev = selectOptionPrev
					} else {
						prev = u
						// FROM-region state: openers enter it, clause-enders
						// leave it. This gates the structural ',' predecessor so
						// a `GROUP BY a, b` comma is never a table separator.
						// A keyword that ARRIVED in FROM position (inPos) is
						// being used as a non-reserved UNQUOTED table name —
						// MySQL allows `FROM offset o, t2`, where `offset` is a
						// relation, not the OFFSET clause. It is consumed as a
						// relation here, so it must NOT also close the region and
						// orphan the following comma-joined relation (Loc=-1).
						// Likewise an alias-capable keyword filling an alias slot.
						// Only a closer NOT in relation position and NOT filling
						// an alias slot ends the region.
						if !inPos {
							if isFromOpener(u) {
								inFrom = true
							} else if isFromCloser(u) && !(curAlias && isAliasCapableCloser(u)) {
								inFrom = false
							}
						}
						// Reopen the alias slot for the NEXT token: after a
						// relation in FROM position, or after an `AS` that itself
						// sat in an alias slot (`relation AS <alias>`).
						if inPos || (curAlias && u == "AS") {
							aliasSlot = true
						}
					}
				} else {
					// A quoted identifier is never a keyword: keep its
					// content out of `prev` so a relation named e.g.
					// `FROM` cannot act as the FROM keyword for the next
					// token, and it never opens/closes the FROM region. Its
					// FROM-position status still flows through
					// prevIdentInFrom for a following qualified name.
					prev = quotedIdentPrev
					// A quoted relation may still take an alias next.
					if inPos {
						aliasSlot = true
					}
				}
				prevIdentInFrom = inPos
				if match {
					return tok.Start
				}
			default:
				// Punctuation (',', '.', operators) never fills an alias
				// slot; the next relation after a ',' opens a fresh one.
				aliasSlot = false
				if tok.Text == "." {
					dotQualifierInFrom = prevIdentInFrom
				}
				prev = tok.Text
				prevIdentInFrom = false
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

// quotedIdentPrev is the `prev` sentinel recorded after a backquoted
// identifier: a non-keyword, non-punctuation marker so quoted content can
// never be read as a FROM-introducing token.
const quotedIdentPrev = "\x00quoted"

// selectOptionPrev is the neutral `prev` sentinel recorded for a
// STRAIGHT_JOIN seen in SELECT-modifier position (like
// SQL_CALC_FOUND_ROWS). It is a non-keyword, non-punctuation marker so
// that occurrence neither opens the FROM region nor acts as a
// relation-introducing predecessor for the following token.
const selectOptionPrev = "\x00option"

// keywordPred reports whether prev is a keyword that directly introduces
// a relation name, valid anywhere (these keywords only appear as FROM
// openers at statement depth; nested occurrences are skipped whole).
// '.' is deliberately excluded: a qualified name's '.' is handled with a
// qualifier-in-FROM check so a `x.y` column reference is not mistaken for
// a relation. ','/'(' are excluded here — they are structural predecessors
// gated on FROM context by the caller.
func keywordPred(prev string) bool {
	switch prev {
	case "FROM", "JOIN", "STRAIGHT_JOIN", "INTO", "UPDATE":
		return true
	}
	return false
}

// tableFactorStart reports whether prev marks the start of a table factor,
// where a '(' opens a parenthesized table-references group (transparent)
// rather than an expression or index hint (opaque).
func tableFactorStart(prev string) bool {
	switch prev {
	case "FROM", "JOIN", "STRAIGHT_JOIN", "INTO", "UPDATE", ",", "(":
		return true
	}
	return false
}

// isFromOpener reports whether the uppercased keyword opens a FROM/JOIN
// table-reference region.
func isFromOpener(u string) bool {
	switch u {
	case "FROM", "JOIN", "STRAIGHT_JOIN", "INTO", "UPDATE":
		return true
	}
	return false
}

// isFromCloser reports whether the uppercased keyword ends the FROM/JOIN
// region at statement depth (the following ','/'(' are no longer
// structural). ON/USING are deliberately NOT closers: a comma after a
// join condition resumes the table-reference list (a cross join), and ON
// parens are already opaque because ON is not a table-factor start.
func isFromCloser(u string) bool {
	switch u {
	case "WHERE", "GROUP", "HAVING", "ORDER", "LIMIT", "OFFSET",
		"WINDOW", "UNION", "EXCEPT", "INTERSECT", "FOR", "LOCK",
		"SET", "VALUES", "PROCEDURE":
		return true
	}
	return false
}

// isAliasCapableCloser reports whether a FROM-region closer keyword is
// ALSO a non-reserved MySQL keyword, hence a legal bare table alias. Only
// such a keyword may fill an alias slot; when it does it is the alias, not
// the clause, so it must NOT close the region (`FROM t1 offset, t2`).
// Among the closer set only OFFSET is non-reserved — WHERE/GROUP/HAVING/
// ORDER/LIMIT/WINDOW/UNION/EXCEPT/INTERSECT/FOR/LOCK/SET/VALUES/PROCEDURE
// are all reserved and so can never stand as a bare alias (they still
// close even directly after a relation, e.g. `FROM offset GROUP BY …`).
func isAliasCapableCloser(u string) bool {
	return u == "OFFSET"
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
	// scopes holds the effective FROM names of the enclosing subquery
	// selects, flat, with marks recording each pushed frame's start so
	// Leave can pop it. A column reference's ScopeAliases is a copy of
	// the whole stack (the union across enclosing subquery levels). The
	// top-level select's FROM is never pushed (n == root).
	scopes []string
	marks  []int
	// hidden stacks the FROM frames temporarily removed while a WITH
	// clause's CTE bodies are walked: a non-lateral CTE body cannot see
	// the FROM items of the select that defines it, so its own select's
	// frame (pushed on Enter) is set aside for the duration of the WITH
	// clause and restored on Leave.
	hidden [][]string
}

func (v *colRefVisitor) Enter(n ast.Node) (ast.Node, bool) {
	switch x := n.(type) {
	case *ast.SubqueryExpr:
		v.subDepth++
	case *ast.SelectStmt:
		if n != v.root {
			v.subDepth++
			v.marks = append(v.marks, len(v.scopes))
			v.scopes = append(v.scopes, selectFromNames(x)...)
		}
	case *ast.WithClause:
		// CTE bodies see only the enclosing scope, never the FROM names
		// of the select that defines the WITH. Set aside that select's
		// own frame (the current top frame) while its CTE bodies are
		// walked; Leave(*ast.WithClause) restores it. The root select
		// pushes no frame, so a top-level WITH has nothing to hide.
		if len(v.marks) > 0 {
			m := v.marks[len(v.marks)-1]
			v.hidden = append(v.hidden, append([]string(nil), v.scopes[m:]...))
			v.scopes = v.scopes[:m]
		} else {
			v.hidden = append(v.hidden, nil)
		}
	case *ast.ColumnNameExpr:
		cr := dialect.ColRef{Loc: x.OriginTextPosition(), InSubquery: v.subDepth > 0}
		if len(v.scopes) > 0 {
			cr.ScopeAliases = append([]string(nil), v.scopes...)
		}
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
			last := len(v.marks) - 1
			v.scopes = v.scopes[:v.marks[last]]
			v.marks = v.marks[:last]
		}
	case *ast.WithClause:
		last := len(v.hidden) - 1
		v.scopes = append(v.scopes, v.hidden[last]...)
		v.hidden = v.hidden[:last]
	}
	return n, true
}

// selectFromNames returns the effective FROM names (alias else table)
// of a select's own FROM clause. Set-operation branches are not
// descended (under-collection is sound; see dialect.ColRef.ScopeAliases).
func selectFromNames(sel *ast.SelectStmt) []string {
	if sel == nil || sel.From == nil {
		return nil
	}
	var rels []dialect.RelRef
	collectJoin(sel.From.TableRefs, dialect.JoinBase, false, &rels)
	names := make([]string, 0, len(rels))
	for _, r := range rels {
		switch {
		case r.Alias != "":
			names = append(names, r.Alias)
		case r.Table != "":
			names = append(names, r.Table)
		}
	}
	return names
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
		v.out = append(v.out, dialect.TableRef{Name: tn.Name.O, Schema: tn.Schema.O, Loc: -1})
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
			// A schema-qualified call (`mydb.count(x)`) invokes a STORED
			// function that merely shares a builtin's name — it is not the
			// total builtin. Leaving FuncName blank keeps the nullability
			// analyzer's funcWhitelist/strictAggs from matching it, so its
			// column is not narrowed to non-nullable on a false pretense.
			if e.Schema.O == "" {
				item.FuncName = e.FnName.L
			}
		case *ast.AggregateFuncExpr:
			item.FuncName = strings.ToLower(e.F)
			// MySQL has no FILTER clause, and window functions parse
			// as WindowFuncExpr — a plain aggregate over one bare
			// column qualifies.
			if len(e.Args) == 1 {
				if cn, ok := e.Args[0].(*ast.ColumnNameExpr); ok {
					item.AggArg = columnPath(cn.Name)
				}
			}
		}
		item.Total = totalExpr(f.Expr)
		out = append(out, item)
	}
	return out
}

func columnPath(n *ast.ColumnName) []string {
	var out []string
	if n.Table.O != "" {
		out = append(out, n.Table.O)
	}
	out = append(out, n.Name.O)
	return out
}

// totalExpr reports a data-independent never-NULL expression (see
// dialect.TargetItem.Total).
func totalExpr(e ast.ExprNode) bool {
	switch v := e.(type) {
	case ast.ValueExpr:
		return v.GetValue() != nil
	case *ast.ExistsSubqueryExpr:
		return true
	case *ast.IsNullExpr, *ast.IsTruthExpr:
		return true
	case *ast.ParenthesesExpr:
		return totalExpr(v.Expr)
	case *ast.FuncCastExpr:
		// MySQL CAST returns NULL on conversion failure — never total.
		return false
	case *ast.FuncCallExpr:
		if v.FnName.L == "coalesce" {
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

func (t *tree) HasGroupBy() bool {
	s := t.sel()
	return s != nil && s.GroupBy != nil && len(s.GroupBy.Items) > 0
}

// NotNullConjuncts finds depth-0 WHERE conjuncts of the exact form
// `col IS NOT NULL`.
func (t *tree) NotNullConjuncts() []dialect.ColRef {
	var where ast.ExprNode
	switch s := t.first().(type) {
	case *ast.SelectStmt:
		where = s.Where
	case *ast.UpdateStmt:
		where = s.Where
	case *ast.DeleteStmt:
		where = s.Where
	}
	var nodes []ast.ExprNode
	flattenConjunctNodes(where, &nodes)
	var out []dialect.ColRef
	for _, c := range nodes {
		nt, ok := c.(*ast.IsNullExpr)
		if !ok || !nt.Not {
			continue
		}
		cn, ok := nt.Expr.(*ast.ColumnNameExpr)
		if !ok {
			continue
		}
		out = append(out, dialect.ColRef{
			Fields: columnPath(cn.Name), Loc: nt.OriginTextPosition(),
		})
	}
	return out
}

func flattenConjunctNodes(e ast.ExprNode, out *[]ast.ExprNode) {
	switch v := e.(type) {
	case nil:
		return
	case *ast.ParenthesesExpr:
		flattenConjunctNodes(v.Expr, out)
	case *ast.BinaryOperationExpr:
		if v.Op == opcode.LogicAnd {
			flattenConjunctNodes(v.L, out)
			flattenConjunctNodes(v.R, out)
			return
		}
		*out = append(*out, v)
	default:
		*out = append(*out, e)
	}
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
