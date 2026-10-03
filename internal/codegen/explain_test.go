package codegen

import (
	"regexp"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
	"github.com/moznion/go-sqletch/internal/template"
	"github.com/moznion/go-sqletch/runtime"
)

// methodBody returns the body lines of `func (q *Queries) <name>(`.
func methodBody(t *testing.T, src, name string) []string {
	t.Helper()
	start := strings.Index(src, "func (q *Queries) "+name+"(")
	if start < 0 {
		t.Fatalf("method %s not found in\n%s", name, src)
	}
	rest := src[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("method %s unterminated", name)
	}
	lines := strings.Split(rest[:end], "\n")
	return lines[1:]
}

// preamble keeps the lines a method runs before it talks to the
// database, minus the rejection branches' bodies — the only place the
// query method (observeReject + its own zero value) and the explain
// method (Plan{}) legitimately differ.
func preamble(body []string, stop ...string) []string {
	var out []string
	for _, l := range body {
		for _, s := range stop {
			if strings.HasPrefix(strings.TrimSpace(l), s) {
				return out
			}
		}
		tl := strings.TrimSpace(l)
		if strings.HasPrefix(tl, "q.observeReject(") || strings.HasPrefix(tl, "return ") ||
			strings.HasPrefix(tl, "var zero ") {
			continue
		}
		out = append(out, l)
	}
	return out
}

func TestGenerate_ExplainMethod(t *testing.T) {
	files := generateUC1(t)
	src := string(files["search_users.sql.gen.go"])
	if !regexp.MustCompile(`func \(q \*Queries\) ExplainSearchUsers\(ctx context\.Context, arg SearchUsersParams, opts runtime\.ExplainOptions\) \(runtime\.Plan, error\) \{`).MatchString(src) {
		t.Fatalf("ExplainSearchUsers signature missing:\n%s", src)
	}
	body := methodBody(t, src, "ExplainSearchUsers")
	joined := strings.Join(body, "\n")
	for _, banned := range []string{"q.hook", "observeExec", "observeReject", "q.db."} {
		if strings.Contains(joined, banned) {
			t.Errorf("explain body must not call %s:\n%s", banned, joined)
		}
	}
	if last := strings.TrimSpace(body[len(body)-1]); last != `return q.explain(ctx, "SearchUsers", key.String(), opts, sqlText, args)` {
		t.Errorf("explain tail = %q", last)
	}
	if !strings.Contains(joined, `return runtime.Plan{}, fmt.Errorf("SearchUsers: %w", err)`) {
		t.Errorf("@choose rejection must return the wrapped error:\n%s", joined)
	}
	if strings.Contains(string(files["querier.gen.go"]), "Explain") {
		t.Errorf("Explain methods must stay out of Querier (mock compatibility):\n%s", files["querier.gen.go"])
	}
	db := string(files["db.gen.go"])
	for _, want := range []string{
		"func (q *Queries) explain(ctx context.Context, query, shapeKey string, opts runtime.ExplainOptions, sqlText string, args []any) (plan runtime.Plan, err error) {",
		"runtime.ExplainStatement(runtime.ExplainPostgres, opts, sqlText)",
		"Begin(context.Context) (pgx.Tx, error)",
		"runtime.ErrExplainNoTx",
		"tx.Rollback(context.WithoutCancel(ctx))",
	} {
		if !strings.Contains(db, want) {
			t.Errorf("db.gen.go missing %q:\n%s", want, db)
		}
	}
}

