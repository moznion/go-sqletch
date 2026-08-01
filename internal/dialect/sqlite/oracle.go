package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sqlite3 "github.com/ncruces/go-sqlite3"

	"github.com/moznion/sqletch/internal/cache"
	"github.com/moznion/sqletch/internal/dialect"
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

// prepareOne prepares sql and rejects trailing statements.
func (o *Oracle) prepareOne(sql string) (*sqlite3.Stmt, error) {
	stmt, tail, err := o.conn.Prepare(sql)
	if err != nil {
		return nil, toOracleError(sql, err)
	}
	if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(tail), ";")) != "" {
		_ = stmt.Close()
		return nil, &dialect.OracleError{Pos: -1, Msg: "multiple statements in one query"}
	}
	return stmt, nil
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

func (o *Oracle) planRows(ctx context.Context, sql string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stmt, err := o.prepareOne("EXPLAIN QUERY PLAN " + sql)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stmt.Close() }()
	var rows []string
	for stmt.Step() {
		// id, parent, notused, detail
		rows = append(rows, stmt.ColumnText(3))
	}
	if err := stmt.Err(); err != nil {
		return nil, toOracleError(sql, err)
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
	names, err := o.tableNames()
	if err != nil {
		return nil, err
	}
	cat := &cache.Catalog{}
	for _, name := range names {
		tbl := cache.Table{Schema: "main", Name: name, OID: uint32(len(cat.Tables) + 1)}
		if err := o.tableColumns(name, &tbl); err != nil {
			return nil, err
		}
		cat.Tables = append(cat.Tables, tbl)
	}
	return cat, nil
}

func (o *Oracle) tableNames() ([]string, error) {
	stmt, err := o.prepareOne(
		"SELECT name FROM sqlite_master WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer func() { _ = stmt.Close() }()
	var names []string
	for stmt.Step() {
		names = append(names, stmt.ColumnText(0))
	}
	return names, stmt.Err()
}

func (o *Oracle) tableColumns(table string, out *cache.Table) error {
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
		// INTEGER PRIMARY KEY is the rowid alias: implicitly NOT NULL
		// and auto-assigned (counts as defaulted).
		rowidAlias := pk == 1 && typOID == TypeInteger
		out.Cols = append(out.Cols, cache.Column{
			Name: name, Att: att, TypeOID: typOID, TypeName: declType,
			NotNull:    notNull || rowidAlias,
			HasDefault: hasDefault || rowidAlias,
		})
	}
	return stmt.Err()
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
