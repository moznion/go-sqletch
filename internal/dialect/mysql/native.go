package mysql

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/dialect"
)

// NativeOracle is the native-inference oracle backend (design 15):
// Describe answered by sqletch's own name resolution over a DDL-built
// catalog plus the Tier 2 annotations, with no server anywhere. It is
// strict and fail-closed — anything outside the modeled subset is a
// NativeUnsupportedError (SQLETCH214), never a guess — and its
// answers must be byte-identical to the server backend's for every
// input it accepts (the corpus gate pins this).
//
// v1 subset: single top-level SELECT/INSERT/UPDATE/DELETE statements
// over catalog tables. No derived tables and no subqueries (except
// the dialect's own arity-0 @in emission); expression result columns
// need an AS alias and a `-- @column` annotation (D3/D4); ENUM/SET
// result columns are refused (their wire form differs from their
// catalog form).
type NativeOracle struct {
	cat     *cache.Catalog
	version string
}

// NewNativeOracle builds the backend from the ordered schema inputs
// and the pinned server version (which it reports verbatim — under a
// native backend the pin IS the modeled engine).
func NewNativeOracle(schema []cache.SchemaFile, serverVersion string) (*NativeOracle, error) {
	cat, err := BuildCatalog(schema)
	if err != nil {
		return nil, err
	}
	return &NativeOracle{cat: cat, version: serverVersion}, nil
}

func (o *NativeOracle) Describe(ctx context.Context, sql string) (dialect.Desc, error) {
	if err := ctx.Err(); err != nil {
		return dialect.Desc{}, err
	}
	stmts, err := parseSQL(sql)
	if err != nil {
		return dialect.Desc{}, toParseOracleError(sql, err)
	}
	if len(stmts) != 1 {
		return dialect.Desc{}, &dialect.OracleError{Pos: -1,
			Msg: fmt.Sprintf("expected one statement, got %d", len(stmts))}
	}

	d := describer{cat: o.cat, hints: parseColumnHints(sql)}
	desc := dialect.Desc{}
	switch s := stmts[0].(type) {
	case *ast.SelectStmt:
		cols, err := d.describeSelect(s)
		if err != nil {
			return dialect.Desc{}, err
		}
		desc.Columns = cols
	case *ast.InsertStmt:
		if err := d.describeInsert(s); err != nil {
			return dialect.Desc{}, err
		}
	case *ast.UpdateStmt:
		if err := d.describeUpdate(s); err != nil {
			return dialect.Desc{}, err
		}
	case *ast.DeleteStmt:
		if err := d.describeDelete(s); err != nil {
			return dialect.Desc{}, err
		}
	default:
		return dialect.Desc{}, &dialect.NativeUnsupportedError{Pos: -1,
			Construct: fmt.Sprintf("a %T statement", stmts[0]),
			Hint:      "the native backend models SELECT/INSERT/UPDATE/DELETE only"}
	}

	// Parameter slots are untyped by the protocol on MySQL; the
	// pipeline fills them from the mandatory `-- @param` annotations.
	// Emitting the same zero slots keeps entries byte-identical.
	if n := countPlaceholders(sql); n > 0 {
		desc.Params = make([]dialect.TypeRef, n)
	}
	return desc, nil
}

// Plan validates like Describe and claims nothing more: there is no
// planner here (design 15 D2). check --exhaustive under the native
// backend prints that EXPLAIN coverage needs a server-backed run.
func (o *NativeOracle) Plan(ctx context.Context, sql string) error {
	_, err := o.Describe(ctx, sql)
	return err
}

func (o *NativeOracle) Snapshot(ctx context.Context) (*cache.Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cp := *o.cat
	return &cp, nil
}

func (o *NativeOracle) ServerVersion(context.Context) (string, error) {
	return o.version, nil
}

// ---- resolution ------------------------------------------------------------

type nativeRel struct {
	name  string // effective qualifier: alias if present, else table name
	alias bool
	table *cache.Table
}

type describer struct {
	cat   *cache.Catalog
	hints map[string]dialect.TypeRef
	scope []nativeRel
	// aliases holds output-column names, visible to GROUP BY / HAVING
	// / ORDER BY resolution like MySQL's.
	aliases map[string]bool
}

