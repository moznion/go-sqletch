package codegen

import (
	"regexp"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

// A result column whose name does not map to a valid Go identifier
// (a quoted alias / oracle column carrying spaces, punctuation, or
// injection-shaped text) must be refused with SQLETCH307 rather than
// silently failing gofmt or splicing text into the generated package.
func TestGenerate_InvalidColumnIdentifier(t *testing.T) {
	gen := func(colName string) []diagnostics.Diagnostic {
		q := scanOne(t, "-- name: Q :many\nSELECT t.a FROM t;\n")
		_, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
			Q: q, Frags: BuildFrags(postgres.Profile{}, q),
			Columns:    []dialect.ColumnDesc{{Name: colName, Type: dialect.TypeRef{OID: 20}}},
			Nullable:   []bool{false},
			ParamTypes: map[string]dialect.TypeRef{},
		}})
		return diags
	}

	for _, bad := range []string{
		"user name",     // quoted alias with a space
		"evil struct{}", // brace/space — would break the struct literal
		"a;b",           // punctuation
	} {
		if !hasCode(gen(bad), diagnostics.CodeInvalidColumnIdentifier) {
			t.Errorf("column %q: want SQLETCH307, got %+v", bad, gen(bad))
		}
	}

	// A normal column name still generates cleanly.
	q := scanOne(t, "-- name: Q :many\nSELECT t.id FROM t;\n")
	files, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
		Q: q, Frags: BuildFrags(postgres.Profile{}, q),
		Columns:    []dialect.ColumnDesc{{Name: "id", Type: dialect.TypeRef{OID: 20}}},
		Nullable:   []bool{false},
		ParamTypes: map[string]dialect.TypeRef{},
	}})
	if diagnostics.HasErrors(diags) {
		t.Fatalf("valid column rejected: %+v", diags)
	}
	if !strings.Contains(string(files["q.sql.gen.go"]), "ID ") {
		t.Errorf("expected field ID for column id\n----\n%s", files["q.sql.gen.go"])
	}
}

// argIdent must suffix every identifier that would collide with a local
// writeFunc emits in the query method's body — otherwise a policy /
// @filter-tree! parameter spelled like a generated local yields Go that
// fails to compile (redeclaration) or silently clobbers the argument in
// the consumer's module.
func TestArgIdent_ReservedLocals(t *testing.T) {
	cases := map[string]string{
		// fixed locals
		"exec_start": "execStartArg",
		"res":        "resArg",
		"n":          "nArg",
		"rerr":       "rerrArg",
		"tag":        "tagArg",
		"row":        "rowArg",
		"rows":       "rowsArg",
		"items":      "itemsArg",
		"key":        "keyArg",
		"zero":       "zeroArg",
		"i":          "iArg",
		"ctx":        "ctxArg",
		"arg":        "argArg",
		// indexed families ord%d / oseq%d / nul%d
		"ord0":  "ord0Arg",
		"oseq1": "oseq1Arg",
		"nul2":  "nul2Arg",
		"nul_0": "nul0Arg",
		"ord_1": "ord1Arg",
		// unaffected names pass through unchanged
		"tenant_id":  "tenantID",
		"scope":      "scope",
		"ordinal":    "ordinal", // not ord+digits
		"nullable":   "nullable",
		"n_extra":    "nExtra",
		"resolution": "resolution",
	}
	for in, want := range cases {
		if got := argIdent(in); got != want {
			t.Errorf("argIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

// End-to-end: a required @filter-tree! parameter named like a generated
// local (exec_start → execStart) must be renamed in the signature so it
// no longer collides with the body's `var execStart time.Time`.
func TestGenerate_RequiredArgAvoidsLocalCollision(t *testing.T) {
	q := scanOne(t, `-- name: Pick :many
SELECT t.id FROM t
WHERE TRUE
  AND @filter-tree!(exec_start)
@predicate(tenant)
t.tenant_id = :scope_tenant_id
@end;
`)
	files, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
		Q: q, Frags: BuildFrags(postgres.Profile{}, q),
		Columns:    []dialect.ColumnDesc{{Name: "id", Type: dialect.TypeRef{OID: 20}}},
		Nullable:   []bool{false},
		ParamTypes: map[string]dialect.TypeRef{"scope_tenant_id": {OID: 20}},
	}})
	if diagnostics.HasErrors(diags) {
		t.Fatalf("generate: %+v", diags)
	}
	src := string(files["pick.sql.gen.go"])
	// The argument is renamed away from the local.
	if !regexp.MustCompile(`func \(q \*Queries\) Pick\(ctx context\.Context, execStartArg runtime\.Tree,`).MatchString(src) {
		t.Errorf("required tree arg not renamed to execStartArg\n----\n%s", src)
	}
	// The generated local it would have collided with is still emitted.
	if !strings.Contains(src, "var execStart time.Time") {
		t.Errorf("expected the execStart local to remain\n----\n%s", src)
	}
	// And there is no bare `execStart runtime.Tree` parameter (the collision).
	if regexp.MustCompile(`\bexecStart runtime\.Tree\b`).MatchString(src) {
		t.Errorf("required tree arg still collides with the local\n----\n%s", src)
	}
}
