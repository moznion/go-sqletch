package sqlite

import (
	"testing"

	sqlite3 "github.com/ncruces/go-sqlite3"
)

// AffinityRef reproduces sqlite3AffinityType, which matches INT / CHAR /
// CLOB / TEXT / BLOB / REAL / FLOA / DOUB case-insensitively over ASCII
// ONLY (sqlite3UpperToLower). Go's case mappings are wider: ToUpper maps
// the dotless `ı` and ToLower the dotted `İ` to an ASCII `I`/`i`, so
// `ıntext` read as INTEGER where the engine gives TEXT affinity. The
// bundled engine is the ground truth: CAST under a declared type applies
// exactly that type's affinity, and the two probes tell all five apart.
func TestAffinityRef_MatchesEngine(t *testing.T) {
	conn, err := sqlite3.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// typeof(CAST('1.5' AS d)) || '/' || typeof(CAST('012' AS d))
	engineAffinity := map[string]string{
		"integer/integer": "INTEGER", "real/integer": "NUMERIC",
		"text/text": "TEXT", "real/real": "REAL", "blob/blob": "BLOB",
	}
	ours := map[uint32]string{
		TypeInteger: "INTEGER", TypeText: "TEXT", TypeBlob: "BLOB", TypeReal: "REAL",
		TypeNumeric: "NUMERIC", TypeBool: "NUMERIC", TypeTime: "NUMERIC",
	}
	for _, decl := range []string{
		"INTEGER", "int", "VARCHAR", "clob", "Text", "BLOB", "real", "Float", "DOUBLE",
		"NUMERIC", "BOOLEAN", "DATETIME", "POINT",
		// Non-ASCII spellings Go's case mappings turn into keywords.
		"ınt", "İNT", "ıntext", "İNTEXT", "pOİNT", "lıfetime",
	} {
		got := sqliteReading(conn, "SELECT typeof(CAST('1.5' AS "+decl+")) || '/' || typeof(CAST('012' AS "+decl+")) AS a")
		eng, ok := engineAffinity[got[len("a="):]]
		if !ok {
			t.Fatalf("%q: unexpected engine reading %q", decl, got)
		}
		tr, ok := AffinityRef(decl)
		if !ok || ours[tr.OID] != eng {
			t.Errorf("AffinityRef(%q) = %+v (%s affinity), engine gives %s", decl, tr, ours[tr.OID], eng)
		}
	}
}

// The carve-outs (sqletch's own, not SQLite's) match the declared type
// ASCII-case-insensitively too: `datetİme` is no DATETIME to anyone.
func TestAffinityRef_CarveOutsFoldASCIIOnly(t *testing.T) {
	for _, decl := range []string{"datetİme", "tİme", "tİmestamp", "DATETİME(3)"} {
		if tr, ok := AffinityRef(decl); !ok || tr.OID != TypeNumeric {
			t.Errorf("AffinityRef(%q) = (%+v, %v), want NUMERIC", decl, tr, ok)
		}
	}
	for _, decl := range []string{"bOOLEAN", "Date", "TimeStamp"} {
		if tr, ok := AffinityRef(decl); !ok || (tr.OID != TypeBool && tr.OID != TypeTime) {
			t.Errorf("AffinityRef(%q) = (%+v, %v), want the ASCII carve-out", decl, tr, ok)
		}
	}
	if tr, _ := AffinityRef("DATETİME"); tr.Name != "datetİme" {
		t.Errorf("resolved name = %q, want the declared type ASCII-lowered", tr.Name)
	}
}

func TestTypeByName_FoldsASCIIOnly(t *testing.T) {
	tm := TypeMap{}
	for in, want := range map[string]uint32{
		"INTEGER": TypeInteger, "ınteger": TypeNumeric, "İNT": TypeNumeric,
		"ıntext": TypeText, "Datetime(3)": TypeTime, "datetİme": TypeNumeric,
	} {
		if tr, ok := tm.TypeByName(in); !ok || tr.OID != want {
			t.Errorf("TypeByName(%q) = (%+v, %v), want code %d", in, tr, ok, want)
		}
	}
}