func (d *describer) scopeFrom(refs *ast.TableRefsClause) ([]nativeRel, error) {
	if refs == nil {
		return nil, nil
	}
	var rels []dialect.RelRef
	collectJoin(refs.TableRefs, dialect.JoinBase, false, &rels)
	scope := make([]nativeRel, 0, len(rels))
	for _, r := range rels {
		if r.Table == "" {
			return nil, &dialect.NativeUnsupportedError{Pos: -1,
				Construct: "a derived table (subquery in FROM)",
				Hint:      "flatten the query or switch to database.oracle: \"server\""}
		}
		if r.Schema != "" {
			// The native backend models a SINGLE DDL-built database with
			// no DSN: a cross-database reference (`otherdb.users`) is
			// unmodelable. Looking it up by the bare table name would type
			// columns from the WRONG (local) table, so fail closed here —
			// the FROM twin of the schema-qualified column/star refusals.
			return nil, &dialect.NativeUnsupportedError{Pos: r.Loc,
				Construct: "a schema-qualified FROM table",
				Hint:      "drop the schema qualifier, or switch to database.oracle: \"server\""}
		}
		tb := d.cat.Lookup(r.Table)
		if tb == nil {
			return nil, &dialect.OracleError{Pos: -1,
				Msg: fmt.Sprintf("table %q doesn't exist", r.Table)}
		}
		name, isAlias := r.Table, false
		if r.Alias != "" {
			name, isAlias = r.Alias, true
		}
		scope = append(scope, nativeRel{name: name, alias: isAlias, table: tb})
	}
	// Effective qualifiers (alias-else-table) must be unique, or a
	// qualified reference has no single referent — MySQL rejects the
	// statement outright (ER_NONUNIQ_TABLE) before it looks at any
	// column. The comparison is case-insensitive, matching MySQL's
	// alias/identifier handling. Skipping this let `FROM t, t` and
	// `FROM t AS a, u AS a` sail through with err=nil.
	seen := make(map[string]bool, len(scope))
	for i := range scope {
		key := strings.ToLower(scope[i].name)
		if seen[key] {
			return nil, &dialect.OracleError{Pos: -1,
				Msg: fmt.Sprintf("not unique table/alias: %q", scope[i].name)}
		}
		seen[key] = true
	}
	return scope, nil
}

// walkJoinConds resolves every JOIN ON condition in a FROM tree and
// refuses NATURAL/USING joins. scopeFrom flattens the FROM tree into a
// flat relation scope but drops the join conditions entirely, so a
// query joining ON a nonexistent column (or with a bad USING column)
// used to describe with err=nil and then fail at execution on a real
// server — the same fail-closed hole as an unread WITH.
//
// NATURAL and USING joins are refused (SQLETCH214): MySQL COALESCEs
// their common columns into a single output column (appearing once,
// from the first table), a projected shape the native star expander
// cannot reproduce byte-identically. Refuse rather than guess. This
// also makes USING-column resolution moot for those joins.
func (d *describer) walkJoinConds(node ast.ResultSetNode) error {
	switch v := node.(type) {
	case *ast.Join:
		if v.NaturalJoin || len(v.Using) > 0 {
			return &dialect.NativeUnsupportedError{Pos: -1,
				Construct: "a NATURAL or USING join",
				Hint:      "spell the condition as ON a.col = b.col, or switch to database.oracle: \"server\""}
		}
		if err := d.walkJoinConds(v.Left); err != nil {
			return err
		}
		if v.Right != nil {
			if err := d.walkJoinConds(v.Right); err != nil {
				return err
			}
		}
		if v.On != nil {
			if err := d.resolveExpr(v.On.Expr, false); err != nil {
				return err
			}
		}
	case *ast.TableSource:
		// A parenthesized join nests under a TableSource.
		if j, ok := v.Source.(*ast.Join); ok {
			return d.walkJoinConds(j)
		}
	}
	return nil
}

