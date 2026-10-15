package dialect

import (
	"reflect"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/cache"
)

// An all-built-in entry must serialize exactly as before design 26
// added the user-defined-type fields: no committed PostgreSQL entry and
// no MySQL/SQLite entry may change bytes (design 26 §4.2).
func TestEntryFromDesc_BuiltinBytesUnchanged(t *testing.T) {
	d := Desc{
		Params:  []TypeRef{{OID: 20, Name: "int8"}},
		Columns: []ColumnDesc{{Name: "id", Type: TypeRef{OID: 20, Name: "int8"}, SrcRel: 16390, SrcAtt: 1}},
	}
	got, err := cache.EncodeOracle(EntryFromDesc("fp", "SELECT 1", d))
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "format": 2,
  "schema_fp": "fp",
  "rendered_sql": "SELECT 1",
  "params": [
    {
      "oid": 20,
      "name": "int8"
    }
  ],
  "columns": [
    {
      "name": "id",
      "oid": 20,
      "type_name": "int8",
      "src_rel": 16390,
      "src_att": 1
    }
  ]
}
`
	if string(got) != want {
		t.Fatalf("built-in entry bytes changed:\n%s\nwant:\n%s", got, want)
	}
}

func TestEntryDescRoundTrip_UserDefinedTypes(t *testing.T) {
	enum := &TypeExt{Schema: "public", Kind: TypeKindEnum, Labels: []string{"QUEUED", "STARTED"}}
	d := Desc{
		Params: []TypeRef{
			{OID: 16400, Name: "event_kind", Ext: enum},
			{OID: 25, Name: "text"},
		},
		Columns: []ColumnDesc{
			{Name: "kind", Type: TypeRef{OID: 16400, Name: "event_kind", Ext: enum}, SrcRel: 16410, SrcAtt: 2},
			{Name: "kinds", Type: TypeRef{OID: 16399, Name: "_event_kind", Ext: &TypeExt{Schema: "public", Kind: "b"}}},
		},
	}
	e := EntryFromDesc("fp", "SELECT", d)
	data, err := cache.EncodeOracle(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{`"kind": "e"`, `"labels": [`, `"type_kind": "e"`, `"type_labels": [`, `"type_kind": "b"`} {
		if !strings.Contains(string(data), frag) {
			t.Errorf("entry lacks %s:\n%s", frag, data)
		}
	}
	if back := DescFromEntry(e); !reflect.DeepEqual(back, d) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", back, d)
	}
}
