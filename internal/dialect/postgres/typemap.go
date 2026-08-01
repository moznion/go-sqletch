package postgres

import (
	"sort"
	"strings"

	"github.com/moznion/go-sqletch/internal/dialect"
)

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

	// Array types (@in parameters and array columns).
	1000: {Name: "[]bool"},                      // _bool
	1005: {Name: "[]int16"},                     // _int2
	1007: {Name: "[]int32"},                     // _int4
	1016: {Name: "[]int64"},                     // _int8
	1009: {Name: "[]string"},                    // _text
	1014: {Name: "[]string"},                    // _bpchar
	1015: {Name: "[]string"},                    // _varchar
	1021: {Name: "[]float32"},                   // _float4
	1022: {Name: "[]float64"},                   // _float8
	1182: {Name: "[]time.Time", Import: "time"}, // _date
	1115: {Name: "[]time.Time", Import: "time"}, // _timestamp
	1185: {Name: "[]time.Time", Import: "time"}, // _timestamptz
	1231: {Name: "[]float64"},                   // _numeric
	2951: {Name: "[]string"},                    // _uuid
}

func (TypeMap) GoType(oid uint32) (dialect.GoTypeRef, bool) {
	t, ok := goTypes[oid]
	return t, ok
}

// typesByName resolves `-- @param name: sqltype` hints. Keys are
// normalized (lowercased, length arguments stripped).
var typesByName = map[string]dialect.TypeRef{
	"text":    {OID: 25, Name: "text"},
	"varchar": {OID: 1043, Name: "varchar"}, "character varying": {OID: 1043, Name: "varchar"},
	"char": {OID: 1042, Name: "bpchar"}, "bpchar": {OID: 1042, Name: "bpchar"},
	"bigint": {OID: 20, Name: "int8"}, "int8": {OID: 20, Name: "int8"},
	"integer": {OID: 23, Name: "int4"}, "int": {OID: 23, Name: "int4"}, "int4": {OID: 23, Name: "int4"},
	"smallint": {OID: 21, Name: "int2"}, "int2": {OID: 21, Name: "int2"},
	"boolean": {OID: 16, Name: "bool"}, "bool": {OID: 16, Name: "bool"},
	"timestamptz": {OID: 1184, Name: "timestamptz"}, "timestamp with time zone": {OID: 1184, Name: "timestamptz"},
	"timestamp": {OID: 1114, Name: "timestamp"}, "timestamp without time zone": {OID: 1114, Name: "timestamp"},
	"date":  {OID: 1082, Name: "date"},
	"uuid":  {OID: 2950, Name: "uuid"},
	"jsonb": {OID: 3802, Name: "jsonb"}, "json": {OID: 114, Name: "json"},
	"double precision": {OID: 701, Name: "float8"}, "float8": {OID: 701, Name: "float8"},
	"real": {OID: 700, Name: "float4"}, "float4": {OID: 700, Name: "float4"},
	"numeric": {OID: 1700, Name: "numeric"}, "decimal": {OID: 1700, Name: "numeric"},
	"bytea":  {OID: 17, Name: "bytea"},
	"text[]": {OID: 1009, Name: "_text"}, "varchar[]": {OID: 1015, Name: "_varchar"},
	"bigint[]": {OID: 1016, Name: "_int8"}, "int8[]": {OID: 1016, Name: "_int8"},
	"integer[]": {OID: 1007, Name: "_int4"}, "int[]": {OID: 1007, Name: "_int4"}, "int4[]": {OID: 1007, Name: "_int4"},
	"smallint[]": {OID: 1005, Name: "_int2"}, "int2[]": {OID: 1005, Name: "_int2"},
	"boolean[]": {OID: 1000, Name: "_bool"}, "bool[]": {OID: 1000, Name: "_bool"},
	"timestamptz[]": {OID: 1185, Name: "_timestamptz"},
	"uuid[]":        {OID: 2951, Name: "_uuid"},
	"float8[]":      {OID: 1022, Name: "_float8"}, "double precision[]": {OID: 1022, Name: "_float8"},
}

// writableNames is the reverse of typesByName: the spelling to suggest
// when a `-- @param` hint disagrees with the oracle (SQLETCH213). The
// oracle's own name for an array is `_varchar`, which is not what an
// author writes — `varchar[]` is. Built once, deterministically:
// shortest spelling wins, ties broken lexicographically.
var writableNames = func() map[uint32]string {
	out := map[uint32]string{}
	names := make([]string, 0, len(typesByName))
	for n := range typesByName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		oid := typesByName[n].OID
		if cur, ok := out[oid]; ok && len(cur) <= len(n) {
			continue
		}
		out[oid] = n
	}
	return out
}()

// WritableName returns the `-- @param` spelling for an OID, so
// diagnostics can show the compliant rewrite. False when the type has
// no annotation spelling (it can then only be inferred).
func (TypeMap) WritableName(oid uint32) (string, bool) {
	n, ok := writableNames[oid]
	return n, ok
}

// TypeByName resolves a SQL type name from a `-- @param` hint.
func (TypeMap) TypeByName(name string) (dialect.TypeRef, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	// Strip length/precision arguments: varchar(16) -> varchar.
	if i := strings.IndexByte(n, '('); i >= 0 {
		if j := strings.IndexByte(n[i:], ')'); j >= 0 {
			n = strings.TrimSpace(n[:i] + n[i+j+1:])
		}
	}
	t, ok := typesByName[n]
	return t, ok
}