// scopeTarget reports whether name matches a relation in scope by its
// effective qualifier (alias-else-table): aliases match
// case-insensitively, bare table names as spelled — mirroring resolve.
func (d *describer) scopeTarget(name string) bool {
	for i := range d.scope {
		r := &d.scope[i]
		if r.alias && strings.EqualFold(r.name, name) {
			return true
		}
		if !r.alias && r.name == name {
			return true
		}
	}
	return false
}

// resolve finds [qualifier.]name in scope. MySQL matches column names
// and aliases case-insensitively; bare table names used as qualifiers
// match as spelled (the devdb servers run case-sensitive table names).
func (d *describer) resolve(qualifier, name string, pos int) (*nativeRel, *cache.Column, error) {
	var relMatches []*nativeRel
	scope := d.scope
	for i := range scope {
		r := &scope[i]
		if qualifier != "" {
			if r.alias && !strings.EqualFold(r.name, qualifier) {
				continue
			}
			if !r.alias && r.name != qualifier {
				continue
			}
		}
		relMatches = append(relMatches, r)
	}
	if qualifier != "" && len(relMatches) == 0 {
		return nil, nil, &dialect.OracleError{Pos: pos,
			Msg: fmt.Sprintf("unknown table %q", qualifier)}
	}
	var foundRel *nativeRel
	var foundCol *cache.Column
	for _, r := range relMatches {
		for i := range r.table.Cols {
			c := &r.table.Cols[i]
			if !strings.EqualFold(c.Name, name) {
				continue
			}
			if foundCol != nil {
				return nil, nil, &dialect.OracleError{Pos: pos,
					Msg: fmt.Sprintf("column %q in field list is ambiguous", name)}
			}
			foundRel, foundCol = r, c
		}
	}
	if foundCol == nil {
		return nil, nil, &dialect.OracleError{Pos: pos,
			Msg: fmt.Sprintf("unknown column %q", qualifiedName(qualifier, name))}
	}
	return foundRel, foundCol, nil
}

func qualifiedName(qualifier, name string) string {
	if qualifier == "" {
		return name
	}
	return qualifier + "." + name
}

// typeRefForColumn reproduces refFromField for a catalog column: the
// encoded wire type from the catalog's COLUMN_TYPE via TypeByName,
// spelled with typeCodeName. ENUM/SET are refused — the protocol
// reports them as CHAR with flag bits, a wire form the catalog cannot
// reproduce (v1 exclusion, kept honest by the differential gate).
func typeRefForColumn(c *cache.Column, pos int) (dialect.TypeRef, error) {
	tr, ok := (TypeMap{}).TypeByName(c.TypeName)
	if !ok {
		return dialect.TypeRef{}, &dialect.NativeUnsupportedError{Pos: pos,
			Construct: fmt.Sprintf("a column of type %q", c.TypeName),
			Hint:      "switch to database.oracle: \"server\" for this schema"}
	}
	base := tr.OID &^ (FlagUnsigned | FlagBinary)
	if base == typeEnum || base == typeSet {
		return dialect.TypeRef{}, &dialect.NativeUnsupportedError{Pos: pos,
			Construct: fmt.Sprintf("projecting the %s column %q", tr.Name, c.Name),
			Hint:      "ENUM/SET wire types differ from their catalog form; cast the column or switch to database.oracle: \"server\""}
	}
	oid := wireNormalize(tr.OID)
	return dialect.TypeRef{OID: oid, Name: typeCodeName(oid)}, nil
}

// wireNormalize maps a catalog-derived encoded type to what the
// protocol actually reports, pinned by the differential gate: every
// TEXT/BLOB flavor arrives as BLOB (only the binary charset
// distinguishes text from blob), and YEAR/BIT carry the UNSIGNED
// flag.
func wireNormalize(oid uint32) uint32 {
	switch oid &^ (FlagUnsigned | FlagBinary) {
	case typeTinyBlob, typeMedumBlob, typeLongBlob:
		return typeBlob | (oid & FlagBinary)
	case typeYear, typeBit:
		return oid | FlagUnsigned
	}
	return oid
}

// ---- SELECT ----------------------------------------------------------------

