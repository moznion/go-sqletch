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
	types, err := o.typeRefs(ctx, oids)
	if err != nil {
		return dialect.Desc{}, err
	}
	for _, oid := range sd.ParamOIDs {
		desc.Params = append(desc.Params, types[oid].ref(oid))
	}
	for _, f := range sd.Fields {
		desc.Columns = append(desc.Columns, dialect.ColumnDesc{
			Name:   string(f.Name),
			Type:   types[f.DataTypeOID].ref(f.DataTypeOID),
			SrcRel: f.TableOID,
			SrcAtt: int16(f.TableAttributeNumber),
		})
	}
	return desc, nil
}

// genericPlanPrefix wraps a rendering for GENERIC_PLAN. Error positions
// PostgreSQL reports are relative to this whole string, so plan-stage
// diagnostics subtract its length to become rendering-relative (see
// dialect.ShiftOracleErrPos).
const genericPlanPrefix = "EXPLAIN (GENERIC_PLAN) "

// Plan surfaces planner-stage errors invisible to prepare. GENERIC_PLAN
// (PostgreSQL 16+) plans a parameterized statement without values —
// exactly the shape-verification need.
func (o *Oracle) Plan(ctx context.Context, sql string) error {
	if _, err := o.conn.Exec(ctx, genericPlanPrefix+sql); err != nil {
		return dialect.ShiftOracleErrPos(mapOracleErr(err), len(genericPlanPrefix))
	}
	return nil
}

// PlanText returns the planner's textual output for a shape — the
// `explain --analyze` payload. It goes through pgconn's raw simple
// query: GENERIC_PLAN takes bare $n placeholders without values, and
// pgx's higher-level paths would demand arguments for them.
func (o *Oracle) PlanText(ctx context.Context, sql string) (string, error) {
	results, err := o.conn.PgConn().Exec(ctx, genericPlanPrefix+sql).ReadAll()
	if err != nil {
		return "", dialect.ShiftOracleErrPos(mapOracleErr(err), len(genericPlanPrefix))
	}
	var b strings.Builder
	for _, res := range results {
		if res.Err != nil {
			return "", dialect.ShiftOracleErrPos(mapOracleErr(res.Err), len(genericPlanPrefix))
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
       (c.relkind IN ('v', 'm')) AS is_view,
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
		var hasChildren, isView, notNull, hasDef bool
		if err := rows.Scan(&schema, &rel, &oid, &hasChildren, &isView, &col, &att, &typOID, &typ, &notNull, &hasDef); err != nil {
			return nil, err
		}
		if cur == nil || cur.OID != oid {
			cat.Tables = append(cat.Tables, cache.Table{
				Schema: schema, Name: rel, OID: oid, HasChildren: hasChildren, IsView: isView,
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

// firstNormalObjectID is PostgreSQL's FirstNormalObjectId: every OID
// below it is assigned at initdb (built-in), every OID at or above it to
// a user-defined object — extension types included.
const firstNormalObjectID = 16384

// typeRefsQuery resolves the oracle's type OIDs to names and, for
// user-defined types, their kind and enum labels (design 26 §4.1).
const typeRefsQuery = `
SELECT t.oid, t.typname, n.nspname, t.typtype::text,
       array(SELECT e.enumlabel::text FROM pg_enum e
             WHERE e.enumtypid = t.oid ORDER BY e.enumsortorder)
FROM pg_type t
JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE t.oid = ANY($1)`

// StaleEntry reports whether a committed oracle entry predates design
// 26: it names a user-defined type but records no kind. Describe records
// a kind for EVERY user-defined OID, so that combination cannot come
// from a current run; reading the entry anyway would make an enum look
// unmapped (SQLETCH311) instead of being re-described (D4). The caller
// treats a stale entry as a miss.
func StaleEntry(e *cache.OracleEntry) bool {
	for _, p := range e.Params {
		if p.OID >= firstNormalObjectID && p.Kind == "" {
			return true
		}
	}
	for _, c := range e.Columns {
		if c.OID >= firstNormalObjectID && c.TypeKind == "" {
			return true
		}
	}
	return false
}

type typeInfo struct {
	name, schema, kind string
	labels             []string
}

// ref builds the TypeRef for oid. Ext is attached for EVERY user-defined
// type and never for a built-in, keyed on the OID range rather than on
// TypeMap membership, so what a cold Describe records does not depend
// on the static Go mapping table (design 26 §4.1, D4).
func (ti typeInfo) ref(oid uint32) dialect.TypeRef {
	tr := dialect.TypeRef{OID: oid, Name: ti.name}
	if oid >= firstNormalObjectID && ti.kind != "" {
		tr.Ext = &dialect.TypeExt{Schema: ti.schema, Kind: ti.kind}
		if ti.kind == dialect.TypeKindEnum {
			tr.Ext.Labels = ti.labels
		}
	}
	return tr
}

func (o *Oracle) typeRefs(ctx context.Context, oids []uint32) (map[uint32]typeInfo, error) {
	out := map[uint32]typeInfo{}
	if len(oids) == 0 {
		return out, nil
	}
	rows, err := o.conn.Query(ctx, typeRefsQuery, oids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var ti typeInfo
		if err := rows.Scan(&oid, &ti.name, &ti.schema, &ti.kind, &ti.labels); err != nil {
			return nil, err
		}
		out[oid] = ti
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
		// bytes closely enough for diagnostics — a multibyte rendering
		// can still drift the span by the number of multibyte runes left
		// of the error (right span, right verdict). Plan-stage callers
		// wrap the rendering in an all-ASCII EXPLAIN prefix and strip its
		// length via dialect.ShiftOracleErrPos, so the prefix never
		// contributes drift.
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
