package mysql

import "testing"

func TestGoType(t *testing.T) {
	tm := TypeMap{}
	tests := []struct {
		oid  uint32
		want string
	}{
		{typeLonglong, "int64"},
		{typeLonglong | FlagUnsigned, "uint64"},
		{typeLong, "int32"},
		{typeTiny, "int8"},
		{typeVarString, "string"},
		{typeVarString | FlagBinary, "[]byte"},
		{typeBlob, "string"},              // TEXT: blob code, text charset
		{typeBlob | FlagBinary, "[]byte"}, // real BLOB
		{typeDatetime, "time.Time"},
		{typeNewDecimal, "float64"},
		{typeJSON, "[]byte"},
	}
	for _, tt := range tests {
		got, ok := tm.GoType(tt.oid)
		if !ok || got.Name != tt.want {
			t.Errorf("GoType(%#x) = (%+v, %v), want %s", tt.oid, got, ok, tt.want)
		}
	}
	if _, ok := tm.GoType(0xffff); ok {
		t.Error("unknown type code must not resolve")
	}
}

func TestTypeByName(t *testing.T) {
	tm := TypeMap{}
	tests := map[string]uint32{
		"bigint":              typeLonglong,
		"BIGINT UNSIGNED":     typeLonglong | FlagUnsigned,
		"bigint(20) unsigned": typeLonglong | FlagUnsigned,
		"varchar(255)":        typeVarString,
		"text":                typeBlob,
		"varbinary(16)":       typeVarString | FlagBinary,
		"datetime":            typeDatetime,
		"int":                 typeLong,
		"json":                typeJSON,
	}
	for in, oid := range tests {
		tr, ok := tm.TypeByName(in)
		if !ok || tr.OID != oid {
			t.Errorf("TypeByName(%q) = (%+v, %v), want OID %#x", in, tr, ok, oid)
		}
	}
	if _, ok := tm.TypeByName("weirdtype"); ok {
		t.Error("unknown type must not resolve")
	}
	if tr, _ := tm.TypeByName("bigint unsigned"); tr.Name != "bigint unsigned" {
		t.Errorf("unsigned name = %q", tr.Name)
	}
}