// TestGenerate_ExplainPreambleIdentity pins design 22 §4: the explain
// method composes exactly what the query method composes, for every
// preamble kind (plain, guards, @choose/@order-by, @when, filter-tree,
// filter-tree!, policy-required args, static expansion, question style).
func TestGenerate_ExplainPreambleIdentity(t *testing.T) {
	type gen struct {
		name  string
		files map[string][]byte
	}
	var gens []gen
	for _, src := range conformanceCorpus {
		q := scanOne(t, src)
		types := map[string]dialect.TypeRef{}
		for _, name := range q.ParamOrder {
			types[name] = dialect.TypeRef{OID: 20}
		}
		in := QueryInput{
			Q: q, Frags: BuildFrags(postgres.Profile{}, q), ParamTypes: types,
			Columns: []dialect.ColumnDesc{{Name: "id", Type: dialect.TypeRef{OID: 20}}}, Nullable: []bool{false},
		}
		files, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{in})
		if diagnostics.HasErrors(diags) {
			t.Fatalf("%s: %+v", q.Name, diags)
		}
		gens = append(gens, gen{q.Name, files})

		// The same query under strict static expansion.
		shapes := map[string]runtime.Expanded{}
		sqlText, idx := runtime.Compose(in.Frags, runtime.ShapeKey{})
		shapes[runtime.ShapeKey{}.String()] = runtime.Expanded{SQL: sqlText, ArgIdx: idx}
		in.ExpandedShapes = shapes
		files, diags = Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{in})
		if diagnostics.HasErrors(diags) {
			t.Fatalf("%s expanded: %+v", q.Name, diags)
		}
		gens = append(gens, gen{q.Name, files})
	}
	{
		q := scanOne(t, `-- name: Pick :many
SELECT t.id FROM t
WHERE TRUE
  AND @filter-tree!(scope)
@predicate(tenant)
t.tenant_id = :tenant_id
@end;
`)
		files, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
			Q: q, Frags: BuildFrags(postgres.Profile{}, q),
			Columns:    []dialect.ColumnDesc{{Name: "id", Type: dialect.TypeRef{OID: 20}}},
			Nullable:   []bool{false},
			ParamTypes: map[string]dialect.TypeRef{"tenant_id": {OID: 20}},
		}})
		if diagnostics.HasErrors(diags) {
			t.Fatalf("filter-tree!: %+v", diags)
		}
		src := string(files["pick.sql.gen.go"])
		if !strings.Contains(src, "func (q *Queries) ExplainPick(ctx context.Context, scope runtime.Tree, arg PickParams, opts runtime.ExplainOptions) (runtime.Plan, error) {") {
			t.Errorf("required tree must stay a required argument of the explain method:\n%s", src)
		}
		if !strings.Contains(src, "\tkey.Trees = []string{scope.Encode()}\n\treturn q.explain(") {
			t.Errorf("explain must report the tree's key segment:\n%s", src)
		}
		gens = append(gens, gen{q.Name, files})
	}
	{
		f, ds := template.NewScanner(mysql.Profile{}).ScanFile("t.sql", []byte(inListTemplate))
		if len(ds) != 0 {
			t.Fatal(ds)
		}
		q := f.Queries[0]
		q.Params["min_id"].Optional = true
		tm := mysql.TypeMap{}
		big, _ := tm.TypeByName("bigint")
		vc, _ := tm.TypeByName("varchar(16)")
		files, diags := Generate(Options{Package: "gen", Style: runtime.StyleQuestion, Explain: runtime.ExplainMySQL}, tm, []QueryInput{{
			Q: q, Frags: BuildFrags(mysql.Profile{}, q),
			ParamTypes: map[string]dialect.TypeRef{"tenant_id": big, "statuses": vc, "min_id": big, "limit": big},
			Columns:    []dialect.ColumnDesc{{Name: "id", Type: big}, {Name: "email", Type: vc}},
			Nullable:   []bool{false, false},
		}})
		if diagnostics.HasErrors(diags) {
			t.Fatalf("question: %+v", diags)
		}
		gens = append(gens, gen{q.Name, files})
	}

	for _, g := range gens {
		src := string(g.files[pascalToSnake(g.name)+".sql.gen.go"])
		qp := preamble(methodBody(t, src, g.name), "q.hook(", "q.hookTree(")
		ep := preamble(methodBody(t, src, "Explain"+g.name), "key.Trees = ")
		if strings.Join(qp, "\n") != strings.Join(ep, "\n") {
			t.Errorf("%s: preambles differ\nquery:\n%s\nexplain:\n%s", g.name, strings.Join(qp, "\n"), strings.Join(ep, "\n"))
		}
		if len(qp) == 0 || !strings.Contains(strings.Join(qp, "\n"), "args := ") {
			t.Errorf("%s: preamble extraction is vacuous: %q", g.name, qp)
		}
	}
}

