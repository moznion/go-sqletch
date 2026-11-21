package postgres

import "testing"

// Type hints trim and split on ASCII whitespace only, like the server's
// scanner: U+00A0 / U+0085 are not whitespace to PostgreSQL (PG 16:
// `NULL::int4<U+00A0>` is an unknown type), though strings.TrimSpace
// strips them.
func TestTypeByName_ASCIIWhitespaceOnly(t *testing.T) {
	tm := TypeMap{}
	for _, in := range []string{"int4\u00a0", "\u0085text", "varchar(3)\u00a0"} {
		if tr, ok := tm.TypeByName(in); ok {
			t.Errorf("TypeByName(%q) = %+v, want unresolved", in, tr)
		}
	}
	for _, in := range []string{" int4\t", "\nvarchar(3) "} {
		if _, ok := tm.TypeByName(in); !ok {
			t.Errorf("TypeByName(%q) unresolved", in)
		}
	}
}
