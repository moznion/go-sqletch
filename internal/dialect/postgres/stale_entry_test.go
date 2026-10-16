package postgres

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/cache"
)

// Design 26 D4: an entry naming a user-defined type without a recorded
// kind was written before Describe recorded kinds, and must be a miss.
func TestStaleEntry(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    cache.OracleEntry
		want bool
	}{
		{"all built-in", cache.OracleEntry{
			Params:  []cache.EntryType{{OID: 20, Name: "int8"}},
			Columns: []cache.EntryColumn{{Name: "id", OID: 20, TypeName: "int8", SrcRel: 16390, SrcAtt: 1}},
		}, false},
		{"described enum param and column", cache.OracleEntry{
			Params:  []cache.EntryType{{OID: 16400, Name: "event_kind", Schema: "public", Kind: "e", Labels: []string{"A"}}},
			Columns: []cache.EntryColumn{{Name: "k", OID: 16400, TypeName: "event_kind", TypeSchema: "public", TypeKind: "e", TypeLabels: []string{"A"}}},
		}, false},
		{"described non-enum user type", cache.OracleEntry{
			Columns: []cache.EntryColumn{{Name: "ks", OID: 16399, TypeName: "_event_kind", TypeSchema: "public", TypeKind: "b"}},
		}, false},
		{"pre-design enum param", cache.OracleEntry{
			Params: []cache.EntryType{{OID: 16400, Name: "event_kind"}},
		}, true},
		{"pre-design enum column", cache.OracleEntry{
			Params:  []cache.EntryType{{OID: 25, Name: "text"}},
			Columns: []cache.EntryColumn{{Name: "k", OID: 16400, TypeName: "event_kind"}},
		}, true},
		{"boundary: first user OID", cache.OracleEntry{
			Params: []cache.EntryType{{OID: firstNormalObjectID, Name: "t"}},
		}, true},
		{"boundary: last built-in OID", cache.OracleEntry{
			Params: []cache.EntryType{{OID: firstNormalObjectID - 1, Name: "t"}},
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StaleEntry(&tc.e); got != tc.want {
				t.Fatalf("StaleEntry = %v, want %v", got, tc.want)
			}
		})
	}
}
