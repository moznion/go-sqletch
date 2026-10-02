package rules

import (
	"slices"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/template"
)

// PostgreSQL type OIDs used by the fixtures.
const (
	pgInt8    = 20
	pgInt4    = 23
	pgText    = 25
	pgFloat8  = 701
	pgNumeric = 1700
	pgDate    = 1082
	pgTstamp  = 1114
)

func mysqlType(t *testing.T, name string) dialect.TypeRef {
	t.Helper()
	tr, ok := mysql.TypeMap{}.TypeByName(name)
	if !ok {
		t.Fatalf("mysql type %q", name)
	}
	return tr
}

func typesCatalog(t *testing.T, dialectName string) *cache.Catalog {
	t.Helper()
	switch dialectName {
	case "postgres":
		return &cache.Catalog{Tables: []cache.Table{
			{Schema: "public", Name: "users", OID: 1, Cols: []cache.Column{
				{Name: "id", TypeOID: pgInt8, TypeName: "int8"},
				{Name: "age", TypeOID: pgInt4, TypeName: "int4"},
				{Name: "email", TypeOID: pgText, TypeName: "text"},
				{Name: "born", TypeOID: pgDate, TypeName: "date"},
				{Name: "score", TypeOID: pgFloat8, TypeName: "float8"},
			}},
			{Schema: "public", Name: "orgs", OID: 2, Cols: []cache.Column{
				{Name: "id", TypeOID: pgInt8, TypeName: "int8"},
			}},
		}}
	case "mysql":
		vc, bi, tx := mysqlType(t, "varchar"), mysqlType(t, "bigint"), mysqlType(t, "text")
		return &cache.Catalog{Tables: []cache.Table{
			{Schema: "app", Name: "users", OID: 1, Cols: []cache.Column{
				{Name: "id", TypeOID: bi.OID, TypeName: "bigint"},
				{Name: "code", TypeOID: vc.OID, TypeName: "varchar(16)"},
				{Name: "bio", TypeOID: tx.OID, TypeName: "text"},
			}},
		}}
	default:
		return &cache.Catalog{Tables: []cache.Table{
			{Schema: "main", Name: "users", OID: 1, Cols: []cache.Column{
				{Name: "id", TypeOID: 1, TypeName: "INTEGER"},
				{Name: "code", TypeOID: 3, TypeName: "TEXT"},
			}},
		}}
	}
}

func perfTypes(t *testing.T, dialectName, src string, params map[string]dialect.TypeRef) []diagnostics.Diagnostic {
	t.Helper()
	profile := perfProfiles[dialectName]
	frontends := map[string]dialect.Frontend{
		"postgres": postgres.Frontend{}, "mysql": mysql.Frontend{}, "sqlite": sqlite.Frontend{},
	}
	f, diags := template.NewScanner(profile).ScanFile("t.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	rs, err := ast.Renderings(profile, q)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := frontends[dialectName].Parse(rs[0].SQL)
	if err != nil {
		t.Fatal(err)
	}
	typeByName := map[string]func(string) (dialect.TypeRef, bool){
		"postgres": postgres.TypeMap{}.TypeByName, "mysql": mysql.TypeMap{}.TypeByName, "sqlite": sqlite.TypeMap{}.TypeByName,
	}[dialectName]
	return CheckPerfTypes(profile, dialectName, q, rs, tree, typesCatalog(t, dialectName), params, typeByName)
}

func TestPerfTypes_Postgres(t *testing.T) {
	numeric := dialect.TypeRef{OID: pgNumeric, Name: "numeric"}
	float8 := dialect.TypeRef{OID: pgFloat8, Name: "float8"}
	int8 := dialect.TypeRef{OID: pgInt8, Name: "int8"}
	tstamp := dialect.TypeRef{OID: pgTstamp, Name: "timestamp"}
	cases := []struct {
		name, where string
		p           dialect.TypeRef
		want        []string
	}{
		{"bigint vs numeric cast", "u.id = :p::numeric", numeric, []string{"u.id = :p::numeric"}},
		{"int vs float8", "u.age < :p", float8, []string{"u.age < :p"}},
		{"reversed", ":p >= u.age", numeric, []string{":p >= u.age"}},
		{"unqualified", "age = :p", numeric, []string{"age = :p"}},
		{"CAST form", "u.id = CAST(:p AS numeric)", numeric, []string{"u.id = CAST(:p AS numeric)"}},
		{"between", "u.age BETWEEN :p AND 10", numeric, []string{"u.age BETWEEN :p"}},

		// Cross-type comparisons inside one btree operator family keep
		// the index usable: never flagged.
		{"int8 vs int8", "u.id = :p", int8, nil},
		{"int4 vs int8", "u.age = :p", int8, nil},
		{"date vs timestamp", "u.born < :p", tstamp, nil},
		{"float column vs numeric param", "u.score = :p", numeric, nil},
		{"text column", "u.email = :p", numeric, nil},
		{"double cast", "u.id = :p::numeric::bigint", numeric, nil},
		{"wrapped column", "abs(u.id) = :p", numeric, nil},
		{"subquery scope", "u.id IN (SELECT o.id FROM orgs AS o WHERE o.id = :p)", numeric, nil},

		// With a cast, the comparison is against the CAST's type, not the
		// parameter's inferred one ($1 is typed once, by its first use):
		// `price = :p AND id = :p::int4` infers numeric, yet `id = $1::int4`
		// is int8 vs int4 — index-safe.
		{"cast to int over numeric param", "u.id = :p::int4", numeric, nil},
		{"CAST to int over numeric param", "u.id = CAST(:p AS integer)", numeric, nil},
		{"cast to numeric over int param", "u.id = :p::numeric", int8, []string{"u.id = :p::numeric"}},
		{"cast to multiword type", "u.age < :p::double precision", int8, []string{"u.age < :p::double precision"}},
		{"cast to unknown type", "u.id = :p::no_such_type", numeric, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "-- name: Q :one\nSELECT u.email FROM users AS u WHERE " + c.where + "\n"
			diags := perfTypes(t, "postgres", src, map[string]dialect.TypeRef{"p": c.p})
			got := spanTexts(src, diags, diagnostics.CodePerfTypeMismatch)
			if !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
			assertWarnings(t, diags)
		})
	}
}

