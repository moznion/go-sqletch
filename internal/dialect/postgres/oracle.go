package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/dialect"
)

// Oracle is the server-backed PostgreSQL type oracle: it lets the
// database itself be the type checker via the extended protocol's
// Parse/Describe. See docs/design/04-type-oracle.md.
type Oracle struct {
	conn *pgx.Conn
	seq  atomic.Uint64
}

var _ dialect.Oracle = (*Oracle)(nil)

func NewOracle(conn *pgx.Conn) *Oracle { return &Oracle{conn: conn} }

func (o *Oracle) Describe(ctx context.Context, sql string) (dialect.Desc, error) {
	name := fmt.Sprintf("sqletch_describe_%d", o.seq.Add(1))
	sd, err := o.conn.Prepare(ctx, name, sql)
	if err != nil {
		return dialect.Desc{}, mapOracleErr(err)
	}
	defer func() { _ = o.conn.Deallocate(ctx, name) }()

	desc := dialect.Desc{}
	var oids []uint32
	oids = append(oids, sd.ParamOIDs...)
	for _, f := range sd.Fields {
		oids = append(oids, f.DataTypeOID)
	}
	names, err := o.typeNames(ctx, oids)
	if err != nil {
		return dialect.Desc{}, err
	}
	for _, oid := range sd.ParamOIDs {
		desc.Params = append(desc.Params, dialect.TypeRef{OID: oid, Name: names[oid]})
	}
	for _, f := range sd.Fields {
		desc.Columns = append(desc.Columns, dialect.ColumnDesc{
			Name:   string(f.Name),
			Type:   dialect.TypeRef{OID: f.DataTypeOID, Name: names[f.DataTypeOID]},
			SrcRel: f.TableOID,
			SrcAtt: int16(f.TableAttributeNumber),
		})
	}
	return desc, nil
}

// Plan surfaces planner-stage errors invisible to prepare. GENERIC_PLAN
// (PostgreSQL 16+) plans a parameterized statement without values —
// exactly the shape-verification need.
func (o *Oracle) Plan(ctx context.Context, sql string) error {
	if _, err := o.conn.Exec(ctx, "EXPLAIN (GENERIC_PLAN) "+sql); err != nil {
		return mapOracleErr(err)
	}
	return nil
}

// PlanText returns the planner's textual output for a shape — the
// `explain --analyze` payload. It goes through pgconn's raw simple
// query: GENERIC_PLAN takes bare $n placeholders without values, and
// pgx's higher-level paths would demand arguments for them.
func (o *Oracle) PlanText(ctx context.Context, sql string) (string, error) {
	results, err := o.conn.PgConn().Exec(ctx, "EXPLAIN (GENERIC_PLAN) "+sql).ReadAll()
	if err != nil {
		return "", mapOracleErr(err)
	}
	var b strings.Builder
	for _, res := range results {
		if res.Err != nil {
			return "", mapOracleErr(res.Err)
		}
		for _, row := range res.Rows {
			if len(row) > 0 {
				b.Write(row[0])
				b.WriteByte('\n')
			}
		}
	}
	return b.String(), nil
}

const snapshotQuery = `
SELECT n.nspname, c.relname, c.oid,
       (c.relhassubclass AND c.relkind = 'r') AS has_children,
       a.attname, a.attnum, a.atttypid, t.typname, a.attnotnull, a.atthasdef
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = c.oid
JOIN pg_type t ON t.oid = a.atttypid
WHERE c.relkind IN ('r', 'v', 'm', 'p')
  AND a.attnum > 0 AND NOT a.attisdropped
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
ORDER BY n.nspname, c.relname, a.attnum`

func (o *Oracle) Snapshot(ctx context.Context) (*cache.Catalog, error) {
	rows, err := o.conn.Query(ctx, snapshotQuery)
	if err != nil {
		return nil, mapOracleErr(err)
	}
	defer rows.Close()

	cat := &cache.Catalog{}
	var cur *cache.Table
	for rows.Next() {
		var schema, rel, col, typ string
		var oid, typOID uint32
		var att int16
		var hasChildren, notNull, hasDef bool
		if err := rows.Scan(&schema, &rel, &oid, &hasChildren, &col, &att, &typOID, &typ, &notNull, &hasDef); err != nil {
			return nil, err
		}
		if cur == nil || cur.OID != oid {
			cat.Tables = append(cat.Tables, cache.Table{
				Schema: schema, Name: rel, OID: oid, HasChildren: hasChildren,
			})
			cur = &cat.Tables[len(cat.Tables)-1]
		}
		cur.Cols = append(cur.Cols, cache.Column{
			Name: col, Att: att, TypeOID: typOID, TypeName: typ,
			NotNull: notNull, HasDefault: hasDef,
		})
	}
	return cat, rows.Err()
}

func (o *Oracle) ServerVersion(ctx context.Context) (string, error) {
	var v string
	if err := o.conn.QueryRow(ctx, "SHOW server_version").Scan(&v); err != nil {
		return "", err
	}
	return v, nil
}

func (o *Oracle) typeNames(ctx context.Context, oids []uint32) (map[uint32]string, error) {
	out := map[uint32]string{}
	if len(oids) == 0 {
		return out, nil
	}
	rows, err := o.conn.Query(ctx,
		"SELECT oid, typname FROM pg_type WHERE oid = ANY($1)", oids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var name string
		if err := rows.Scan(&oid, &name); err != nil {
			return nil, err
		}
		out[oid] = name
	}
	return out, rows.Err()
}

func mapOracleErr(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	pos := -1
	if pgErr.Position > 0 {
		// Position is 1-based; PostgreSQL counts characters, which for
		// sqletch renderings (predominantly ASCII SQL) coincides with
		// bytes closely enough for diagnostics.
		pos = int(pgErr.Position) - 1
	}
	return &dialect.OracleError{
		Pos:      pos,
		SQLState: pgErr.Code,
		Msg:      pgErr.Message,
		// 42P18 = indeterminate_datatype ("could not determine data
		// type of parameter"); 42725 = ambiguous_function/operator
		// (e.g. unknown + unknown). Both share the same remedy: an
		// explicit cast on the parameter (SQLETCH201's hint).
		Indeterminate: pgErr.Code == "42P18" || pgErr.Code == "42725",
	}
}
