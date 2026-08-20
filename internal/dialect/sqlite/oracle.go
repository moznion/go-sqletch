package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sqlite3 "github.com/ncruces/go-sqlite3"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/dialect"
)

// Oracle is the sqlite3_prepare-backed type oracle over the in-process
// engine (ncruces/go-sqlite3: the real SQLite compiled to WASM, run
// under wazero — pure Go, nothing external). Preparing compiles the
// statement through SQLite's planner, so prepare alone is both the
// parse and the plan check; EXPLAIN QUERY PLAN adds the human-readable
// plan. Result columns are typed by declared type through the affinity
// rules; expression columns have no declared type (SQLite returns
// NULL) and stay zero-typed — the pipeline fills them from mandatory
// `-- @column` annotations. Parameter slots are never typed by SQLite;
// `-- @param` annotations are mandatory (Tier 2).
type Oracle struct {
	conn *sqlite3.Conn
	cat  *cache.Catalog // lazy snapshot for source-column resolution
}

func NewOracle(conn *sqlite3.Conn) *Oracle { return &Oracle{conn: conn} }

// prepareOne prepares sql and rejects trailing statements. A tail made
// up only of whitespace, extra semicolons, and comments is legal: the
// template scanner preserves post-`;` comments in the skeleton, so a
// rendering like `SELECT 1; -- note` must not be read as two statements.
func (o *Oracle) prepareOne(sql string) (*sqlite3.Stmt, error) {
	stmt, tail, err := o.conn.Prepare(sql)
	if err != nil {
		return nil, toOracleError(sql, err)
	}
	if tailHasStatement(tail) {
		_ = stmt.Close()
		return nil, &dialect.OracleError{Pos: -1, Msg: "multiple statements in one query"}
	}
	return stmt, nil
}

// tailHasStatement reports whether tail carries anything beyond
// whitespace, semicolons, and comments — i.e. a genuine second
// statement. A tail that fails to lex cleanly is treated conservatively
// as a statement (rejected), preserving the strict behaviour for
// pathological input while accepting ordinary trailing comments.
func tailHasStatement(tail string) bool {
	src := []byte(tail)
	prof := Profile{}
	pos := 0
	for {
		tok, err := prof.NextToken(src, pos)
		if err != nil {
			return true
		}
		if tok.Kind == dialect.KindEOF {
			return false
		}
		pos = tok.End
		switch tok.Kind {
		case dialect.KindWhitespace, dialect.KindLineComment,
			dialect.KindBlockComment, dialect.KindSemicolon:
			continue
		}
		return true
	}
}

func (o *Oracle) Describe(ctx context.Context, sql string) (dialect.Desc, error) {
	if err := ctx.Err(); err != nil {
		return dialect.Desc{}, err
	}
	stmt, err := o.prepareOne(sql)
	if err != nil {
		return dialect.Desc{}, err
	}
	defer func() { _ = stmt.Close() }()

	cat, err := o.catalog(ctx)
	if err != nil {
		return dialect.Desc{}, err
	}
	desc := dialect.Desc{}
	if n := stmt.BindCount(); n > 0 {
		// Untyped by SQLite; annotation-filled downstream.
		desc.Params = make([]dialect.TypeRef, n)
	}
	for i := 0; i < stmt.ColumnCount(); i++ {
		col := dialect.ColumnDesc{Name: stmt.ColumnName(i)}
		if tr, ok := AffinityRef(stmt.ColumnDeclType(i)); ok {
			col.Type = tr
		}
		if tbl, org := stmt.ColumnTableName(i), stmt.ColumnOriginName(i); tbl != "" && org != "" {
			if t := cat.Lookup(tbl); t != nil {
				if c := t.Col(org); c != nil {
					col.SrcRel, col.SrcAtt = t.OID, c.Att
				}
			}
		}
		desc.Columns = append(desc.Columns, col)
	}
	return desc, nil
}

