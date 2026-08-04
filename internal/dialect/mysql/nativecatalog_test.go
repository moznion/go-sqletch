package mysql

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/cache"
)

func buildOne(t *testing.T, ddl string) *cache.Catalog {
	t.Helper()
	cat, err := BuildCatalog([]cache.SchemaFile{{Path: "s.sql", Content: []byte(ddl)}})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	return cat
}

func col(t *testing.T, cat *cache.Catalog, table, name string) cache.Column {
	t.Helper()
	tb := cat.Lookup(table)
	if tb == nil {
		t.Fatalf("no table %q", table)
	}
	c := tb.Col(name)
	if c == nil {
		t.Fatalf("no column %s.%s", table, name)
	}
	return *c
}

func TestBuildCatalogColumnFacts(t *testing.T) {
	cat := buildOne(t, `
CREATE TABLE t (
    id         BIGINT AUTO_INCREMENT PRIMARY KEY,
    email      VARCHAR(255) NOT NULL,
    flag       TINYINT(1) NOT NULL DEFAULT 0,
    note       TEXT,
    body       BLOB,
    price      DECIMAL(8,2) NOT NULL,
    plain_dec  DECIMAL NOT NULL,
    counted    INT UNSIGNED NOT NULL,
    nick       VARCHAR(64) NULL,
    dropped_at TIMESTAMP(6) NULL DEFAULT NULL,
    updated_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)
);`)

	for _, tt := range []struct {
		name       string
		typeName   string
		notNull    bool
		hasDefault bool
		oid        uint32
	}{
		{"id", "bigint", true, true, typeLonglong},
		{"email", "varchar(255)", true, false, typeVarString},
		{"flag", "tinyint(1)", true, true, typeTiny},
		{"note", "text", false, false, typeBlob},
		{"body", "blob", false, false, typeBlob | FlagBinary},
		{"price", "decimal(8,2)", true, false, typeNewDecimal},
		{"plain_dec", "decimal(10,0)", true, false, typeNewDecimal},
		{"counted", "int unsigned", true, false, typeLong | FlagUnsigned},
		{"nick", "varchar(64)", false, false, typeVarString},
		{"dropped_at", "timestamp(6)", false, false, typeTimestamp},
		{"updated_at", "timestamp(6)", true, true, typeTimestamp},
	} {
		c := col(t, cat, "t", tt.name)
		if c.TypeName != tt.typeName || c.NotNull != tt.notNull || c.HasDefault != tt.hasDefault || c.TypeOID != tt.oid {
			t.Errorf("%s: got (type %q, oid %#x, notnull %v, default %v), want (%q, %#x, %v, %v)",
				tt.name, c.TypeName, c.TypeOID, c.NotNull, c.HasDefault,
				tt.typeName, tt.oid, tt.notNull, tt.hasDefault)
		}
	}
}

func TestBuildCatalogTablePrimaryKeyAndOrder(t *testing.T) {
	cat := buildOne(t, `
CREATE TABLE zebra (a BIGINT, b BIGINT, PRIMARY KEY (a, b));
CREATE TABLE alpha (x BIGINT);
`)
	if len(cat.Tables) != 2 || cat.Tables[0].Name != "alpha" || cat.Tables[1].Name != "zebra" {
		t.Fatalf("tables must be name-ordered: %+v", cat.Tables)
	}
	if cat.Tables[0].OID != 1 || cat.Tables[1].OID != 2 {
		t.Fatalf("OIDs must be 1-based in table order")
	}
	for _, name := range []string{"a", "b"} {
		if c := col(t, cat, "zebra", name); !c.NotNull {
			t.Errorf("PK column %s must be NOT NULL", name)
		}
	}
	if a := col(t, cat, "zebra", "a"); a.Att != 1 {
		t.Errorf("att must be the definition ordinal, got %d", a.Att)
	}
	if b := col(t, cat, "zebra", "b"); b.Att != 2 {
		t.Errorf("att must be the definition ordinal, got %d", b.Att)
	}
	if s := cat.Tables[0].Schema; s != CatalogSchemaName {
		t.Errorf("schema name must be %q, got %q", CatalogSchemaName, s)
	}
}

func TestBuildCatalogDropAndRecreate(t *testing.T) {
	cat := buildOne(t, `
DROP TABLE IF EXISTS t;
CREATE TABLE t (id BIGINT);
DROP TABLE t;
CREATE TABLE t (id BIGINT, extra TEXT);
SET NAMES utf8mb4;
`)
	if len(cat.Tables) != 1 || len(cat.Tables[0].Cols) != 2 {
		t.Fatalf("recreate must win: %+v", cat.Tables)
	}
}

func TestBuildCatalogRejections(t *testing.T) {
	for _, tt := range []struct {
		name, ddl, want string
	}{
		{"alter", "CREATE TABLE t (id BIGINT);\nALTER TABLE t ADD COLUMN x BIGINT;", "ALTER TABLE"},
		{"view", "CREATE TABLE t (id BIGINT);\nCREATE VIEW v AS SELECT id FROM t;", "CREATE VIEW"},
		{"generated", "CREATE TABLE t (id BIGINT, d BIGINT AS (id + 1));", "generated column"},
		{"like", "CREATE TABLE t (id BIGINT);\nCREATE TABLE u LIKE t;", "LIKE"},
		{"ctas", "CREATE TABLE t (id BIGINT);\nCREATE TABLE u AS SELECT id FROM t;", "AS SELECT"},
		{"temporary", "CREATE TEMPORARY TABLE t (id BIGINT);", "TEMPORARY"},
		{"duplicate", "CREATE TABLE t (id BIGINT);\nCREATE TABLE t (id BIGINT);", "created twice"},
		{"drop unknown", "DROP TABLE nope;", "no such table"},
		{"unparsable", "CREATE TABLE t (id BIGINT;", "unparsable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildCatalog([]cache.SchemaFile{{Path: "s.sql", Content: []byte(tt.ddl)}})
			if err == nil {
				t.Fatal("want an UnsupportedDDLError")
			}
			ue, ok := err.(*UnsupportedDDLError)
			if !ok {
				t.Fatalf("want *UnsupportedDDLError, got %T: %v", err, err)
			}
			if !strings.Contains(ue.Msg, tt.want) {
				t.Errorf("message %q should mention %q", ue.Msg, tt.want)
			}
			if ue.File != "s.sql" {
				t.Errorf("error must carry the schema file, got %q", ue.File)
			}
			if tt.name == "alter" && ue.Pos <= 0 {
				t.Errorf("second-statement rejection should carry a nonzero offset, got %d", ue.Pos)
			}
		})
	}
}

// TestBuildCatalogDeterminism: byte-identical output for identical
// inputs, including map-ordered internals.
func TestBuildCatalogDeterminism(t *testing.T) {
	ddl := `
CREATE TABLE b (x BIGINT);
CREATE TABLE a (y VARCHAR(10) NOT NULL);
CREATE TABLE c (z TEXT, w BIGINT NOT NULL DEFAULT 7);
`
	first, err := cache.EncodeCatalog(withFP(buildOne(t, ddl)))
	if err != nil {
		t.Fatal(err)
	}
	for range 16 {
		again, err := cache.EncodeCatalog(withFP(buildOne(t, ddl)))
		if err != nil {
			t.Fatal(err)
		}
		if string(first) != string(again) {
			t.Fatal("BuildCatalog output is not deterministic")
		}
	}
}

func withFP(cat *cache.Catalog) *cache.Catalog {
	cat.SchemaFP = "deadbeefdeadbeefdeadbeefdeadbeef"
	return cat
}
