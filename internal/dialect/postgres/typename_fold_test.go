package postgres

import "testing"

// PostgreSQL downcases an unquoted type name over ASCII only (verified
// on PG 16: `SELECT NULL::İnt4` is "type "İnt4" does not exist", while
// `NULL::INT4` is int4). strings.ToLower maps `İ` to `i`, so
// `-- @param x: İnt4` resolved to int4.
func TestTypeByName_FoldsASCIIOnly(t *testing.T) {
	tm := TypeMap{}
	for _, in := range []string{"İnt4", "BİGINT", "TEXTİ", "varcharİ(3)"} {
		if tr, ok := tm.TypeByName(in); ok {
			t.Errorf("TypeByName(%q) = %+v, want unresolved", in, tr)
		}
	}
	for _, in := range []string{"INT4", "BigInt", "Text", "VARCHAR(16)"} {
		if _, ok := tm.TypeByName(in); !ok {
			t.Errorf("TypeByName(%q) unresolved", in)
		}
	}
}
