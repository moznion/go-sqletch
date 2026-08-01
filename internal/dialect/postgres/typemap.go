package postgres

import "github.com/moznion/sqletch/internal/dialect"

// TypeMap maps PostgreSQL type OIDs to Go types for generated code.
// Deliberately conservative: unmapped types are a compile error
// (SQLETCH311) with the type name in the message — never a silent
// guess. Extensions require checking that pgx can Scan/Encode the Go
// type for that OID.
type TypeMap struct{}

var _ dialect.TypeMap = TypeMap{}

var goTypes = map[uint32]dialect.GoTypeRef{
	16:   {Name: "bool"},                      // bool
	17:   {Name: "[]byte"},                    // bytea
	18:   {Name: "string"},                    // char
	19:   {Name: "string"},                    // name
	20:   {Name: "int64"},                     // int8
	21:   {Name: "int16"},                     // int2
	23:   {Name: "int32"},                     // int4
	25:   {Name: "string"},                    // text
	26:   {Name: "uint32"},                    // oid
	114:  {Name: "[]byte"},                    // json
	700:  {Name: "float32"},                   // float4
	701:  {Name: "float64"},                   // float8
	1042: {Name: "string"},                    // bpchar
	1043: {Name: "string"},                    // varchar
	1082: {Name: "time.Time", Import: "time"}, // date
	1114: {Name: "time.Time", Import: "time"}, // timestamp
	1184: {Name: "time.Time", Import: "time"}, // timestamptz
	1700: {Name: "float64"},                   // numeric (documented lossy mapping)
	2950: {Name: "string"},                    // uuid
	3802: {Name: "[]byte"},                    // jsonb
}

func (TypeMap) GoType(oid uint32) (dialect.GoTypeRef, bool) {
	t, ok := goTypes[oid]
	return t, ok
}
