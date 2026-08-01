package codegen

import (
	"regexp"
	"strings"
	"testing"

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect"
	"github.com/moznion/sqletch/internal/dialect/postgres"
	"github.com/moznion/sqletch/internal/shape"
	"github.com/moznion/sqletch/internal/template"
	"github.com/moznion/sqletch/runtime"
)

const useCase1 = `-- name: SearchUsers :many
SELECT
    u.id,
    u.email,
    u.status,
    u.created_at
FROM users AS u

@if-present(organization_id)
JOIN organization_users AS ou
  ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif

WHERE TRUE

@if-present(status)
  AND u.status = :status
@endif

@if-present(email_prefix)
  AND u.email LIKE :email_prefix || '%'
@endif

@choose(sort)
@case(created_at_desc)
ORDER BY u.created_at DESC
@case(email_asc)
ORDER BY u.email ASC, u.id ASC
@default
ORDER BY u.id ASC
@end

LIMIT :limit;
`

func scanOne(t *testing.T, src string) *template.QueryTemplate {
	t.Helper()
	f, diags := template.NewScanner(postgres.Profile{}).ScanFile("t.sql", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("scan: %+v", diags)
	}
	return f.Queries[0]
}

// TestComposeConformance is the load-bearing test of the whole design:
// for every enumerable shape, the runtime composer over the generated
// fragment table must produce byte-identical SQL to the verification
// renderer, and identical parameter binding order.
func TestComposeConformance(t *testing.T) {
	corpus := []string{useCase1,
		`-- name: ListAuditLogs :many
SELECT a.id, a.action FROM audit_logs AS a
WHERE a.tenant_id = :tenant_id
@if-present(after_id)
  AND a.id < :after_id
@endif
ORDER BY a.id DESC
LIMIT :limit;
`,
		`-- name: SharedParam :many
SELECT t.id FROM t
WHERE t.a = :v
@if-present(x)
  AND t.x = :x AND t.b = :v
@endif
;
`,
		`-- name: UpdateUserProfile :one
UPDATE users
SET
    updated_at = now()
@if-present(email)
  , email = :email
@endif
@if-present(nickname)
  , nickname = :nickname
@endif
WHERE id = :id
RETURNING id, email, nickname, updated_at;
`,
	}
	for _, src := range corpus {
		q := scanOne(t, src)
		frags := BuildFrags(postgres.Profile{}, q)
		keys, _ := shape.Enumerate(q, 0)
		for _, k := range keys {
			want, err := ast.RenderShape(postgres.Profile{}, q, k.Guards, k.Selection())
			if err != nil {
				t.Fatal(err)
			}
			got, argIdx := runtime.Compose(frags, runtime.ShapeKey{Guards: k.Guards, Choices: k.Choices})
			if got != want.SQL {
				t.Fatalf("%s shape %s:\nruntime:\n%q\nrenderer:\n%q", q.Name, k, got, want.SQL)
			}
			// Bind order: argIdx maps into ParamOrder; must equal the
			// renderer's ParamsSeq name-for-name.
			if len(argIdx) != len(want.ParamsSeq) {
				t.Fatalf("%s shape %s: argIdx len %d, ParamsSeq len %d", q.Name, k, len(argIdx), len(want.ParamsSeq))
			}
			for i, idx := range argIdx {
				if q.ParamOrder[idx] != want.ParamsSeq[i] {
					t.Fatalf("%s shape %s: arg %d is %q, renderer expects %q",
						q.Name, k, i, q.ParamOrder[idx], want.ParamsSeq[i])
				}
			}
		}
	}
}

func typesFixture() map[string]dialect.TypeRef {
	return map[string]dialect.TypeRef{
		"organization_id": {OID: 20, Name: "int8"},
		"status":          {OID: 25, Name: "text"},
		"email_prefix":    {OID: 25, Name: "text"},
		"created_after":   {OID: 1184, Name: "timestamptz"},
		"limit":           {OID: 20, Name: "int8"},
		"tenant_id":       {OID: 20, Name: "int8"},
		"after_id":        {OID: 20, Name: "int8"},
	}
}

func columnsFixture() []dialect.ColumnDesc {
	return []dialect.ColumnDesc{
		{Name: "id", Type: dialect.TypeRef{OID: 20, Name: "int8"}},
		{Name: "email", Type: dialect.TypeRef{OID: 25, Name: "text"}},
		{Name: "status", Type: dialect.TypeRef{OID: 25, Name: "text"}},
		{Name: "created_at", Type: dialect.TypeRef{OID: 1184, Name: "timestamptz"}},
	}
}

