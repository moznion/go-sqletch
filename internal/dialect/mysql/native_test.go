package mysql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/dialect"
)

const nativeTestDDL = `
CREATE TABLE users (
    id        BIGINT AUTO_INCREMENT PRIMARY KEY,
    email     VARCHAR(255) NOT NULL,
    org_id    BIGINT,
    mood      ENUM('ok','meh') NOT NULL
);
CREATE TABLE orgs (
    id   BIGINT PRIMARY KEY,
    name VARCHAR(64) NOT NULL
);
CREATE TABLE dupes (
    email VARCHAR(255) NOT NULL
);
CREATE TABLE wirey (
    y  YEAR,
    b  BIT(5),
    tt TINYTEXT,
    mb MEDIUMBLOB
);
`

func nativeOracle(t *testing.T) *NativeOracle {
	t.Helper()
	o, err := NewNativeOracle([]cache.SchemaFile{{Path: "s.sql", Content: []byte(nativeTestDDL)}}, "8.4")
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func describe(t *testing.T, sql string) (dialect.Desc, error) {
	t.Helper()
	return nativeOracle(t).Describe(context.Background(), sql)
}

func TestNativeDescribeDirectColumns(t *testing.T) {
	desc, err := describe(t, `SELECT u.id, u.email AS mail, org_id FROM users AS u WHERE u.id = ? LIMIT ?`)
	if err != nil {
		t.Fatal(err)
	}
	if len(desc.Params) != 2 {
		t.Fatalf("want 2 zero param slots, got %d", len(desc.Params))
	}
	for _, p := range desc.Params {
		if p.OID != 0 || p.Name != "" {
			t.Fatalf("param slots must stay zero (annotation-filled downstream): %+v", p)
		}
	}
	want := []dialect.ColumnDesc{
		{Name: "id", Type: dialect.TypeRef{OID: typeLonglong, Name: "bigint"}, SrcRel: 3, SrcAtt: 1},
		{Name: "mail", Type: dialect.TypeRef{OID: typeVarString, Name: "varchar"}, SrcRel: 3, SrcAtt: 2},
		{Name: "org_id", Type: dialect.TypeRef{OID: typeLonglong, Name: "bigint"}, SrcRel: 3, SrcAtt: 3},
	}
	if len(desc.Columns) != len(want) {
		t.Fatalf("want %d columns, got %+v", len(want), desc.Columns)
	}
	for i, w := range want {
		if desc.Columns[i] != w {
			t.Errorf("column %d: got %+v, want %+v", i, desc.Columns[i], w)
		}
	}
}

func TestNativeDescribeStarExpansion(t *testing.T) {
	desc, err := describe(t, `SELECT o.*, u.id FROM users AS u JOIN orgs AS o ON o.id = u.org_id`)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(desc.Columns))
	for _, c := range desc.Columns {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, ","); got != "id,name,id" {
		t.Fatalf("star must expand in catalog att order: %s", got)
	}
	if desc.Columns[0].SrcRel != desc.Columns[1].SrcRel {
		t.Fatal("o.* columns must share the orgs source relation")
	}
}

// TestNativeDescribeWireNormalization pins the protocol quirks the
// differential gate discovered: TEXT/BLOB flavors collapse to the
// BLOB wire code, and YEAR/BIT carry the UNSIGNED flag.
func TestNativeDescribeWireNormalization(t *testing.T) {
	desc, err := describe(t, "SELECT w.y, w.b, w.tt, w.mb FROM wirey AS w")
	if err != nil {
		t.Fatal(err)
	}
	want := []dialect.TypeRef{
		{OID: typeYear | FlagUnsigned, Name: "year unsigned"},
		{OID: typeBit | FlagUnsigned, Name: "bit unsigned"},
		{OID: typeBlob, Name: "text"},
		{OID: typeBlob | FlagBinary, Name: "blob"},
	}
	for i, w := range want {
		if desc.Columns[i].Type != w {
			t.Errorf("column %d: got %+v, want %+v", i, desc.Columns[i].Type, w)
		}
	}
}

