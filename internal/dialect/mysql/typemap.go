package mysql

import (
	"strings"

	"github.com/moznion/go-sqletch/internal/dialect"
)

// MySQL has no OIDs; dialect.TypeRef.OID carries the wire-protocol
// type code (MYSQL_TYPE_*) plus the two flag bits below. The encoding
// is stable — it is what the committed cache stores.
const (
	// FlagUnsigned marks UNSIGNED integer/decimal columns.
	FlagUnsigned uint32 = 1 << 8
	// FlagBinary marks binary-charset string/blob columns ([]byte, not
	// string).
	FlagBinary uint32 = 1 << 9
)

// Wire-protocol type codes (subset sqletch maps).
const (
	typeDecimal    = 0x00
	typeTiny       = 0x01
	typeShort      = 0x02
	typeLong       = 0x03
	typeFloat      = 0x04
	typeDouble     = 0x05
	typeTimestamp  = 0x07
	typeLonglong   = 0x08
	typeInt24      = 0x09
	typeDate       = 0x0a
	typeTime       = 0x0b
	typeDatetime   = 0x0c
	typeYear       = 0x0d
	typeBit        = 0x10
	typeJSON       = 0xf5
	typeNewDecimal = 0xf6
	typeEnum       = 0xf7
	typeSet        = 0xf8
	typeTinyBlob   = 0xf9
	typeMedumBlob  = 0xfa
	typeLongBlob   = 0xfb
	typeBlob       = 0xfc
	typeVarString  = 0xfd
	typeString     = 0xfe
)

// TypeMap maps encoded MySQL type refs to Go types for generated
// code. Deliberately conservative: unmapped types are a compile error
// with the type name in the message — never a silent guess.
type TypeMap struct{}

var _ dialect.TypeMap = TypeMap{}

func (TypeMap) GoType(oid uint32) (dialect.GoTypeRef, bool) {
	unsigned := oid&FlagUnsigned != 0
	binary := oid&FlagBinary != 0
	switch oid &^ (FlagUnsigned | FlagBinary) {
	case typeTiny:
		// tinyint(1) is MySQL's boolean idiom, but the protocol cannot
		// distinguish it from a numeric tinyint; int8/uint8 keeps the
		// full domain (annotate a bool column as `tinyint` and compare
		// against 0/1 in Go).
		if unsigned {
			return dialect.GoTypeRef{Name: "uint8"}, true
		}
		return dialect.GoTypeRef{Name: "int8"}, true
	case typeShort, typeYear:
		if unsigned {
			return dialect.GoTypeRef{Name: "uint16"}, true
		}
		return dialect.GoTypeRef{Name: "int16"}, true
	case typeLong, typeInt24:
		if unsigned {
			return dialect.GoTypeRef{Name: "uint32"}, true
		}
		return dialect.GoTypeRef{Name: "int32"}, true
	case typeLonglong:
		if unsigned {
			return dialect.GoTypeRef{Name: "uint64"}, true
		}
		return dialect.GoTypeRef{Name: "int64"}, true
	case typeFloat:
		return dialect.GoTypeRef{Name: "float32"}, true
	case typeDouble:
		return dialect.GoTypeRef{Name: "float64"}, true
	case typeDecimal, typeNewDecimal:
		// Documented lossy mapping, same decision as PostgreSQL numeric.
		return dialect.GoTypeRef{Name: "float64"}, true
	case typeTimestamp, typeDatetime, typeDate:
		return dialect.GoTypeRef{Name: "time.Time", Import: "time"}, true
	case typeTime:
		// TIME is a duration-like value; keep the driver's textual form.
		return dialect.GoTypeRef{Name: "string"}, true
	case typeVarString, typeString, typeEnum, typeSet:
		if binary {
			return dialect.GoTypeRef{Name: "[]byte"}, true
		}
		return dialect.GoTypeRef{Name: "string"}, true
	case typeBlob, typeTinyBlob, typeMedumBlob, typeLongBlob:
		// TEXT columns arrive as BLOB codes with a text charset.
		if binary {
			return dialect.GoTypeRef{Name: "[]byte"}, true
		}
		return dialect.GoTypeRef{Name: "string"}, true
	case typeJSON:
		return dialect.GoTypeRef{Name: "[]byte"}, true
	case typeBit:
		return dialect.GoTypeRef{Name: "[]byte"}, true
	default:
		return dialect.GoTypeRef{}, false
	}
}