// refuseWith rejects a statement-level WITH (CTE) clause. CTEs are
// outside the native subset (design 15: "no derived tables and no
// subqueries"), and the describers do not model them: a name resolving
// to a CTE would need the CTE body's own column resolution. Skipping
// s.With silently let a query whose main statement is independently
// valid — an unused CTE, or one whose body references a nonexistent
// table — pass describe with err=nil and then fail at execution on a
// real server. Fail closed here instead (SQLETCH214).
func refuseWith(w *ast.WithClause) error {
	if w == nil {
		return nil
	}
	return &dialect.NativeUnsupportedError{Pos: -1,
		Construct: "a WITH (common table expression) clause",
		Hint:      "inline the CTE or switch to database.oracle: \"server\""}
}

// refuseNamedWindow rejects a statement-level WINDOW clause. A named
// window's definition lives only in SelectStmt.WindowSpecs, never in the
// select-list expressions the describer walks, so a body referencing a
// nonexistent column (or an unused named window) would otherwise pass
// describe with err=nil and fail at execution on a real server — the
// same silent sail-through as an unread WITH. Inline OVER(...) specs in
// the select list are walked normally and are unaffected. Fail closed
// (SQLETCH214).
func refuseNamedWindow(specs []ast.WindowSpec) error {
	if len(specs) == 0 {
		return nil
	}
	return &dialect.NativeUnsupportedError{Pos: -1,
		Construct: "a named WINDOW clause",
		Hint:      "inline the window as OVER (...) or switch to database.oracle: \"server\""}
}

func (d *describer) describeSelect(s *ast.SelectStmt) ([]dialect.ColumnDesc, error) {
	if err := refuseWith(s.With); err != nil {
		return nil, err
	}
	if err := refuseNamedWindow(s.WindowSpecs); err != nil {
		return nil, err
	}
	scope, err := d.scopeFrom(s.From)
	if err != nil {
		return nil, err
	}
	d.scope = scope
	d.aliases = map[string]bool{}

	// JOIN ON conditions must resolve (and NATURAL/USING joins are
	// refused) before anything projects — a bad ON reference fails the
	// server's prepare, and a USING join changes the * shape.
	if s.From != nil {
		if err := d.walkJoinConds(s.From.TableRefs); err != nil {
			return nil, err
		}
	}

	var cols []dialect.ColumnDesc
	if s.Fields == nil {
		return nil, &dialect.OracleError{Pos: -1, Msg: "statement has no select list"}
	}
	for _, f := range s.Fields.Fields {
		switch {
		case f.WildCard != nil:
			expanded, err := d.expandStar(f)
			if err != nil {
				return nil, err
			}
			cols = append(cols, expanded...)
		default:
			c, err := d.describeField(f)
			if err != nil {
				return nil, err
			}
			cols = append(cols, c)
			d.aliases[c.Name] = true
		}
	}

	// Non-projection clauses must still resolve — a bad reference
	// there fails the server's prepare, so it must fail here.
	if err := d.resolveExpr(s.Where, false); err != nil {
		return nil, err
	}
	if s.GroupBy != nil {
		for _, item := range s.GroupBy.Items {
			if err := d.resolveExpr(item.Expr, true); err != nil {
				return nil, err
			}
		}
	}
	if s.Having != nil {
		if err := d.resolveExpr(s.Having.Expr, true); err != nil {
			return nil, err
		}
	}
	if s.OrderBy != nil {
		for _, item := range s.OrderBy.Items {
			if err := d.resolveExpr(item.Expr, true); err != nil {
				return nil, err
			}
		}
	}
	return cols, nil
}