func TestNativeDescribeExpressionColumns(t *testing.T) {
	// Hinted and aliased: accepted, type from the annotation.
	sql := "-- @column total: bigint\nSELECT count(*) AS total FROM users"
	desc, err := describe(t, sql)
	if err != nil {
		t.Fatal(err)
	}
	if len(desc.Columns) != 1 || desc.Columns[0] != (dialect.ColumnDesc{
		Name: "total", Type: dialect.TypeRef{OID: typeLonglong, Name: "bigint"},
	}) {
		t.Fatalf("hinted expression column: %+v", desc.Columns)
	}
}

// TestNativeDescribeInferredAggregates pins D3b widening #1: COUNT
// and MIN/MAX over a direct column need no annotation; the inferred
// type matches what the wire would report, and SrcRel/SrcAtt stay
// zero (a computed field, like the server says).
func TestNativeDescribeInferredAggregates(t *testing.T) {
	sql := "SELECT count(*) AS n, count(DISTINCT u.email) AS d, max(u.id) AS mx, min(u.email) AS mn FROM users AS u"
	desc, err := describe(t, sql)
	if err != nil {
		t.Fatal(err)
	}
	want := []dialect.ColumnDesc{
		{Name: "n", Type: dialect.TypeRef{OID: typeLonglong, Name: "bigint"}},
		{Name: "d", Type: dialect.TypeRef{OID: typeLonglong, Name: "bigint"}},
		{Name: "mx", Type: dialect.TypeRef{OID: typeLonglong, Name: "bigint"}},
		{Name: "mn", Type: dialect.TypeRef{OID: typeVarString, Name: "varchar"}},
	}
	for i, w := range want {
		if desc.Columns[i] != w {
			t.Errorf("column %d: got %+v, want %+v", i, desc.Columns[i], w)
		}
	}
	// The aggregate's argument must still resolve.
	if _, err := describe(t, "SELECT max(u.ghost) AS g FROM users AS u"); err == nil {
		t.Fatal("aggregate over an unknown column must be rejected")
	}
	// A wire-normalized argument type flows through the inference.
	desc, err = describe(t, "SELECT max(w.tt) AS t1, min(w.y) AS y1 FROM wirey AS w")
	if err != nil {
		t.Fatal(err)
	}
	if desc.Columns[0].Type != (dialect.TypeRef{OID: typeBlob, Name: "text"}) ||
		desc.Columns[1].Type != (dialect.TypeRef{OID: typeYear | FlagUnsigned, Name: "year unsigned"}) {
		t.Fatalf("wire normalization must flow through aggregates: %+v", desc.Columns)
	}
}

func TestNativeDescribeDML(t *testing.T) {
	for _, sql := range []string{
		"INSERT INTO users (email, org_id) VALUES (?, ?)",
		"INSERT INTO users SET email = ?",
		"INSERT INTO users (email) VALUES (?) ON DUPLICATE KEY UPDATE email = ?",
		"UPDATE users AS u SET u.email = ? WHERE u.id = ?",
		"DELETE FROM users WHERE id = ?",
	} {
		desc, err := describe(t, sql)
		if err != nil {
			t.Errorf("%s: %v", sql, err)
			continue
		}
		if len(desc.Columns) != 0 {
			t.Errorf("%s: DML must describe no columns", sql)
		}
	}
}

func TestNativeDescribeEngineParityRejections(t *testing.T) {
	// These are rejections the real engine would also make: they must
	// surface as OracleError (SQLETCH202-class), not as refusals.
	for _, tt := range []struct{ name, sql, want string }{
		{"unknown table", "SELECT id FROM missing", "doesn't exist"},
		{"unknown column", "SELECT nope FROM users", "unknown column"},
		{"unknown qualified", "SELECT u.nope FROM users AS u", "unknown column"},
		{"unknown qualifier", "SELECT x.id FROM users AS u", "unknown table"},
		{"table name hidden by alias", "SELECT users.id FROM users AS u", "unknown table"},
		{"ambiguous", "SELECT email FROM users, dupes", "ambiguous"},
		{"where unknown", "SELECT u.id FROM users AS u WHERE ghost = ?", "unknown column"},
		{"insert arity", "INSERT INTO users (email, org_id) VALUES (?)", "doesn't match"},
		{"insert unknown col", "INSERT INTO users (ghost) VALUES (?)", "unknown column"},
		{"unparsable", "SELECT FROM WHERE", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := describe(t, tt.sql)
			var oe *dialect.OracleError
			if !errors.As(err, &oe) {
				t.Fatalf("want *dialect.OracleError, got %T: %v", err, err)
			}
			if !strings.Contains(oe.Msg, tt.want) {
				t.Errorf("message %q should mention %q", oe.Msg, tt.want)
			}
		})
	}
}

