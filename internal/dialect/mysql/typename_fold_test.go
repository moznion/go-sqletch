package mysql

import "testing"

// Type names fold ASCII-only, as MySQL's own keyword lookup does
// (verified on MySQL 8.4: `CAST(1 AS SİGNED)`, `x İNT`, `x bİgint` are
// all ERROR 1064). strings.ToLower maps the dotted `İ` to `i`, so
// `-- @param x: bİgint` resolved to BIGINT — a type name no MySQL server
// accepts. The ASCII spellings, any case, still resolve.
func TestTypeByName_FoldsASCIIOnly(t *testing.T) {
	tm := TypeMap{}
	for _, in := range []string{"bİgint", "İNT", "bigint UNSİGNED", "DATETİME", "varcharİ"} {
		if tr, ok := tm.TypeByName(in); ok {
			t.Errorf("TypeByName(%q) = %+v, want unresolved", in, tr)
		}
	}
	for in, oid := range map[string]uint32{
		"BigInt": typeLonglong, "INT": typeLong, "bigint UNSIGNED": typeLonglong | FlagUnsigned,
	} {
		if tr, ok := tm.TypeByName(in); !ok || tr.OID != oid {
			t.Errorf("TypeByName(%q) = (%+v, %v), want OID %#x", in, tr, ok, oid)
		}
	}
}