func (d *describer) expandStar(f *ast.SelectField) ([]dialect.ColumnDesc, error) {
	qualifier := f.WildCard.Table.O
	if f.WildCard.Schema.O != "" {
		return nil, &dialect.NativeUnsupportedError{Pos: f.Offset,
			Construct: "a schema-qualified star", Hint: "drop the schema qualifier"}
	}
	if len(d.scope) == 0 {
		return nil, &dialect.OracleError{Pos: f.Offset, Msg: "* with no tables"}
	}
	var out []dialect.ColumnDesc
	for i := range d.scope {
		r := &d.scope[i]
		if qualifier != "" {
			match := (r.alias && strings.EqualFold(r.name, qualifier)) || (!r.alias && r.name == qualifier)
			if !match {
				continue
			}
		}
		for j := range r.table.Cols {
			c := &r.table.Cols[j]
			tr, err := typeRefForColumn(c, f.Offset)
			if err != nil {
				return nil, err
			}
			out = append(out, dialect.ColumnDesc{
				Name: c.Name, Type: tr, SrcRel: r.table.OID, SrcAtt: c.Att,
			})
		}
		if qualifier != "" {
			return out, nil
		}
	}
	if qualifier != "" {
		return nil, &dialect.OracleError{Pos: f.Offset,
			Msg: fmt.Sprintf("unknown table %q", qualifier)}
	}
	return out, nil
}

func (d *describer) describeField(f *ast.SelectField) (dialect.ColumnDesc, error) {
	if ref, ok := f.Expr.(*ast.ColumnNameExpr); ok {
		if ref.Name.Schema.O != "" {
			return dialect.ColumnDesc{}, &dialect.NativeUnsupportedError{
				Pos: ref.OriginTextPosition(), Construct: "a schema-qualified column reference",
				Hint: "qualify with the table or alias only"}
		}
		rel, colRef, err := d.resolve(ref.Name.Table.O, ref.Name.Name.O, ref.OriginTextPosition())
		if err != nil {
			return dialect.ColumnDesc{}, err
		}
		tr, err := typeRefForColumn(colRef, ref.OriginTextPosition())
		if err != nil {
			return dialect.ColumnDesc{}, err
		}
		name := f.AsName.O
		if name == "" {
			name = ref.Name.Name.O // output name is the reference as written
		}
		return dialect.ColumnDesc{
			Name: name, Type: tr, SrcRel: rel.table.OID, SrcAtt: colRef.Att,
		}, nil
	}

	// Expression column: alias and annotation required (D3/D4), and
	// every column reference inside it must still resolve.
	if err := d.resolveExpr(f.Expr, false); err != nil {
		return dialect.ColumnDesc{}, err
	}
	if f.AsName.O == "" {
		return dialect.ColumnDesc{}, &dialect.NativeUnsupportedError{Pos: f.Offset,
			Construct: "an expression column without an AS alias",
			Hint:      "name it: `expr AS alias` plus `-- @column alias: type`"}
	}
	tr, ok := d.hints[f.AsName.O]
	if !ok {
		return dialect.ColumnDesc{}, &dialect.NativeUnsupportedError{Pos: f.Offset,
			Construct: fmt.Sprintf("the expression column %q without a `-- @column` annotation", f.AsName.O),
			Hint:      fmt.Sprintf("add `-- @column %s: <sql type>` (the server backend would infer it; the native backend never guesses)", f.AsName.O)}
	}
	return dialect.ColumnDesc{Name: f.AsName.O, Type: tr}, nil
}

// ---- DML -------------------------------------------------------------------

func (d *describer) describeInsert(s *ast.InsertStmt) error {
	if s.Select != nil {
		return &dialect.NativeUnsupportedError{Pos: -1,
			Construct: "INSERT ... SELECT",
			Hint:      "switch to database.oracle: \"server\" for this query"}
	}
	scope, err := d.scopeFrom(s.Table)
	if err != nil {
		return err
	}
	d.scope = scope
	d.aliases = map[string]bool{}
	if len(scope) != 1 {
		return &dialect.OracleError{Pos: -1, Msg: "INSERT needs exactly one target table"}
	}

	width := len(scope[0].table.Cols)
	if len(s.Columns) > 0 {
		width = len(s.Columns)
		for _, cn := range s.Columns {
			if _, _, err := d.resolve(cn.Table.O, cn.Name.O, cn.OriginTextPosition()); err != nil {
				return err
			}
		}
	}
	for _, row := range s.Lists {
		if len(row) != width {
			return &dialect.OracleError{Pos: -1,
				Msg: fmt.Sprintf("column count doesn't match value count (%d vs %d)", width, len(row))}
		}
		for _, e := range row {
			if err := d.resolveExpr(e, false); err != nil {
				return err
			}
		}
	}
	// INSERT ... SET is normalized by the parser into Columns/Lists,
	// so the checks above already cover it.
	for _, a := range s.OnDuplicate {
		if _, _, err := d.resolve(a.Column.Table.O, a.Column.Name.O, a.Column.OriginTextPosition()); err != nil {
			return err
		}
		if err := d.resolveExpr(a.Expr, false); err != nil {
			return err
		}
	}
	return nil
}

