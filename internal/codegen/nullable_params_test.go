package codegen

import (
	"regexp"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/template"
	"github.com/moznion/go-sqletch/runtime"
)

// patchUser exercises all four design-20 §4 rows in one query:
// bio (unguarded, nullable), nickname (guarded, nullable),
// email (guarded, NOT NULL), id (unguarded, NOT NULL).
const patchUser = `-- name: PatchUser :exec
UPDATE users SET bio = :bio
@if-present(nickname)
  , nickname = :nickname
@endif
@if-present(email)
  , email = :email
@endif
WHERE id = :id;
`

func generatePatchUser(t *testing.T, opts Options, profile dialect.LexerProfile, tm dialect.TypeMap, types map[string]dialect.TypeRef, nullable map[string]bool) string {
	t.Helper()
	f, diags := template.NewScanner(profile).ScanFile("t.sql", []byte(patchUser))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	q := f.Queries[0]
	q.Params["nickname"].Optional = true
	q.Params["email"].Optional = true
	files, gd := Generate(opts, tm, []QueryInput{{
		Q:              q,
		Frags:          BuildFrags(profile, q),
		ParamTypes:     types,
		NullableParams: nullable,
	}})
	if len(gd) != 0 {
		t.Fatalf("generate diagnostics: %+v", gd)
	}
	return string(files["patch_user.sql.gen.go"])
}

func assertPatterns(t *testing.T, src string, patterns ...string) {
	t.Helper()
	for _, p := range patterns {
		if !regexp.MustCompile(p).MatchString(src) {
			t.Errorf("generated code missing pattern %q\n----\n%s", p, src)
		}
	}
}

// Design 20 §4: the field type and the bind expression of each of the
// four (guarded × nullable) combinations.
func TestGenerate_NullableParamsMatrix(t *testing.T) {
	pgText := dialect.TypeRef{OID: 25, Name: "text"}
	types := map[string]dialect.TypeRef{
		"bio": pgText, "nickname": pgText, "email": pgText,
		"id": {OID: 20, Name: "int8"},
	}
	src := generatePatchUser(t, Options{Package: "gen"}, postgres.Profile{}, postgres.TypeMap{}, types,
		map[string]bool{"bio": true, "nickname": true})
	assertPatterns(t, src,
		`Bio\s+optional\.Option\[string\]\s+// None binds NULL`,
		`Nickname\s+sqletch\.Omittable\[optional\.Option\[string\]\]\s+// zero value omits the guarded fragment\(s\); None binds NULL`,
		`Email\s+sqletch\.Omittable\[string\]\s+// zero value omits the guarded fragment\(s\)\n`,
		`ID\s+int64\n`,
		`arg\.Nickname\.IsPresent\(\)`,
		`arg\.Email\.IsPresent\(\)`,
		`arg\.Bio\.UnwrapAsPtr\(\), arg\.Nickname\.OrZero\(\)\.UnwrapAsPtr\(\), arg\.Email\.Ptr\(\), arg\.ID\b`,
		`"github.com/moznion/go-optional"`,
		`"github.com/moznion/go-sqletch"`,
	)
	if strings.Contains(src, "IsSome()") {
		t.Errorf("presence must be IsPresent, never IsSome:\n%s", src)
	}
}

// Without derived nullability, guarded params are Omittable[T] and a
// query with no nullable surface does not import go-optional at all.
func TestGenerate_GuardedWithoutNullableParams(t *testing.T) {
	pgText := dialect.TypeRef{OID: 25, Name: "text"}
	types := map[string]dialect.TypeRef{
		"bio": pgText, "nickname": pgText, "email": pgText,
		"id": {OID: 20, Name: "int8"},
	}
	src := generatePatchUser(t, Options{Package: "gen"}, postgres.Profile{}, postgres.TypeMap{}, types, nil)
	assertPatterns(t, src,
		`Bio\s+string\n`,
		`Nickname\s+sqletch\.Omittable\[string\]\s+// zero value omits the guarded fragment\(s\)\n`,
		`arg\.Bio, arg\.Nickname\.Ptr\(\), arg\.Email\.Ptr\(\), arg\.ID\b`,
		`"github.com/moznion/go-sqletch"`,
	)
	if strings.Contains(src, "go-optional") {
		t.Errorf("no nullable surface: go-optional must not be imported:\n%s", src)
	}
}

// The question-style (database/sql) flavor binds through the same
// expressions; only the composition entry point differs.
func TestGenerate_NullableParamsQuestionStyle(t *testing.T) {
	text := dialect.TypeRef{OID: 252, Name: "text"}
	types := map[string]dialect.TypeRef{
		"bio": text, "nickname": text, "email": text,
		"id": {OID: 8, Name: "bigint"},
	}
	src := generatePatchUser(t, Options{Package: "gen", Style: runtime.StyleQuestion}, mysql.Profile{}, mysql.TypeMap{}, types,
		map[string]bool{"bio": true, "nickname": true})
	assertPatterns(t, src,
		`Bio\s+optional\.Option\[string\]`,
		`Nickname\s+sqletch\.Omittable\[optional\.Option\[string\]\]`,
		`arg\.Bio\.UnwrapAsPtr\(\), arg\.Nickname\.OrZero\(\)\.UnwrapAsPtr\(\), arg\.Email\.Ptr\(\), arg\.ID\b`,
	)
}

// A required argument named like the sqletch import would shadow the
// package inside the generated method; argIdent must escape it.
func TestArgIdent_SqletchImportEscaped(t *testing.T) {
	if got := argIdent("sqletch", "Q"); got != "sqletchArg" {
		t.Errorf("argIdent(sqletch) = %q, want sqletchArg", got)
	}
}
