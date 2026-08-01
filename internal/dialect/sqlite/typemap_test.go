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
