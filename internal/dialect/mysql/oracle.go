package mysql

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-mysql-org/go-mysql/client"
	gomysql "github.com/go-mysql-org/go-mysql/mysql"

	"github.com/moznion/sqletch/internal/cache"
	"github.com/moznion/sqletch/internal/dialect"
)

// Oracle is the COM_STMT_PREPARE-backed type oracle. Preparing never
// executes; result-column metadata (name, type, source table/column)
// is reliable, while parameter metadata is not typed by the protocol —
// Desc.Params entries stay zero and the pipeline fills them from the
// mandatory `-- @param` annotations.
//
// Plan prepares `EXPLAIN <sql>` and executes it with every parameter
// bound to NULL: executing an EXPLAIN plans the statement without
// touching data, which surfaces optimizer-stage errors that a bare
// prepare cannot see.
type Oracle struct {
	conn *client.Conn
	cat  *cache.Catalog // lazy snapshot for source-column resolution
}

func NewOracle(conn *client.Conn) *Oracle { return &Oracle{conn: conn} }

func (o *Oracle) Describe(ctx context.Context, sql string) (dialect.Desc, error) {
	if err := ctx.Err(); err != nil {
		return dialect.Desc{}, err
	}
	stmt, err := o.conn.Prepare(sql)
	if err != nil {
		return dialect.Desc{}, toOracleError(err)
	}
	defer func() { _ = stmt.Close() }()

	cat, err := o.catalog(ctx)
	if err != nil {
		return dialect.Desc{}, err
	}
	desc := dialect.Desc{}
	if n := stmt.ParamNum(); n > 0 {
		// Untyped by the protocol; annotation-filled downstream.
		desc.Params = make([]dialect.TypeRef, n)
	}
	fields, err := stmt.GetColumnFields()
	if err != nil {
		return dialect.Desc{}, toOracleError(err)
	}
	for _, f := range fields {
		col := dialect.ColumnDesc{Name: string(f.Name), Type: refFromField(f)}
		if len(f.OrgTable) > 0 && len(f.OrgName) > 0 {
			if tb := cat.Lookup(string(f.OrgTable)); tb != nil {
				if c := tb.Col(string(f.OrgName)); c != nil {
					col.SrcRel, col.SrcAtt = tb.OID, c.Att
				}
			}
		}
		desc.Columns = append(desc.Columns, col)
	}
	return desc, nil
}

func (o *Oracle) Plan(ctx context.Context, sql string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stmt, err := o.conn.Prepare("EXPLAIN " + sql)
	if err != nil {
		return toOracleError(err)
	}
	defer func() { _ = stmt.Close() }()
	args := make([]any, stmt.ParamNum()) // every parameter NULL
	res, err := stmt.Execute(args...)
	if err != nil {
		return toOracleError(err)
	}
	res.Close()
	return nil
}