func TestNativeDescribeSubsetRefusals(t *testing.T) {
	// These are deliberate subset exclusions: they must surface as
	// NativeUnsupportedError (SQLETCH214), never as engine errors and
	// never as guesses.
	for _, tt := range []struct{ name, sql, want string }{
		{"derived table", "SELECT x.id FROM (SELECT id FROM users) AS x", "derived table"},
		{"scalar subquery", "SELECT (SELECT max(id) FROM users) AS m FROM users", "subquery"},
		{"exists subquery", "SELECT u.id FROM users AS u WHERE EXISTS (SELECT 1 FROM orgs)", "EXISTS"},
		{"in subquery", "SELECT u.id FROM users AS u WHERE u.org_id IN (SELECT id FROM orgs)", "subquery"},
		{"unaliased expression", "SELECT count(*) FROM users", "AS alias"},
		{"unhinted uninferred expression", "SELECT sum(id) AS s FROM users", "@column"},
		{"min over enum", "SELECT min(mood) AS m FROM users", "ENUM/SET"},
		{"enum projection", "SELECT mood FROM users", "ENUM/SET"},
		{"enum via star", "SELECT * FROM users", "ENUM/SET"},
		{"other statement", "SHOW TABLES", "SELECT/INSERT/UPDATE/DELETE"},
		{"insert select", "INSERT INTO users (email) SELECT name FROM orgs", "INSERT ... SELECT"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := describe(t, tt.sql)
			var ne *dialect.NativeUnsupportedError
			if !errors.As(err, &ne) {
				t.Fatalf("want *dialect.NativeUnsupportedError, got %T: %v", err, err)
			}
			if !strings.Contains(ne.Error()+ne.Construct+ne.Hint, tt.want) {
				t.Errorf("refusal %q/%q should mention %q", ne.Construct, ne.Hint, tt.want)
			}
		})
	}
}

func TestNativeDescribeInertInSubquery(t *testing.T) {
	// The dialect's own arity-0 @in emission must pass: it is pinned
	// skeleton text (dialect.InEmptySQL), not an authored subquery.
	sql := "SELECT u.id FROM users AS u WHERE u.email IN (SELECT NULL FROM DUAL WHERE FALSE)"
	if _, err := describe(t, sql); err != nil {
		t.Fatalf("arity-0 @in emission must be accepted: %v", err)
	}
}

func TestNativeDescribeAliasVisibility(t *testing.T) {
	// Output aliases are visible to GROUP BY / HAVING / ORDER BY,
	// like MySQL's own resolution.
	sql := "-- @column total: bigint\n" +
		"SELECT org_id AS grp, count(*) AS total FROM users GROUP BY grp HAVING total > ? ORDER BY total DESC"
	if _, err := describe(t, sql); err != nil {
		t.Fatalf("alias resolution in GROUP BY/HAVING/ORDER BY: %v", err)
	}
	// But not in WHERE (the server rejects it there too).
	sql = "-- @column total: bigint\nSELECT count(*) AS total FROM users WHERE total > ?"
	var oe *dialect.OracleError
	if _, err := describe(t, sql); !errors.As(err, &oe) {
		t.Fatalf("aliases must not resolve in WHERE, got %v", err)
	}
}

func TestNativeDescribeQuestionCountsIgnoreStringsAndComments(t *testing.T) {
	desc, err := describe(t, "SELECT u.id FROM users AS u WHERE u.email = '?' -- ? in comment\n  AND u.id = ?")
	if err != nil {
		t.Fatal(err)
	}
	if len(desc.Params) != 1 {
		t.Fatalf("placeholders in strings/comments must not count: got %d", len(desc.Params))
	}
}

func TestNativeServerVersionIsThePin(t *testing.T) {
	v, err := nativeOracle(t).ServerVersion(context.Background())
	if err != nil || v != "8.4" {
		t.Fatalf("ServerVersion must return the pin verbatim, got %q, %v", v, err)
	}
}
