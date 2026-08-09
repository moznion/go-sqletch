package codegen

import (
	"regexp"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/shape"
	"github.com/moznion/go-sqletch/internal/template"
	"github.com/moznion/go-sqletch/runtime"
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

var conformanceCorpus = []string{useCase1,
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
	`-- name: CreateUser :one
INSERT INTO users (
    email
@if-present(nickname)
  , nickname
@endif
) VALUES (
    :email
@if-present(nickname)
  , :nickname
@endif
)
RETURNING id;
`,
	`-- name: WhenAndHaving :many
SELECT t.user_id, sum(t.amount) AS total FROM t
WHERE TRUE
@when(include_all = false)
  AND t.visible
@end
GROUP BY t.user_id
HAVING TRUE
@if-present(min_total)
  AND sum(t.amount) >= :min_total
@endif
;
`,
	`-- name: OrderedUsers :many
SELECT u.id, u.email FROM users AS u
WHERE TRUE
@if-present(status)
  AND u.status = :status
@endif
@order-by(sort)
@key(created_at)
u.created_at
@key(email)
u.email
@default
ORDER BY u.id ASC
@end
LIMIT :limit;
`,
	`-- name: SignupsByBucket :many
SELECT
@choose(bucket)
@case(daily)
date_trunc('day', u.created_at)
@case(weekly)
date_trunc('week', u.created_at)
@end
 AS bucket,
    count(*) AS signups
FROM users AS u
WHERE u.created_at >= :since
GROUP BY 1
ORDER BY 1;
`,
}

// Generation must be deterministic even on the internal-error path:
// these diagnostics carry no span, so nothing downstream can reorder
// them back into a stable sequence.
func TestFormatFilesDiagnosticsAreOrdered(t *testing.T) {
	var first []string
	for range 8 {
		files := map[string][]byte{
			"c.gen.go":  []byte("package p\nfunc ("),
			"a.gen.go":  []byte("package p\nfunc ("),
			"b.gen.go":  []byte("package p\nfunc ("),
			"ok.gen.go": []byte("package p\n"),
		}
		diags := formatFiles(files)
		if len(diags) != 3 {
			t.Fatalf("diags = %d, want 3: %+v", len(diags), diags)
		}
		var got []string
		for _, d := range diags {
			got = append(got, d.Message)
		}
		if first == nil {
			first = got
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("diagnostic order varies between runs:\n%v\nvs\n%v", got, first)
			}
		}
		if !strings.Contains(got[0], "a.gen.go") || !strings.Contains(got[2], "c.gen.go") {
			t.Errorf("diagnostics must be ordered by file name: %v", got)
		}
		// The valid file is still formatted.
		if string(files["ok.gen.go"]) != "package p\n" {
			t.Errorf("ok.gen.go = %q", files["ok.gen.go"])
		}
	}
}

// The scanner rejects oversized constructs and the runtime defends
// against them; both ends must agree on where the line is, or the
// compiler accepts a template whose composition then errors — or
// worse, silently truncates. This package is the only one that imports
// both, so the pin lives here.
func TestShapeKeyLimitsAgree(t *testing.T) {
	if template.MaxOrderKeys != runtime.MaxOrderKeys {
		t.Errorf("MaxOrderKeys: scanner %d, runtime %d",
			template.MaxOrderKeys, runtime.MaxOrderKeys)
	}
	if template.MaxChooseOrdinals != runtime.MaxChooseOrdinals {
		t.Errorf("MaxChooseOrdinals: scanner %d, runtime %d",
			template.MaxChooseOrdinals, runtime.MaxChooseOrdinals)
	}
	// The packed element key<<1|desc must stay inside a uint8, and
	// shape.orderOptions tracks used keys in a uint64.
	if max := (runtime.MaxOrderKeys-1)<<1 | 1; max > 255 {
		t.Errorf("packed order element %d overflows uint8", max)
	}
	if runtime.MaxOrderKeys > 64 {
		t.Errorf("MaxOrderKeys %d overflows the used-key mask", runtime.MaxOrderKeys)
	}
}

// TestComposeConformance is the load-bearing test of the whole design:
// for every enumerable shape, the runtime composer over the generated
// fragment table must produce byte-identical SQL to the verification
// renderer, and identical parameter binding order.
func TestComposeConformance(t *testing.T) {
	conformanceOver(t, postgres.Profile{}, runtime.StyleDollar)
}