func (d *describer) describeUpdate(s *ast.UpdateStmt) error {
	if err := refuseWith(s.With); err != nil {
		return err
	}
	scope, err := d.scopeFrom(s.TableRefs)
	if err != nil {
		return err
	}
	d.scope = scope
	d.aliases = map[string]bool{}
	if s.TableRefs != nil {
		if err := d.walkJoinConds(s.TableRefs.TableRefs); err != nil {
			return err
		}
	}
	for _, a := range s.List {
		if _, _, err := d.resolve(a.Column.Table.O, a.Column.Name.O, a.Column.OriginTextPosition()); err != nil {
			return err
		}
		if err := d.resolveExpr(a.Expr, false); err != nil {
			return err
		}
	}
	if err := d.resolveExpr(s.Where, false); err != nil {
		return err
	}
	if s.Order != nil {
		for _, item := range s.Order.Items {
			if err := d.resolveExpr(item.Expr, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *describer) describeDelete(s *ast.DeleteStmt) error {
	if err := refuseWith(s.With); err != nil {
		return err
	}
	scope, err := d.scopeFrom(s.TableRefs)
	if err != nil {
		return err
	}
	d.scope = scope
	d.aliases = map[string]bool{}
	if s.TableRefs != nil {
		if err := d.walkJoinConds(s.TableRefs.TableRefs); err != nil {
			return err
		}
	}
	// A multi-table DELETE names its delete targets in s.Tables; each
	// must reference a table or alias in the FROM scope. Ignoring them
	// let `DELETE ghost FROM t1 JOIN t2 ...` describe with err=nil while
	// the server rejects the unknown target (ER_UNKNOWN_TABLE).
	if s.Tables != nil {
		for _, tn := range s.Tables.Tables {
			if tn.Schema.O != "" {
				return &dialect.NativeUnsupportedError{Pos: -1,
					Construct: "a schema-qualified DELETE target",
					Hint:      "name the target by table or alias only"}
			}
			if !d.scopeTarget(tn.Name.O) {
				return &dialect.OracleError{Pos: -1,
					Msg: fmt.Sprintf("unknown table %q in MULTI DELETE", tn.Name.O)}
			}
		}
	}
	if err := d.resolveExpr(s.Where, false); err != nil {
		return err
	}
	if s.Order != nil {
		for _, item := range s.Order.Items {
			if err := d.resolveExpr(item.Expr, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- expression walking ----------------------------------------------------

// resolveExpr resolves every column reference under expr.
// allowAliases additionally accepts select-list output names (GROUP
// BY / HAVING / ORDER BY visibility). Subqueries are refused except
// the dialect's own inert arity-0 @in emission.
func (d *describer) resolveExpr(expr ast.ExprNode, allowAliases bool) error {
	if expr == nil {
		return nil
	}
	v := &refVisitor{d: d, allowAliases: allowAliases}
	expr.Accept(v)
	return v.err
}

type refVisitor struct {
	d            *describer
	allowAliases bool
	err          error
}

func (v *refVisitor) Enter(n ast.Node) (ast.Node, bool) {
	if v.err != nil {
		return n, true
	}
	switch x := n.(type) {
	case *ast.SubqueryExpr:
		if inertSubquery(x.Query) {
			return n, true // skip its internals
		}
		v.err = &dialect.NativeUnsupportedError{Pos: x.OriginTextPosition(),
			Construct: "a subquery",
			Hint:      "flatten the query or switch to database.oracle: \"server\""}
		return n, true
	case *ast.ExistsSubqueryExpr:
		v.err = &dialect.NativeUnsupportedError{Pos: x.OriginTextPosition(),
			Construct: "an EXISTS subquery",
			Hint:      "flatten the query or switch to database.oracle: \"server\""}
		return n, true
	case *ast.ColumnNameExpr:
		if x.Name.Schema.O != "" {
			v.err = &dialect.NativeUnsupportedError{Pos: x.OriginTextPosition(),
				Construct: "a schema-qualified column reference",
				Hint:      "qualify with the table or alias only"}
			return n, true
		}
		if v.allowAliases && x.Name.Table.O == "" && v.d.aliases[x.Name.Name.O] {
			return n, true
		}
		if _, _, err := v.d.resolve(x.Name.Table.O, x.Name.Name.O, x.OriginTextPosition()); err != nil {
			if v.allowAliases && x.Name.Table.O == "" {
				// Case-insensitive alias fallback, matching MySQL.
				for a := range v.d.aliases {
					if strings.EqualFold(a, x.Name.Name.O) {
						return n, true
					}
				}
			}
			v.err = err
		}
		return n, true
	}
	return n, false
}

func (v *refVisitor) Leave(n ast.Node) (ast.Node, bool) { return n, v.err == nil }

// inertSubquery recognizes the one subquery shape the backend allows:
// the dialect's own arity-0 @in emission (`SELECT NULL FROM DUAL
// WHERE FALSE`-shaped) — no column references, no relations beyond
// DUAL. Anything else is not inert.
func inertSubquery(q ast.ResultSetNode) bool {
	sel, ok := q.(*ast.SelectStmt)
	if !ok {
		return false
	}
	if sel.From != nil {
		var rels []dialect.RelRef
		collectJoin(sel.From.TableRefs, dialect.JoinBase, false, &rels)
		for _, r := range rels {
			if !strings.EqualFold(r.Table, "dual") {
				return false
			}
		}
	}
	inert := true
	check := &inertVisitor{ok: &inert}
	sel.Accept(check)
	return inert
}

type inertVisitor struct{ ok *bool }

func (v *inertVisitor) Enter(n ast.Node) (ast.Node, bool) {
	switch n.(type) {
	case *ast.ColumnNameExpr, *ast.SubqueryExpr, *ast.ExistsSubqueryExpr:
		*v.ok = false
		return n, true
	}
	return n, false
}

func (v *inertVisitor) Leave(n ast.Node) (ast.Node, bool) { return n, *v.ok }

// ---- inputs ----------------------------------------------------------------

// colHintNativeRe mirrors the template scanner's `-- @column` syntax
// (internal/template/scanner.go); the annotation comments ride in the
// skeleton, so the rendered SQL self-carries them.
var colHintNativeRe = regexp.MustCompile(`(?m)^--\s*@column\s+([a-z][a-z0-9_]*)\s*:\s*(.+?)\s*$`)

func parseColumnHints(sql string) map[string]dialect.TypeRef {
	hints := map[string]dialect.TypeRef{}
	for _, m := range colHintNativeRe.FindAllStringSubmatch(sql, -1) {
		if tr, ok := (TypeMap{}).TypeByName(m[2]); ok {
			oid := wireNormalize(tr.OID)
			hints[m[1]] = dialect.TypeRef{OID: oid, Name: typeCodeName(oid)}
		}
	}
	return hints
}

// countPlaceholders counts '?' binds lexically (strings and comments
// excluded by the profile).
func countPlaceholders(sql string) int {
	src := []byte(sql)
	profile := Profile{}
	n, pos := 0, 0
	for {
		tok, err := profile.NextToken(src, pos)
		if err != nil || tok.Kind == dialect.KindEOF {
			return n
		}
		if tok.Kind == dialect.KindPositionalParam && tok.Text == "?" {
			n++
		}
		pos = tok.End
	}
}

func toParseOracleError(sql string, err error) error {
	pos := -1
	var pe *dialect.ParseError
	if errors.As(toParseError(sql, err), &pe) {
		pos = pe.Pos
	}
	return &dialect.OracleError{Pos: pos, Msg: err.Error()}
}
