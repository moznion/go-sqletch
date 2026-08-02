package dialect

import (
	"context"
	"fmt"

	"github.com/moznion/go-sqletch/internal/cache"
)

// TypeRef identifies a database type.
type TypeRef struct {
	OID  uint32
	Name string
}

// ColumnDesc describes one result column. SrcRel/SrcAtt tie a direct
// column reference back to its catalog column (0 when the column is a
// computed expression) — the nullability analysis keys on this.
type ColumnDesc struct {
	Name   string
	Type   TypeRef
	SrcRel uint32
	SrcAtt int16
}

// Desc is the oracle's answer for one rendering.
type Desc struct {
	Params  []TypeRef // by placeholder position ($1 = [0])
	Columns []ColumnDesc
}

// OracleError reports a prepare/describe failure. Pos is a byte offset
// into the described SQL (-1 when unknown). Indeterminate is set for
// "could not determine data type of parameter" failures — the CLI
// attaches the explicit-cast hint (SQLETCH201).
type OracleError struct {
	Pos           int
	SQLState      string
	Msg           string
	Indeterminate bool
}

func (e *OracleError) Error() string {
	return fmt.Sprintf("oracle error (%s) at %d: %s", e.SQLState, e.Pos, e.Msg)
}

// NativeUnsupportedError is a native-backend refusal: the input is
// outside the backend's modeled subset (design 15 §2 — refuse, never
// guess). Distinct from OracleError, which mirrors a rejection the
// real engine would also make; the CLI maps this to SQLETCH214 and
// the differential harness treats it as the tolerable direction only.
type NativeUnsupportedError struct {
	Pos       int    // byte offset into the described SQL (-1 unknown)
	Construct string // what was refused, human-readable
	Hint      string // the compliant rewrite, if one exists
}

func (e *NativeUnsupportedError) Error() string {
	return fmt.Sprintf("native oracle: %s is outside the modeled subset", e.Construct)
}

// GoTypeRef is a Go type for a database type, plus the import it needs
// ("" for builtins).
type GoTypeRef struct {
	Name   string
	Import string
}

// TypeMap translates database types to Go types for codegen.
type TypeMap interface {
	GoType(oid uint32) (GoTypeRef, bool)
}

// Oracle is the type oracle: it answers what the database itself knows
// about a rendering. Backends (server, embedded engine, native
// inference) implement the same interface — see the Oracle backends
// section of docs/spec.md.
type Oracle interface {
	// Describe prepares (never executes) sql and reports parameter and
	// result column types.
	Describe(ctx context.Context, sql string) (Desc, error)
	// Plan runs the dialect's plan-only statement (EXPLAIN) to surface
	// planner-stage errors that prepare cannot see.
	Plan(ctx context.Context, sql string) error
	// Snapshot dumps the catalog portions needed for offline analysis.
	Snapshot(ctx context.Context) (*cache.Catalog, error)
	ServerVersion(ctx context.Context) (string, error)
}