func TestGenerate_ExplainNameCollision(t *testing.T) {
	foo := scanOne(t, "-- name: Foo :many\nSELECT t.id FROM t;\n")
	efoo := scanOne(t, "-- name: ExplainFoo :many\nSELECT t.id FROM t;\n")
	var ins []QueryInput
	for _, q := range []*template.QueryTemplate{efoo, foo} {
		ins = append(ins, QueryInput{
			Q: q, Frags: BuildFrags(postgres.Profile{}, q),
			Columns:    []dialect.ColumnDesc{{Name: "id", Type: dialect.TypeRef{OID: 20}}},
			Nullable:   []bool{false},
			ParamTypes: map[string]dialect.TypeRef{},
		})
	}
	_, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, ins)
	if !hasCode(diags, diagnostics.CodeNameCollision) {
		t.Fatalf("Foo's ExplainFoo collides with query ExplainFoo (want SQLETCH310), got %+v", diags)
	}
	var found bool
	for _, d := range diags {
		if d.Code == diagnostics.CodeNameCollision && d.Span == efoo.HeaderSpan && strings.Contains(d.Message, "ExplainFoo") {
			found = true
		}
	}
	if !found {
		t.Errorf("collision must point at the query literally named ExplainFoo: %+v", diags)
	}
}

func TestGenerate_ExplainHelperNamesReserved(t *testing.T) {
	for _, name := range []string{"explain", "explainTx"} {
		q := scanOne(t, "-- name: "+name+" :many\nSELECT t.id FROM t;\n")
		_, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
			Q: q, Frags: BuildFrags(postgres.Profile{}, q),
			Columns:    []dialect.ColumnDesc{{Name: "id", Type: dialect.TypeRef{OID: 20}}},
			Nullable:   []bool{false},
			ParamTypes: map[string]dialect.TypeRef{},
		}})
		if !hasCode(diags, diagnostics.CodeInvalidColumnIdentifier) {
			t.Errorf("query %q collides with a db.gen.go explain helper (want SQLETCH307), got %+v", name, diags)
		}
	}
}

func TestArgIdent_OptsReserved(t *testing.T) {
	if got := argIdent("opts", "Q"); got != "optsArg" {
		t.Errorf("argIdent(opts) = %q, want optsArg (the explain method's trailing parameter)", got)
	}
}

func TestGenerate_ExplainDialectFlavors(t *testing.T) {
	one := func(t *testing.T, opts Options, tm dialect.TypeMap, profile dialect.LexerProfile) (map[string][]byte, []diagnostics.Diagnostic) {
		t.Helper()
		f, ds := template.NewScanner(profile).ScanFile("t.sql", []byte("-- name: Ids :many\nSELECT t.id FROM t;\n"))
		if len(ds) != 0 {
			t.Fatal(ds)
		}
		q := f.Queries[0]
		ref := dialect.TypeRef{OID: 20}
		if byName, ok := tm.(interface {
			TypeByName(string) (dialect.TypeRef, bool)
		}); ok && opts.Style == runtime.StyleQuestion {
			ref, _ = byName.TypeByName("integer")
			if r, ok := byName.TypeByName("bigint"); ok {
				ref = r
			}
		}
		return Generate(opts, tm, []QueryInput{{
			Q: q, Frags: BuildFrags(profile, q),
			Columns:  []dialect.ColumnDesc{{Name: "id", Type: ref}},
			Nullable: []bool{false}, ParamTypes: map[string]dialect.TypeRef{},
		}})
	}

	t.Run("mysql", func(t *testing.T) {
		files, diags := one(t, Options{Package: "gen", Style: runtime.StyleQuestion, Explain: runtime.ExplainMySQL}, mysql.TypeMap{}, mysql.Profile{})
		if diagnostics.HasErrors(diags) {
			t.Fatal(diags)
		}
		db := string(files["db.gen.go"])
		for _, want := range []string{
			"runtime.ExplainStatement(runtime.ExplainMySQL, opts, sqlText)",
			`"SAVEPOINT sqletch_explain"`, `"ROLLBACK TO SAVEPOINT sqletch_explain"`, `"RELEASE SAVEPOINT sqletch_explain"`,
			"BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)",
			"runtime.ErrExplainNoTx",
		} {
			if !strings.Contains(db, want) {
				t.Errorf("mysql db.gen.go missing %q:\n%s", want, db)
			}
		}
	})
	t.Run("sqlite", func(t *testing.T) {
		files, diags := one(t, Options{Package: "gen", Style: runtime.StyleQuestion, Explain: runtime.ExplainSQLite}, sqlite.TypeMap{}, sqlite.Profile{})
		if diagnostics.HasErrors(diags) {
			t.Fatal(diags)
		}
		db := string(files["db.gen.go"])
		for _, want := range []string{
			"runtime.ExplainStatement(runtime.ExplainSQLite, opts, sqlText)",
			"runtime.SQLitePlan(",
		} {
			if !strings.Contains(db, want) {
				t.Errorf("sqlite db.gen.go missing %q:\n%s", want, db)
			}
		}
		// ExplainStatement refuses ANALYZE on SQLite before any DB work,
		// so no transaction machinery is emitted.
		for _, banned := range []string{"SAVEPOINT", "BeginTx", "explainTx"} {
			if strings.Contains(db, banned) {
				t.Errorf("sqlite db.gen.go must not carry %q:\n%s", banned, db)
			}
		}
	})
	t.Run("question style needs a dialect", func(t *testing.T) {
		_, diags := one(t, Options{Package: "gen", Style: runtime.StyleQuestion}, mysql.TypeMap{}, mysql.Profile{})
		if !diagnostics.HasErrors(diags) {
			t.Fatal("question style without an explain dialect must not guess between MySQL and SQLite")
		}
	})
	t.Run("dollar style defaults to postgres", func(t *testing.T) {
		files, diags := one(t, Options{Package: "gen"}, postgres.TypeMap{}, postgres.Profile{})
		if diagnostics.HasErrors(diags) {
			t.Fatal(diags)
		}
		if !strings.Contains(string(files["db.gen.go"]), "runtime.ExplainPostgres") {
			t.Error("dollar style must default to the postgres explain helper")
		}
	})
	t.Run("style and dialect must agree", func(t *testing.T) {
		_, diags := one(t, Options{Package: "gen", Explain: runtime.ExplainMySQL}, postgres.TypeMap{}, postgres.Profile{})
		if !diagnostics.HasErrors(diags) {
			t.Fatal("dollar style with the MySQL explain dialect must be refused")
		}
	})
}