func generateUC1(t *testing.T) map[string][]byte {
	t.Helper()
	q := scanOne(t, useCase1)
	q.Params["organization_id"].Optional = true
	q.Params["status"].Optional = true
	q.Params["email_prefix"].Optional = true
	files, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
		Q:          q,
		Frags:      BuildFrags(postgres.Profile{}, q),
		ParamTypes: typesFixture(),
		Columns:    columnsFixture(),
		Nullable:   []bool{false, false, false, false},
	}})
	if len(diags) != 0 {
		t.Fatalf("generate diagnostics: %+v", diags)
	}
	return files
}

func TestGenerate_UseCase1(t *testing.T) {
	files := generateUC1(t)
	for _, name := range []string{"db.go", "querier.go", "search_users.sql.go"} {
		if files[name] == nil {
			t.Fatalf("missing file %s (have %v)", name, keysOf(files))
		}
	}
	src := string(files["search_users.sql.go"])
	// gofmt column-aligns struct fields, so field assertions use \s+.
	for _, want := range []string{
		`type SearchUsersSort int`,
		`SearchUsersSortDefault SearchUsersSort = iota`,
		`SearchUsersSortCreatedAtDesc`,
		`OrganizationID\s+\*int64`,
		`Status\s+\*string`,
		`Sort\s+SearchUsersSort`,
		`Limit\s+int64`,
		`type SearchUsersRow struct`,
		`CreatedAt\s+time\.Time`,
		`var searchUsersFrags = \[\]runtime\.Frag`,
		`func \(q \*Queries\) SearchUsers\(ctx context\.Context, arg SearchUsersParams\) \(\[\]SearchUsersRow, error\)`,
		`runtime\.ChooseOrdinal\(int\(arg\.Sort\), 2, true\)`,
		`q\.cache\.Get\("SearchUsers", searchUsersFrags, key\)`,
		`rows\.Scan\(&i\.ID, &i\.Email, &i\.Status, &i\.CreatedAt\)`,
	} {
		if !regexp.MustCompile(want).MatchString(src) {
			t.Errorf("generated code missing pattern %q\n----\n%s", want, src)
		}
	}
	if !strings.Contains(string(files["querier.go"]), "SearchUsers(ctx context.Context, arg SearchUsersParams) ([]SearchUsersRow, error)") {
		t.Error("querier.go missing the method signature")
	}

	// Determinism: generating twice yields identical bytes.
	files2 := generateUC1(t)
	for name := range files {
		if string(files[name]) != string(files2[name]) {
			t.Errorf("%s differs across generations", name)
		}
	}
}

func TestGenerate_Collision(t *testing.T) {
	q := scanOne(t, `-- name: C :many
SELECT t.a AS user_id, t.b AS user__id FROM t;
`)
	_, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
		Q: q, Frags: BuildFrags(postgres.Profile{}, q),
		Columns: []dialect.ColumnDesc{
			{Name: "user_id", Type: dialect.TypeRef{OID: 20}},
			{Name: "user__id", Type: dialect.TypeRef{OID: 20}},
		},
		Nullable:   []bool{false, false},
		ParamTypes: map[string]dialect.TypeRef{},
	}})
	if !hasCode(diags, diagnostics.CodeNameCollision) {
		t.Errorf("want SQLETCH310, got %+v", diags)
	}
}

func TestGenerate_UnsupportedType(t *testing.T) {
	q := scanOne(t, `-- name: U :many
SELECT t.a FROM t WHERE t.x = :x;
`)
	_, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
		Q: q, Frags: BuildFrags(postgres.Profile{}, q),
		Columns:    []dialect.ColumnDesc{{Name: "a", Type: dialect.TypeRef{OID: 20}}},
		Nullable:   []bool{false},
		ParamTypes: map[string]dialect.TypeRef{"x": {OID: 99999, Name: "weirdtype"}},
	}})
	if !hasCode(diags, diagnostics.CodeUnsupportedType) {
		t.Errorf("want SQLETCH311, got %+v", diags)
	}
}

func TestGoName(t *testing.T) {
	tests := map[string]string{
		"organization_id": "OrganizationID",
		"id":              "ID",
		"email_prefix":    "EmailPrefix",
		"created_at":      "CreatedAt",
		"uuid":            "UUID",
		"api_url":         "APIURL",
		"a":               "A",
	}
	for in, want := range tests {
		if got := GoName(in); got != want {
			t.Errorf("GoName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := lowerCamel("SearchUsers"); got != "searchUsers" {
		t.Errorf("lowerCamel = %q", got)
	}
	if got := lowerCamel("IDList"); got != "idList" {
		t.Errorf("lowerCamel(IDList) = %q", got)
	}
	if got := pascalToSnake("SearchUsers"); got != "search_users" {
		t.Errorf("pascalToSnake = %q", got)
	}
}

func hasCode(diags []diagnostics.Diagnostic, code diagnostics.Code) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

func keysOf(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
