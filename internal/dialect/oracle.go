package dialect

import (
	"context"
	"errors"
	"fmt"

	"github.com/moznion/go-sqletch/internal/cache"
)

// TypeRef identifies a database type.
type TypeRef struct {
	OID  uint32
	Name string
	// Ext describes a user-defined type (design 26 §4.1). Only the
	// PostgreSQL oracle sets it — for EVERY OID >= 16384 and for no
	// built-in — so "user-defined and no Ext" can only be an oracle
	// entry written before Ext existed (D4).
	Ext *TypeExt
}

// TypeExt is the oracle's description of a user-defined type.
type TypeExt struct {
	Schema string
	// Kind is pg_type.typtype: "e" enum, "b" base (an array of an enum,
	// an extension type), "d" domain, "c" composite, "r" range, …
	Kind string
	// Labels are an enum's labels in enumsortorder; empty otherwise.
	Labels []string
}

// TypeKindEnum is TypeExt.Kind for an enum.
const TypeKindEnum = "e"

// IsEnum reports whether t is a user-defined enum the oracle described.
func (t TypeRef) IsEnum() bool { return t.Ext != nil && t.Ext.Kind == TypeKindEnum }

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

// ShiftOracleErrPos returns err with an *OracleError's Pos moved left by
// delta bytes. The plan oracles measure error positions against an
// EXPLAIN-wrapped string (a delta-byte prefix + the rendering), but
// pipeline diagnostics map Pos straight through the rendering's source
// map, so the prefix must be stripped for the position to be
// rendering-relative like the Describe path's. A Pos landing inside the
// stripped prefix, or a non-positional error, is reported unpositioned
// (Pos = -1) rather than mis-attributed; a non-*OracleError is returned
// unchanged.
func ShiftOracleErrPos(err error, delta int) error {
	var oe *OracleError
	if !errors.As(err, &oe) || oe.Pos < 0 {
		return err
	}
	shifted := *oe
	if oe.Pos < delta {
		shifted.Pos = -1
	} else {
		shifted.Pos = oe.Pos - delta
	}
	return &shifted
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