// TestComposeConformance_QuestionStyle runs the same corpus under the
// question-style profiles: '?' per occurrence, repeated binds
// repeated in the arg plan.
func TestComposeConformance_QuestionStyle(t *testing.T) {
	t.Run("mysql", func(t *testing.T) { conformanceOver(t, mysql.Profile{}, runtime.StyleQuestion) })
	t.Run("sqlite", func(t *testing.T) { conformanceOver(t, sqlite.Profile{}, runtime.StyleQuestion) })
}

func conformanceOver(t *testing.T, profile dialect.LexerProfile, style runtime.Style) {
	t.Helper()
	for _, src := range conformanceCorpus {
		f, diags := template.NewScanner(profile).ScanFile("t.sql", []byte(src))
		if len(diags) != 0 {
			t.Fatalf("scan: %+v", diags)
		}
		q := f.Queries[0]
		frags := BuildFrags(profile, q)
		keys, _ := shape.Enumerate(q, 0)
		for _, k := range keys {
			want, err := ast.RenderShape(profile, q, k.Guards, k.Selection(), k.OrderSelection(), k.InSelection())
			if err != nil {
				t.Fatal(err)
			}
			got, argIdx := runtime.ComposeStyle(style, frags, runtime.ShapeKey{Guards: k.Guards, Choices: k.Choices, Orders: k.Orders})
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
	for _, name := range []string{"db.gen.go", "querier.gen.go", "search_users.sql.gen.go"} {
		if files[name] == nil {
			t.Fatalf("missing file %s (have %v)", name, keysOf(files))
		}
	}
	// Generated files must be recognizable by name, not only by the
	// "Code generated" header: nothing may be emitted without .gen.go.
	for _, name := range keysOf(files) {
		if !strings.HasSuffix(name, ".gen.go") {
			t.Errorf("emitted file %q does not carry the .gen.go suffix", name)
		}
	}
	src := string(files["search_users.sql.gen.go"])
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
	if !strings.Contains(string(files["querier.gen.go"]), "SearchUsers(ctx context.Context, arg SearchUsersParams) ([]SearchUsersRow, error)") {
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

func TestGenerate_FileStemCollision(t *testing.T) {
	mk := func(name string) QueryInput {
		q := scanOne(t, "-- name: "+name+" :many\nSELECT t.a FROM t;\n")
		return QueryInput{
			Q: q, Frags: BuildFrags(postgres.Profile{}, q),
			Columns:    []dialect.ColumnDesc{{Name: "a", Type: dialect.TypeRef{OID: 20}}},
			Nullable:   []bool{false},
			ParamTypes: map[string]dialect.TypeRef{},
		}
	}
	// UserID and UserId are distinct Go names but one file stem.
	files, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{},
		[]QueryInput{mk("UserID"), mk("UserId")})
	if !hasCode(diags, diagnostics.CodeNameCollision) {
		t.Errorf("want SQLETCH310, got %+v", diags)
	}
	if _, ok := files["user_i_d.sql.gen.go"]; ok {
		t.Error("capital runs must not split into per-letter words")
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
	snakes := map[string]string{
		"SearchUsers":      "search_users",
		"FindUserByUserID": "find_user_by_user_id",
		"UserID":           "user_id",
		"ID":               "id",
		"IDList":           "id_list",
		"ParseHTTPRequest": "parse_http_request",
		"HTTPServer":       "http_server",
		"ServeHTTP":        "serve_http",
		"Find_UserID":      "find_user_id",
		"find_user":        "find_user",
		"GetV2User":        "get_v2_user",
		"HTTP2Server":      "http2_server",
		"A":                "a",
	}
	for in, want := range snakes {
		if got := pascalToSnake(in); got != want {
			t.Errorf("pascalToSnake(%q) = %q, want %q", in, got, want)
		}
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

// Values the caller must not omit leave the params struct and become
// arguments of the generated method. Go cannot make a struct field
// mandatory — a keyed composite literal that leaves one out compiles
// and yields the zero value — so a scoping value kept as a field turns
// a forgotten scope into a silently unscoped read instead of an error.
// As an argument, forgetting it does not compile.
func TestGenerate_RequiredValuesAreArguments(t *testing.T) {
	t.Run("policy-woven parameter", func(t *testing.T) {
		q := scanOne(t, `-- name: CountLogs :one
SELECT count(*) AS total FROM audit_logs WHERE audit_logs.tenant_id = :tenant_id;
`)
		// Stand in for the weaver: the parameter carries the name of the
		// policy that injected it.
		q.Params["tenant_id"].Policy = "tenant_scope"

		files, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
			Q: q, Frags: BuildFrags(postgres.Profile{}, q),
			Columns:    []dialect.ColumnDesc{{Name: "total", Type: dialect.TypeRef{OID: 20}}},
			Nullable:   []bool{false},
			ParamTypes: map[string]dialect.TypeRef{"tenant_id": {OID: 20}},
		}})
		if diagnostics.HasErrors(diags) {
			t.Fatalf("generate: %+v", diags)
		}

		src := string(files["count_logs.sql.gen.go"])
		for _, want := range []string{
			`func \(q \*Queries\) CountLogs\(ctx context\.Context, tenantID TenantID, arg CountLogsParams\)`,
			`type CountLogsParams struct \{\n\}`,
		} {
			if !regexp.MustCompile(want).MatchString(src) {
				t.Errorf("missing pattern %q\n----\n%s", want, src)
			}
		}
		if regexp.MustCompile(`TenantID\s+int64\b`).MatchString(src) {
			t.Errorf("policy parameter stayed a params field; omitting it would compile\n----\n%s", src)
		}
		if !strings.Contains(string(files["querier.gen.go"]),
			"CountLogs(ctx context.Context, tenantID TenantID, arg CountLogsParams)") {
			t.Errorf("querier.go missing the argument\n----\n%s", files["querier.gen.go"])
		}
	})

	t.Run("required filter-tree", func(t *testing.T) {
		q := scanOne(t, `-- name: Pick :many
SELECT t.id FROM t
WHERE TRUE
  AND @filter-tree!(scope)
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
		if !regexp.MustCompile(
			`func \(q \*Queries\) Pick\(ctx context\.Context, scope runtime\.Tree, arg PickParams\)`).MatchString(src) {
			t.Errorf("required tree is not an argument\n----\n%s", src)
		}
		if regexp.MustCompile(`Scope\s+runtime\.Tree`).MatchString(src) {
			t.Errorf("required tree stayed a params field\n----\n%s", src)
		}
		// The nil check stays: an argument can still be given an
		// explicit nil, which is a deliberate act rather than an
		// oversight, and Unscoped() is the intended way to say it.
		if !strings.Contains(src, "runtime.ErrFilterRequired") {
			t.Errorf("lost the explicit-nil guard\n----\n%s", src)
		}
		// The `;t=` key segment is derived once, by the cache. The call
		// site must NOT derive it a second time for the hook — that
		// encoded every tree twice per call — and must route the hook
		// through hookTree so an installed hook still sees the segment.
		if strings.Contains(src, "key.Trees =") {
			t.Errorf("call site re-derives the tree key segment\n----\n%s", src)
		}
		if !strings.Contains(src, "q.hookTree(key, scope, sqlText)") {
			t.Errorf("filter-tree query does not hook through hookTree\n----\n%s", src)
		}
		if !strings.Contains(string(files["db.gen.go"]), "func (q *Queries) hookTree(") {
			t.Error("db.gen.go lacks hookTree for a package that has a filter tree")
		}
	})

	// hookTree is dead code in a package with no @filter-tree query, and
	// an unused unexported method is exactly what a consumer's linter
	// complains about — so it is emitted only where it is called.
	t.Run("hookTree omitted without a filter tree", func(t *testing.T) {
		q := scanOne(t, `-- name: Plain :many
SELECT t.id FROM t WHERE t.a = :a;
`)
		files, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
			Q: q, Frags: BuildFrags(postgres.Profile{}, q),
			Columns:    []dialect.ColumnDesc{{Name: "id", Type: dialect.TypeRef{OID: 20}}},
			Nullable:   []bool{false},
			ParamTypes: map[string]dialect.TypeRef{"a": {OID: 20}},
		}})
		if diagnostics.HasErrors(diags) {
			t.Fatalf("generate: %+v", diags)
		}
		if strings.Contains(string(files["db.gen.go"]), "hookTree") {
			t.Errorf("hookTree emitted for a package with no filter tree\n----\n%s", files["db.gen.go"])
		}
	})

	// An optional tree is genuinely omittable (nil renders TRUE), so it
	// stays a field: requiring it as an argument would be noise.
	t.Run("optional filter-tree stays a field", func(t *testing.T) {
		q := scanOne(t, `-- name: Loose :many
SELECT t.id FROM t
WHERE TRUE
  AND @filter-tree(scope)
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
		src := string(files["loose.sql.gen.go"])
		if !regexp.MustCompile(`Scope\s+runtime\.Tree`).MatchString(src) {
			t.Errorf("optional tree should stay a params field\n----\n%s", src)
		}
		if !regexp.MustCompile(
			`func \(q \*Queries\) Loose\(ctx context\.Context, arg LooseParams\)`).MatchString(src) {
			t.Errorf("optional tree should not become an argument\n----\n%s", src)
		}
	})
}

// A policy parameter's argument gets a distinct named type (one per
// woven parameter, shared by every query the policy touches), so two
// same-typed policy arguments cannot be swapped at a call site: the
// wrong order is a type mismatch instead of a silent cross-scoping.
func TestGenerate_PolicyParamNamedTypes(t *testing.T) {
	weave := func(t *testing.T, src string, policies map[string]string) *template.QueryTemplate {
		t.Helper()
		q := scanOne(t, src)
		// Stand in for the weaver: each parameter carries the name of
		// the policy that injected it.
		for param, pol := range policies {
			q.Params[param].Policy = pol
		}
		return q
	}

	t.Run("distinct type per parameter", func(t *testing.T) {
		q := weave(t, `-- name: CountLogs :one
SELECT count(*) AS total FROM audit_logs
WHERE audit_logs.tenant_id = :tenant_id AND audit_logs.org_id = :org_id;
`, map[string]string{"tenant_id": "tenant_scope", "org_id": "org_scope"})

		files, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
			Q: q, Frags: BuildFrags(postgres.Profile{}, q),
			Columns:    []dialect.ColumnDesc{{Name: "total", Type: dialect.TypeRef{OID: 20}}},
			Nullable:   []bool{false},
			ParamTypes: map[string]dialect.TypeRef{"tenant_id": {OID: 20}, "org_id": {OID: 20}},
		}})
		if diagnostics.HasErrors(diags) {
			t.Fatalf("generate: %+v", diags)
		}

		pol := string(files["policy.gen.go"])
		for _, want := range []string{"type TenantID int64", "type OrgID int64"} {
			if !strings.Contains(pol, want) {
				t.Errorf("policy.gen.go missing %q\n----\n%s", want, pol)
			}
		}
		// Deterministic emission: type declarations in sorted name order.
		if strings.Index(pol, "type OrgID") > strings.Index(pol, "type TenantID") {
			t.Errorf("policy types not sorted\n----\n%s", pol)
		}
		if !strings.Contains(pol, "tenant_scope") {
			t.Errorf("type doc does not name the weaving policy\n----\n%s", pol)
		}

		src := string(files["count_logs.sql.gen.go"])
		if !regexp.MustCompile(
			`func \(q \*Queries\) CountLogs\(ctx context\.Context, tenantID TenantID, orgID OrgID, arg CountLogsParams\)`).MatchString(src) {
			t.Errorf("signature does not use the named types\n----\n%s", src)
		}
		// The driver sees the underlying type: the named type is a
		// call-site discipline, never a wire change.
		if !strings.Contains(src, "int64(tenantID), int64(orgID)") {
			t.Errorf("bind site does not convert back to the underlying type\n----\n%s", src)
		}
		if !strings.Contains(string(files["querier.gen.go"]),
			"CountLogs(ctx context.Context, tenantID TenantID, orgID OrgID, arg CountLogsParams)") {
			t.Errorf("querier.go signature does not use the named types\n----\n%s", files["querier.gen.go"])
		}
	})

	t.Run("one shared type across queries", func(t *testing.T) {
		a := weave(t, `-- name: CountLogs :one
SELECT count(*) AS total FROM audit_logs WHERE audit_logs.tenant_id = :tenant_id;
`, map[string]string{"tenant_id": "tenant_scope"})
		b := weave(t, `-- name: ListLogs :many
SELECT l.id FROM audit_logs AS l WHERE l.tenant_id = :tenant_id;
`, map[string]string{"tenant_id": "tenant_scope"})

		files, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{
			{Q: a, Frags: BuildFrags(postgres.Profile{}, a),
				Columns:    []dialect.ColumnDesc{{Name: "total", Type: dialect.TypeRef{OID: 20}}},
				Nullable:   []bool{false},
				ParamTypes: map[string]dialect.TypeRef{"tenant_id": {OID: 20}}},
			{Q: b, Frags: BuildFrags(postgres.Profile{}, b),
				Columns:    []dialect.ColumnDesc{{Name: "id", Type: dialect.TypeRef{OID: 20}}},
				Nullable:   []bool{false},
				ParamTypes: map[string]dialect.TypeRef{"tenant_id": {OID: 20}}},
		})
		if diagnostics.HasErrors(diags) {
			t.Fatalf("generate: %+v", diags)
		}
		pol := string(files["policy.gen.go"])
		if got := strings.Count(pol, "type TenantID int64"); got != 1 {
			t.Errorf("want exactly one shared declaration, got %d\n----\n%s", got, pol)
		}
		for _, f := range []string{"count_logs.sql.gen.go", "list_logs.sql.gen.go"} {
			if !strings.Contains(string(files[f]), "tenantID TenantID") {
				t.Errorf("%s does not use the shared type\n----\n%s", f, files[f])
			}
		}
	})

	t.Run("same name, different Go types is a collision", func(t *testing.T) {
		a := weave(t, `-- name: CountLogs :one
SELECT count(*) AS total FROM audit_logs WHERE audit_logs.tenant_id = :tenant_id;
`, map[string]string{"tenant_id": "tenant_scope"})
		b := weave(t, `-- name: ListLogs :many
SELECT l.id FROM audit_logs AS l WHERE l.tenant_id = :tenant_id;
`, map[string]string{"tenant_id": "other_scope"})

		_, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{
			{Q: a, Frags: BuildFrags(postgres.Profile{}, a),
				Columns:    []dialect.ColumnDesc{{Name: "total", Type: dialect.TypeRef{OID: 20}}},
				Nullable:   []bool{false},
				ParamTypes: map[string]dialect.TypeRef{"tenant_id": {OID: 20}}},
			{Q: b, Frags: BuildFrags(postgres.Profile{}, b),
				Columns:    []dialect.ColumnDesc{{Name: "id", Type: dialect.TypeRef{OID: 20}}},
				Nullable:   []bool{false},
				ParamTypes: map[string]dialect.TypeRef{"tenant_id": {OID: 25}}},
		})
		if !hasCode(diags, diagnostics.CodeNameCollision) {
			t.Fatalf("conflicting underlying types must be a name collision, got %+v", diags)
		}
	})

	t.Run("collision with a query-generated type", func(t *testing.T) {
		// Query "Tenant" with @choose(id) claims enum type TenantID —
		// the same name the policy parameter tenant_id generates.
		enumQ := scanOne(t, `-- name: Tenant :many
SELECT t.a FROM t
@choose(id)
@case(x)
ORDER BY t.a ASC
@default
ORDER BY t.a DESC
@end;
`)
		polQ := weave(t, `-- name: CountLogs :one
SELECT count(*) AS total FROM audit_logs WHERE audit_logs.tenant_id = :tenant_id;
`, map[string]string{"tenant_id": "tenant_scope"})

		_, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{
			{Q: enumQ, Frags: BuildFrags(postgres.Profile{}, enumQ),
				Columns:    []dialect.ColumnDesc{{Name: "a", Type: dialect.TypeRef{OID: 20}}},
				Nullable:   []bool{false},
				ParamTypes: map[string]dialect.TypeRef{}},
			{Q: polQ, Frags: BuildFrags(postgres.Profile{}, polQ),
				Columns:    []dialect.ColumnDesc{{Name: "total", Type: dialect.TypeRef{OID: 20}}},
				Nullable:   []bool{false},
				ParamTypes: map[string]dialect.TypeRef{"tenant_id": {OID: 20}}},
		})
		if !hasCode(diags, diagnostics.CodeNameCollision) {
			t.Fatalf("policy type colliding with a query type must be reported, got %+v", diags)
		}
	})
}

func TestGenerate_PolicyParamReservedName(t *testing.T) {
	// The generated package already declares Queries, Querier, DBTX,
	// New, and Ptr; a policy parameter whose Go name lands on one of
	// them must be a diagnostic, not a file that fails to compile.
	q := scanOne(t, `-- name: CountLogs :one
SELECT count(*) AS total FROM audit_logs WHERE audit_logs.queries = :queries;
`)
	q.Params["queries"].Policy = "quota_scope"

	_, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
		Q: q, Frags: BuildFrags(postgres.Profile{}, q),
		Columns:    []dialect.ColumnDesc{{Name: "total", Type: dialect.TypeRef{OID: 20}}},
		Nullable:   []bool{false},
		ParamTypes: map[string]dialect.TypeRef{"queries": {OID: 20}},
	}})
	if !hasCode(diags, diagnostics.CodeNameCollision) {
		t.Fatalf("reserved policy type name must be a collision, got %+v", diags)
	}
}