// typesByName resolves `-- @param name: sqltype` hints — on MySQL the
// mandatory source of parameter types. Keys are normalized
// (lowercased, length arguments stripped).
var typesByName = map[string]dialect.TypeRef{
	"tinyint":   {OID: typeTiny, Name: "tinyint"},
	"bool":      {OID: typeTiny, Name: "tinyint"},
	"boolean":   {OID: typeTiny, Name: "tinyint"},
	"smallint":  {OID: typeShort, Name: "smallint"},
	"mediumint": {OID: typeInt24, Name: "mediumint"},
	"int":       {OID: typeLong, Name: "int"},
	"integer":   {OID: typeLong, Name: "int"},
	"bigint":    {OID: typeLonglong, Name: "bigint"},
	"float":     {OID: typeFloat, Name: "float"},
	"double":    {OID: typeDouble, Name: "double"}, "double precision": {OID: typeDouble, Name: "double"},
	"decimal": {OID: typeNewDecimal, Name: "decimal"}, "numeric": {OID: typeNewDecimal, Name: "decimal"},
	"varchar": {OID: typeVarString, Name: "varchar"}, "char": {OID: typeString, Name: "char"},
	"text": {OID: typeBlob, Name: "text"}, "tinytext": {OID: typeTinyBlob, Name: "tinytext"},
	"mediumtext": {OID: typeMedumBlob, Name: "mediumtext"}, "longtext": {OID: typeLongBlob, Name: "longtext"},
	"blob":       {OID: typeBlob | FlagBinary, Name: "blob"},
	"tinyblob":   {OID: typeTinyBlob | FlagBinary, Name: "tinyblob"},
	"mediumblob": {OID: typeMedumBlob | FlagBinary, Name: "mediumblob"},
	"longblob":   {OID: typeLongBlob | FlagBinary, Name: "longblob"},
	"varbinary":  {OID: typeVarString | FlagBinary, Name: "varbinary"},
	"binary":     {OID: typeString | FlagBinary, Name: "binary"},
	"date":       {OID: typeDate, Name: "date"},
	"datetime":   {OID: typeDatetime, Name: "datetime"},
	"timestamp":  {OID: typeTimestamp, Name: "timestamp"},
	"time":       {OID: typeTime, Name: "time"},
	"year":       {OID: typeYear, Name: "year"},
	"json":       {OID: typeJSON, Name: "json"},
	"bit":        {OID: typeBit, Name: "bit"},
	"enum":       {OID: typeEnum, Name: "enum"},
	"set":        {OID: typeSet, Name: "set"},
}

// TypeByName resolves a SQL type name from a `-- @param` hint.
// "bigint unsigned" style modifiers fold into the flag bits.
func (TypeMap) TypeByName(name string) (dialect.TypeRef, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	// Strip length/precision arguments: varchar(16) -> varchar.
	if i := strings.IndexByte(n, '('); i >= 0 {
		if j := strings.IndexByte(n[i:], ')'); j >= 0 {
			n = strings.Join(strings.Fields(n[:i]+" "+n[i+j+1:]), " ")
		}
	}
	unsigned := false
	if s, ok := strings.CutSuffix(n, " unsigned"); ok {
		unsigned = true
		n = s
	}
	t, ok := typesByName[n]
	if !ok {
		return dialect.TypeRef{}, false
	}
	if unsigned {
		t.OID |= FlagUnsigned
		t.Name += " unsigned"
	}
	return t, ok
}