// TestGenerate_ExplainCompiles builds one package per explain flavor
// (pgx, MySQL database/sql, SQLite database/sql) carrying every
// preamble kind the identity test covers.
func TestGenerate_ExplainCompiles(t *testing.T) {
	pkgs := map[string]map[string][]byte{"pg": generateUC1(t)}

	f, ds := template.NewScanner(mysql.Profile{}).ScanFile("t.sql", []byte(inListTemplate))
	if len(ds) != 0 {
		t.Fatal(ds)
	}
	q := f.Queries[0]
	q.Params["min_id"].Optional = true
	mtm := mysql.TypeMap{}
	big, _ := mtm.TypeByName("bigint")
	vc, _ := mtm.TypeByName("varchar(16)")
	files, diags := Generate(Options{Package: "gen", Style: runtime.StyleQuestion, Explain: runtime.ExplainMySQL}, mtm, []QueryInput{{
		Q: q, Frags: BuildFrags(mysql.Profile{}, q),
		ParamTypes: map[string]dialect.TypeRef{"tenant_id": big, "statuses": vc, "min_id": big, "limit": big},
		Columns:    []dialect.ColumnDesc{{Name: "id", Type: big}, {Name: "email", Type: vc}},
		Nullable:   []bool{false, false},
	}})
	if diagnostics.HasErrors(diags) {
		t.Fatalf("mysql: %+v", diags)
	}
	pkgs["my"] = files

	f, ds = template.NewScanner(sqlite.Profile{}).ScanFile("t.sql", []byte(`-- name: Ids :many
SELECT t.id FROM t
WHERE TRUE
@if-present(min_id)
  AND t.id >= :min_id
@endif
;
`))
	if len(ds) != 0 {
		t.Fatal(ds)
	}
	q = f.Queries[0]
	q.Params["min_id"].Optional = true
	stm := sqlite.TypeMap{}
	integer, ok := stm.TypeByName("integer")
	if !ok {
		t.Fatal("sqlite integer type")
	}
	files, diags = Generate(Options{Package: "gen", Style: runtime.StyleQuestion, Explain: runtime.ExplainSQLite}, stm, []QueryInput{{
		Q: q, Frags: BuildFrags(sqlite.Profile{}, q),
		ParamTypes: map[string]dialect.TypeRef{"min_id": integer},
		Columns:    []dialect.ColumnDesc{{Name: "id", Type: integer}},
		Nullable:   []bool{false},
	}})
	if diagnostics.HasErrors(diags) {
		t.Fatalf("sqlite: %+v", diags)
	}
	pkgs["lite"] = files

	buildGenerated(t, pkgs)
}
