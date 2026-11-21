package mysql

import "testing"

// Type hints trim and split on ASCII whitespace only, like MySQL's
// lexer: `bigint<U+00A0>unsigned` is one (unknown) word to the server,
// though strings.Fields split it into BIGINT UNSIGNED.
func TestTypeByName_ASCIIWhitespaceOnly(t *testing.T) {
	tm := TypeMap{}
	for _, in := range []string{"bigint\u00a0unsigned", "bigint\u00a0", "int(11)\u0085unsigned"} {
		if tr, ok := tm.TypeByName(in); ok {
			t.Errorf("TypeByName(%q) = %+v, want unresolved", in, tr)
		}
	}
	for in, oid := range map[string]uint32{" bigint\tunsigned ": typeLonglong | FlagUnsigned, "int(11)  unsigned": typeLong | FlagUnsigned} {
		if tr, ok := tm.TypeByName(in); !ok || tr.OID != oid {
			t.Errorf("TypeByName(%q) = (%+v, %v), want OID %#x", in, tr, ok, oid)
		}
	}
}