// PlanText returns the EXPLAIN FORMAT=TREE output for explain
// --analyze, parameters bound to NULL.
func (o *Oracle) PlanText(ctx context.Context, sql string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	stmt, err := o.conn.Prepare("EXPLAIN FORMAT=TREE " + sql)
	if err != nil {
		return "", toOracleError(err)
	}
	defer func() { _ = stmt.Close() }()
	args := make([]any, stmt.ParamNum())
	res, err := stmt.Execute(args...)
	if err != nil {
		return "", toOracleError(err)
	}
	defer res.Close()
	var b strings.Builder
	for i := range res.Values {
		s, err := res.GetString(i, 0)
		if err != nil {
			return "", err
		}
		b.WriteString(s)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

const snapshotQuery = `
SELECT c.table_name,
       c.ordinal_position,
       c.column_name,
       c.column_type,
       (c.is_nullable = 'NO') AS not_null,
       (c.column_default IS NOT NULL
        OR c.extra LIKE '%auto_increment%'
        OR c.extra LIKE '%DEFAULT_GENERATED%') AS has_default
FROM information_schema.columns AS c
WHERE c.table_schema = DATABASE()
ORDER BY c.table_name, c.ordinal_position`

// Snapshot dumps the current database's columns. MySQL has no OIDs;
// tables get stable synthetic ones (1-based in table-name order, the
// query's ORDER BY), and column att numbers are ordinal positions.
func (o *Oracle) Snapshot(ctx context.Context) (*cache.Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res, err := o.conn.Execute(snapshotQuery)
	if err != nil {
		return nil, toOracleError(err)
	}
	defer res.Close()

	schema, err := o.schemaName()
	if err != nil {
		return nil, err
	}
	cat := &cache.Catalog{}
	var cur *cache.Table
	tm := TypeMap{}
	for i := range res.Values {
		tbl, err1 := res.GetString(i, 0)
		ord, err2 := res.GetInt(i, 1)
		col, err3 := res.GetString(i, 2)
		typ, err4 := res.GetString(i, 3)
		notNull, err5 := res.GetInt(i, 4)
		hasDef, err6 := res.GetInt(i, 5)
		if err := errors.Join(err1, err2, err3, err4, err5, err6); err != nil {
			return nil, fmt.Errorf("snapshot row %d: %w", i, err)
		}
		if cur == nil || cur.Name != tbl {
			cat.Tables = append(cat.Tables, cache.Table{
				Schema: schema, Name: tbl, OID: uint32(len(cat.Tables) + 1),
			})
			cur = &cat.Tables[len(cat.Tables)-1]
		}
		typOID := uint32(0)
		if tr, ok := tm.TypeByName(typ); ok {
			typOID = tr.OID
		}
		cur.Cols = append(cur.Cols, cache.Column{
			Name: col, Att: int16(ord), TypeOID: typOID, TypeName: typ,
			NotNull: notNull != 0, HasDefault: hasDef != 0,
		})
	}
	return cat, nil
}

func (o *Oracle) ServerVersion(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	res, err := o.conn.Execute("SELECT VERSION()")
	if err != nil {
		return "", toOracleError(err)
	}
	defer res.Close()
	return res.GetString(0, 0)
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

func (o *Oracle) schemaName() (string, error) {
	res, err := o.conn.Execute("SELECT DATABASE()")
	if err != nil {
		return "", toOracleError(err)
	}
	defer res.Close()
	return res.GetString(0, 0)
}

// binaryCharset is the MySQL collation id for binary data; string-ish
// columns with this charset are []byte, others are text.
const binaryCharset = 63

func refFromField(f *gomysql.Field) dialect.TypeRef {
	oid := uint32(f.Type)
	if f.Flag&gomysql.UNSIGNED_FLAG != 0 {
		oid |= FlagUnsigned
	}
	switch f.Type {
	case typeVarString, typeString, typeBlob, typeTinyBlob, typeMedumBlob, typeLongBlob:
		if f.Charset == binaryCharset {
			oid |= FlagBinary
		}
	}
	return dialect.TypeRef{OID: oid, Name: typeCodeName(oid)}
}

// typeCodeName renders an encoded type ref for diagnostics.
func typeCodeName(oid uint32) string {
	base := map[uint32]string{
		typeDecimal: "decimal", typeTiny: "tinyint", typeShort: "smallint",
		typeLong: "int", typeFloat: "float", typeDouble: "double",
		typeTimestamp: "timestamp", typeLonglong: "bigint", typeInt24: "mediumint",
		typeDate: "date", typeTime: "time", typeDatetime: "datetime",
		typeYear: "year", typeBit: "bit", typeJSON: "json",
		typeNewDecimal: "decimal", typeEnum: "enum", typeSet: "set",
		typeTinyBlob: "tinytext", typeMedumBlob: "mediumtext",
		typeLongBlob: "longtext", typeBlob: "text",
		typeVarString: "varchar", typeString: "char",
	}
	name, ok := base[oid&^(FlagUnsigned|FlagBinary)]
	if !ok {
		return fmt.Sprintf("mysql type %#x", oid)
	}
	if oid&FlagBinary != 0 {
		switch oid &^ (FlagUnsigned | FlagBinary) {
		case typeVarString:
			name = "varbinary"
		case typeString:
			name = "binary"
		case typeBlob:
			name = "blob"
		case typeTinyBlob:
			name = "tinyblob"
		case typeMedumBlob:
			name = "mediumblob"
		case typeLongBlob:
			name = "longblob"
		}
	}
	if oid&FlagUnsigned != 0 {
		name += " unsigned"
	}
	return name
}

func toOracleError(err error) error {
	if me, ok := errors.AsType[*gomysql.MyError](err); ok {
		return &dialect.OracleError{Pos: -1, SQLState: me.State, Msg: me.Message}
	}
	return &dialect.OracleError{Pos: -1, Msg: err.Error()}
}
