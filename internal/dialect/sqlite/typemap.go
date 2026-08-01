package sqlite

import (
	"strings"

	"github.com/moznion/sqletch/internal/dialect"
)

// SQLite has no OIDs and no static column types — only declared types
// and affinity rules. dialect.TypeRef.OID carries one of the small
// codes below; TypeRef.Name keeps the normalized declared type for
// diagnostics. The encoding is stable — it is what the committed
// cache stores.
const (
	TypeInteger uint32 = 1
	TypeReal    uint32 = 2
	TypeText    uint32 = 3
	TypeBlob    uint32 = 4
	TypeNumeric uint32 = 5
	TypeBool    uint32 = 6
	TypeTime    uint32 = 7
)

// TypeMap maps encoded SQLite type refs to Go types for generated
// code.
type TypeMap struct{}

var _ dialect.TypeMap = TypeMap{}

func (TypeMap) GoType(oid uint32) (dialect.GoTypeRef, bool) {
	switch oid {
	case TypeInteger:
		return dialect.GoTypeRef{Name: "int64"}, true
	case TypeReal:
		return dialect.GoTypeRef{Name: "float64"}, true
	case TypeText:
		return dialect.GoTypeRef{Name: "string"}, true
	case TypeBlob:
		return dialect.GoTypeRef{Name: "[]byte"}, true
	case TypeNumeric:
		// Documented lossy mapping, same decision as numeric elsewhere.
		return dialect.GoTypeRef{Name: "float64"}, true
	case TypeBool:
		return dialect.GoTypeRef{Name: "bool"}, true
	case TypeTime:
		return dialect.GoTypeRef{Name: "time.Time", Import: "time"}, true
	default:
		return dialect.GoTypeRef{}, false
	}
}

// AffinityRef classifies a declared column type per SQLite's affinity
// rules (https://sqlite.org/datatype3.html §3.1), with deliberate
// carve-outs for the conventional BOOLEAN and date/time declarations
// (numeric affinity in SQLite, but bool / time.Time is what Go callers
// want; drivers convert on scan). ok is false only for an empty
// declared type — an expression column, which needs a `-- @column`
// annotation.
func AffinityRef(decltype string) (dialect.TypeRef, bool) {
	d := strings.ToLower(strings.TrimSpace(decltype))
	if d == "" {
		return dialect.TypeRef{}, false
	}
	u := strings.ToUpper(d)
	ref := func(code uint32) (dialect.TypeRef, bool) {
		return dialect.TypeRef{OID: code, Name: d}, true
	}
	switch {
	case strings.Contains(u, "INT"):
		return ref(TypeInteger)
	case strings.Contains(u, "CHAR"), strings.Contains(u, "CLOB"), strings.Contains(u, "TEXT"):
		return ref(TypeText)
	case strings.Contains(u, "BLOB"):
		return ref(TypeBlob)
	case strings.Contains(u, "REAL"), strings.Contains(u, "FLOA"), strings.Contains(u, "DOUB"):
		return ref(TypeReal)
	case strings.Contains(u, "BOOL"):
		return ref(TypeBool)
	case strings.Contains(u, "DATE"), strings.Contains(u, "TIME"):
		return ref(TypeTime)
	default:
		return ref(TypeNumeric)
	}
}

// TypeByName resolves a `-- @param name: type` / `-- @column name:
// type` annotation — on SQLite the mandatory source of parameter
// types and of expression-column types. Length arguments are
// stripped; resolution then follows the affinity rules, so any
// declarable SQLite type name works.
func (TypeMap) TypeByName(name string) (dialect.TypeRef, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if i := strings.IndexByte(n, '('); i >= 0 {
		if j := strings.IndexByte(n[i:], ')'); j >= 0 {
			n = strings.Join(strings.Fields(n[:i]+" "+n[i+j+1:]), " ")
		}
	}
	if n == "" {
		return dialect.TypeRef{}, false
	}
	return AffinityRef(n)
}