// Plan prepares EXPLAIN QUERY PLAN and steps it: preparing already
// runs SQLite's planner, stepping surfaces any late errors. Unbound
// parameters are NULL by SQLite's rules — no data is touched.
func (o *Oracle) Plan(ctx context.Context, sql string) error {
	_, err := o.planRows(ctx, sql)
	return err
}

// PlanText returns the EXPLAIN QUERY PLAN detail rows.
func (o *Oracle) PlanText(ctx context.Context, sql string) (string, error) {
	rows, err := o.planRows(ctx, sql)
	if err != nil {
		return "", err
	}
	return strings.Join(rows, "\n") + "\n", nil
}

// explainQueryPlanPrefix wraps a rendering for EXPLAIN QUERY PLAN. SQLite
// error offsets are relative to the prepared (prefixed) string, so
// plan-stage diagnostics subtract its length to become rendering-relative
// (see dialect.ShiftOracleErrPos).
const explainQueryPlanPrefix = "EXPLAIN QUERY PLAN "

func (o *Oracle) planRows(ctx context.Context, sql string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prefixed := explainQueryPlanPrefix + sql
	stmt, err := o.prepareOne(prefixed)
	if err != nil {
		return nil, dialect.ShiftOracleErrPos(err, len(explainQueryPlanPrefix))
	}
	defer func() { _ = stmt.Close() }()
	var rows []string
	for stmt.Step() {
		// id, parent, notused, detail
		rows = append(rows, stmt.ColumnText(3))
	}
	if err := stmt.Err(); err != nil {
		// The statement was prepared from the prefixed string, so the
		// error offset is measured against it; strip the prefix so the
		// span lands in the rendering.
		return nil, dialect.ShiftOracleErrPos(toOracleError(prefixed, err), len(explainQueryPlanPrefix))
	}
	return rows, nil
}

// Snapshot dumps the main database's tables. SQLite has no OIDs;
// tables get stable synthetic ones (1-based in name order), and column
// att numbers are pragma cid+1.
func (o *Oracle) Snapshot(ctx context.Context) (*cache.Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rels, err := o.relations()
	if err != nil {
		return nil, err
	}
	cat := &cache.Catalog{}
	for _, rel := range rels {
		tbl := cache.Table{Schema: "main", Name: rel.name, OID: uint32(len(cat.Tables) + 1), IsView: rel.isView}
		if err := o.tableColumns(rel.name, &tbl); err != nil {
			return nil, err
		}
		cat.Tables = append(cat.Tables, tbl)
	}
	return cat, nil
}

// relInfo is one queryable relation and whether it is a view. A view's
// result columns are attributed THROUGH to base tables by SQLite, so
// the nullability analyzer must know which relations are views (see
// cache.Table.IsView).
type relInfo struct {
	name   string
	isView bool
}

