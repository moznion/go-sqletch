package sqlite

import "testing"

func TestAffinityRef(t *testing.T) {
	tests := map[string]uint32{
		"INTEGER":          TypeInteger,
		"bigint":           TypeInteger,
		"INT":              TypeInteger,
		"UNSIGNED BIG INT": TypeInteger,
		"VARCHAR(255)":     TypeText,
		"TEXT":             TypeText,
		"CLOB":             TypeText,
		"BLOB":             TypeBlob,
		"REAL":             TypeReal,
		"DOUBLE PRECISION": TypeReal,
		"FLOAT":            TypeReal,
		"BOOLEAN":          TypeBool,
		"DATETIME":         TypeTime,
		"DATE":             TypeTime,
		"TIMESTAMP":        TypeTime,
		"NUMERIC":          TypeNumeric,
		"DECIMAL(10,5)":    TypeNumeric,
		// The famous affinity gotcha: POINT contains INT.
		"POINT": TypeInteger,
	}
	for in, code := range tests {
		tr, ok := AffinityRef(in)
		if !ok || tr.OID != code {
			t.Errorf("AffinityRef(%q) = (%+v, %v), want code %d", in, tr, ok, code)
		}
	}
	if _, ok := AffinityRef(""); ok {
		t.Error("empty decltype (expression column) must not resolve")
	}
}

// The BOOL / DATE / TIME carve-outs are sqletch's addition on top of
// SQLite's affinity rules, and they must match the WHOLE declared type.
// Substring matching swept in any declaration that merely contained the
// word — "LIFETIME" became time.Time, "CANDIDATE" became time.Time —
// and the generated field then failed to scan the integer actually
// stored there. SQLite itself gives all of these NUMERIC affinity.
func TestAffinityRef_CarveOutsMatchWholeName(t *testing.T) {
	carved := map[string]uint32{
		"BOOL": TypeBool, "BOOLEAN": TypeBool,
		"DATE": TypeTime, "DATETIME": TypeTime, "DATETIME(3)": TypeTime,
		"TIMESTAMP": TypeTime, "TIMESTAMPTZ": TypeTime, "TIME": TypeTime,
	}
	for in, code := range carved {
		if tr, ok := AffinityRef(in); !ok || tr.OID != code {
			t.Errorf("AffinityRef(%q) = (%+v, %v), want code %d", in, tr, ok, code)
		}
	}

	// Merely containing the word is not enough: SQLite's own rules make
	// these NUMERIC, and so must we.
	for _, in := range []string{"LIFETIME", "RUNTIME", "CANDIDATE", "BOOLFLAGS", "UPDATED"} {
		tr, ok := AffinityRef(in)
		if !ok || tr.OID != TypeNumeric {
			t.Errorf("AffinityRef(%q) = (%+v, %v), want NUMERIC (code %d)", in, tr, ok, TypeNumeric)
		}
	}

	// The affinity rules proper still win over the carve-outs, and they
	// ARE substring rules — that part is SQLite's own behaviour.
	for in, code := range map[string]uint32{
		"POINT": TypeInteger, "DATEINT": TypeInteger, "TIMETEXT": TypeText,
	} {
		if tr, ok := AffinityRef(in); !ok || tr.OID != code {
			t.Errorf("AffinityRef(%q) = (%+v, %v), want code %d", in, tr, ok, code)
		}
	}

	// The resolved name stays the declared type verbatim (lowercased):
	// it is what diagnostics and the catalog show.
	if tr, _ := AffinityRef("DATETIME(3)"); tr.Name != "datetime(3)" {
		t.Errorf("resolved name = %q, want the declared type", tr.Name)
	}
}

func TestGoTypeAndTypeByName(t *testing.T) {
	tm := TypeMap{}
	for name, want := range map[string]string{
		"integer":     "int64",
		"varchar(16)": "string",
		"blob":        "[]byte",
		"real":        "float64",
		"boolean":     "bool",
		"datetime":    "time.Time",
		"decimal":     "float64",
	} {
		tr, ok := tm.TypeByName(name)
		if !ok {
			t.Errorf("TypeByName(%q) failed", name)
			continue
		}
		gt, ok := tm.GoType(tr.OID)
		if !ok || gt.Name != want {
			t.Errorf("%s -> %+v -> (%+v, %v), want %s", name, tr, gt, ok, want)
		}
	}
	if _, ok := tm.GoType(999); ok {
		t.Error("unknown code must not resolve")
	}
}