func TestPerfTypes_MySQL(t *testing.T) {
	bigint, decimal, varchar := mysqlType(t, "bigint"), mysqlType(t, "decimal"), mysqlType(t, "varchar")
	cases := []struct {
		name, where string
		p           dialect.TypeRef
		want        []string
	}{
		{"varchar vs bigint", "u.code = :p", bigint, []string{"u.code = :p"}},
		{"text vs decimal", "u.bio = :p", decimal, []string{"u.bio = :p"}},
		{"in list", "u.code IN (:p, :p)", bigint, []string{"u.code IN (:p, :p)"}},

		{"string param", "u.code = :p", varchar, nil},
		// A numeric column vs a string param converts the PARAMETER: the
		// index stays usable.
		{"int column string param", "u.id = :p", varchar, nil},
		// The annotation types the bind, but an explicit cast decides
		// the comparison; the lint cannot know which wins, so it stays
		// quiet.
		{"cast param", "u.code = CAST(:p AS CHAR)", bigint, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "-- name: Q :one\n-- @param p: x\nSELECT u.id FROM users AS u WHERE " + c.where + "\n"
			diags := perfTypes(t, "mysql", src, map[string]dialect.TypeRef{"p": c.p})
			got := spanTexts(src, diags, diagnostics.CodePerfTypeMismatch)
			if !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// SQLite compares a column against a bound parameter under the
// COLUMN's affinity (a parameter has none), so a declared parameter
// type never moves the conversion onto the column: no pair is flagged.
func TestPerfTypes_SQLiteNeverFlags(t *testing.T) {
	src := "-- name: Q :one\n-- @param p: integer\nSELECT id FROM users WHERE code = :p\n"
	diags := perfTypes(t, "sqlite", src, map[string]dialect.TypeRef{"p": {OID: 1, Name: "INTEGER"}})
	if len(diags) != 0 {
		t.Errorf("got %+v", diags)
	}
}

// @nolint SQLETCH132 suppresses it; an @nolint that suppresses nothing is
// judged HERE (the catalog-free pass leaves SQLETCH132 alone).
func TestPerfTypes_NoLint(t *testing.T) {
	numeric := map[string]dialect.TypeRef{"p": {OID: pgNumeric, Name: "numeric"}}
	src := "-- name: Q :one\n-- @nolint SQLETCH132 (legacy float ids)\nSELECT u.email FROM users AS u WHERE u.id = :p::numeric\n"
	if diags := perfTypes(t, "postgres", src, numeric); len(diags) != 0 {
		t.Errorf("suppressed: %+v", diags)
	}
	src = "-- name: Q :one\n-- @nolint SQLETCH132\nSELECT u.email FROM users AS u WHERE u.id = :p\n"
	diags := perfTypes(t, "postgres", src, map[string]dialect.TypeRef{"p": {OID: pgInt8, Name: "int8"}})
	if got := spanTexts(src, diags, diagnostics.CodePerfNoLintUnused); !slices.Equal(got, []string{"-- @nolint SQLETCH132"}) {
		t.Errorf("got %q", got)
	}
	// ...and an @nolint of a catalog-free code is not judged here.
	src = "-- name: Q :one\n-- @nolint SQLETCH128\nSELECT u.email FROM users AS u WHERE u.id = :p\n"
	if diags := perfTypes(t, "postgres", src, map[string]dialect.TypeRef{"p": {OID: pgInt8}}); len(diags) != 0 {
		t.Errorf("got %+v", diags)
	}
}

// A guarded predicate is reachable: it is checked, at its template span.
func TestPerfTypes_Guarded(t *testing.T) {
	src := `-- name: Q :many
SELECT u.email FROM users AS u
WHERE TRUE
@if-present(p)
  AND u.id = :p::numeric
@endif
LIMIT 1
`
	diags := perfTypes(t, "postgres", src, map[string]dialect.TypeRef{"p": {OID: pgNumeric, Name: "numeric"}})
	if got := spanTexts(src, diags, diagnostics.CodePerfTypeMismatch); !slices.Equal(got, []string{"u.id = :p::numeric"}) {
		t.Errorf("got %q", got)
	}
}

// The rules package repeats mysql's TypeRef flag bits to stay free of a
// driver import; this pins the copies.
func TestPerfTypes_MySQLFlagsAgree(t *testing.T) {
	if mysqlFlagUnsigned != mysql.FlagUnsigned || mysqlFlagBinary != mysql.FlagBinary {
		t.Fatal("mysql TypeRef flag bits drifted from internal/dialect/mysql")
	}
}