func (o *Oracle) relations() ([]relInfo, error) {
	stmt, err := o.prepareOne(
		"SELECT name, type FROM sqlite_master WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer func() { _ = stmt.Close() }()
	var rels []relInfo
	for stmt.Step() {
		rels = append(rels, relInfo{name: stmt.ColumnText(0), isView: stmt.ColumnText(1) == "view"})
	}
	return rels, stmt.Err()
}

func (o *Oracle) tableColumns(table string, out *cache.Table) error {
	// Only a genuine rowid alias gets the implicit-NOT-NULL and
	// auto-assigned (defaulted) treatment below, and `pragma table_info`
	// alone cannot tell one apart. `INTEGER PRIMARY KEY DESC` is NOT a
	// rowid alias (SQLite datatype3 §2.4.2 — DESC disables the alias),
	// and `table_info.pk` is the 1-based position WITHIN the primary
	// key, so the first INTEGER column of a composite `PRIMARY KEY(a,b)`
	// also reports pk==1. Both would be wrongly narrowed to NOT NULL —
	// SQLite lets a non-alias PRIMARY KEY column hold NULL — and a
	// WITHOUT ROWID table has no auto rowid, so HasDefault must stay
	// false or an INSERT that omits the column verifies offline yet
	// fails NOT NULL on the engine.
	//
	// The reliable discriminator: SQLite materializes every one of those
	// non-alias primary keys as a real `origin='pk'` index (a rowid
	// alias never gets one). So a column is a rowid alias iff it is a
	// single INTEGER PRIMARY KEY column (pk==1, INTEGER affinity) AND the
	// table has no `origin='pk'` index — which at once excludes DESC,
	// composite, and WITHOUT ROWID primary keys.
	hasPKIndex, err := o.hasPrimaryKeyIndex(table)
	if err != nil {
		return err
	}
	stmt, err := o.prepareOne(`SELECT name, type, "notnull", dflt_value IS NOT NULL, pk FROM pragma_table_info(?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	if err := stmt.BindText(1, table); err != nil {
		return err
	}
	att := int16(0)
	for stmt.Step() {
		att++
		name := stmt.ColumnText(0)
		declType := stmt.ColumnText(1)
		notNull := stmt.ColumnInt(2) != 0
		hasDefault := stmt.ColumnInt(3) != 0
		pk := stmt.ColumnInt(4)
		typOID := uint32(0)
		if tr, ok := AffinityRef(declType); ok {
			typOID = tr.OID
		}
		// A single INTEGER PRIMARY KEY in a rowid table is the rowid
		// alias: implicitly NOT NULL and auto-assigned (counts as
		// defaulted). The pk index guards against DESC/composite/WITHOUT
		// ROWID false positives (see the discriminator above).
		rowidAlias := pk == 1 && typOID == TypeInteger && !hasPKIndex
		out.Cols = append(out.Cols, cache.Column{
			Name: name, Att: att, TypeOID: typOID, TypeName: declType,
			NotNull:    notNull || rowidAlias,
			HasDefault: hasDefault || rowidAlias,
		})
	}
	return stmt.Err()
}

// hasPrimaryKeyIndex reports whether the table has a materialized
// `origin='pk'` index. SQLite creates one for every primary key that is
// NOT a rowid alias — a composite key, a WITHOUT ROWID key, a DESC
// single key, or a non-INTEGER single key — and never for the rowid
// alias itself, so its absence is the signal that a single INTEGER
// PRIMARY KEY really is the rowid.
func (o *Oracle) hasPrimaryKeyIndex(table string) (bool, error) {
	stmt, err := o.prepareOne(`SELECT origin FROM pragma_index_list(?)`)
	if err != nil {
		return false, err
	}
	defer func() { _ = stmt.Close() }()
	if err := stmt.BindText(1, table); err != nil {
		return false, err
	}
	for stmt.Step() {
		if stmt.ColumnText(0) == "pk" {
			return true, stmt.Err()
		}
	}
	return false, stmt.Err()
}

func (o *Oracle) ServerVersion(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	stmt, err := o.prepareOne("SELECT sqlite_version()")
	if err != nil {
		return "", err
	}
	defer func() { _ = stmt.Close() }()
	if !stmt.Step() {
		return "", fmt.Errorf("sqlite_version returned no row: %w", stmt.Err())
	}
	return stmt.ColumnText(0), nil
}

func (o *Oracle) catalog(ctx context.Context) (*cache.Catalog, error) {
	if o.cat == nil {
		cat, err := o.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		o.cat = cat
	}
	return o.cat, nil
}

func toOracleError(sql string, err error) error {
	if se, ok := errors.AsType[*sqlite3.Error](err); ok {
		pos := -1
		// Error.SQL() is the query text FROM the error offset.
		if tail := se.SQL(); tail != "" && strings.HasSuffix(sql, tail) {
			pos = len(sql) - len(tail)
		}
		return &dialect.OracleError{Pos: pos, Msg: se.Error()}
	}
	return &dialect.OracleError{Pos: -1, Msg: err.Error()}
}
