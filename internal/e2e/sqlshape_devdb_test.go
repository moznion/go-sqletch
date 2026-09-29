//go:build devdb

package e2e_test

// SQL-shape conformance harness: sqletch templates go through the real
// CLI pipeline (cold generate against the dialect's dev database), the
// generated package is compiled into a throwaway module, and every
// call in a case table runs against a RECORDING DBTX that captures the
// exact SQL text and bind arguments the generated code hands the
// driver — then fails the call, so no data is ever needed. The test
// asserts both byte-for-byte against the expectations written in the
// case table: the composed SQL is the user-visible contract of every
// template, and this pins it through the public generated API rather
// than through runtime.Compose directly (TestComposeConformance covers
// that seam).
//
// One generate and one `go run` per dialect: every case's query lives
// in its own file (so the skeleton's trailing bytes do not depend on
// its neighbors), and every call is compiled into the same main.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/cli"
)

// shapeCall is one invocation of a generated method.
type shapeCall struct {
	name string
	// call is a Go expression invoking a generated method; `q` is the
	// *gen.Queries and `ctx` a context. Any return arity is accepted —
	// the last value is taken as the error.
	call string
	// wantSQL is the exact SQL text the driver receives.
	wantSQL string
	// wantArgs are the bind arguments as rendered by fmtArg in the
	// harness main: pointers are dereferenced (nil → NULL), scalars
	// are `type(value)`, strings are quoted, slices are %#v.
	wantArgs []string
	// wantErr, when set, expects the call to be rejected before
	// reaching the driver with an error containing this text.
	wantErr string
}

// shapeCase is one query template with the calls exercising it.
type shapeCase struct {
	name  string // query file stem
	query string
	calls []shapeCall
}

// shapeDialect is everything dialect-specific about a suite run.
type shapeDialect struct {
	name          string // sqletch.yaml dialect
	serverVersion string
	// acquire returns the DSN for sqletch.yaml, given the project dir.
	acquire func(t *testing.T, ctx context.Context, dir string) (dsn string, cleanup func())
	// requires are module paths whose versions are copied from the
	// parent go.mod into the throwaway module.
	requires []string
	// recorder is Go source (imports excluded) defining `type recorder`
	// with note(sql string, args []any), and
	// `func newQueries(r *recorder) *gen.Queries`.
	recorder string
	imports  []string
}

type shapeSuite struct {
	dialect  shapeDialect
	schema   string
	policies string // optional `policies:` YAML block
	cases    []shapeCase
}

// shapeResult mirrors the harness main's JSON output.
type shapeResult struct {
	SQL   string   `json:"sql"`
	Hook  string   `json:"hook"`
	Args  []string `json:"args"`
	Calls int      `json:"calls"`
	Err   string   `json:"err"`
}

func runShapeSuite(t *testing.T, s shapeSuite) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dir := t.TempDir()
	dsn, cleanup := s.dialect.acquire(t, ctx, dir)
	defer cleanup()

	writeFile(t, dir, "db/schema.sql", s.schema)
	seen := map[string]bool{}
	for _, c := range s.cases {
		if seen[c.name] {
			t.Fatalf("duplicate case name %q", c.name)
		}
		seen[c.name] = true
		writeFile(t, dir, "queries/"+c.name+".sql", c.query)
	}
	writeFile(t, dir, "sqletch.yaml", `version: 1
dialect: `+s.dialect.name+`
server_version: "`+s.dialect.serverVersion+`"
database:
  dsn: `+dsn+`
schema:
  files: [db/schema.sql]
targets:
  - queries: [queries/*.sql]
    output:
      package: gen
      path: gen
cache:
  path: .sqletch/cache
`+s.policies)

	var out, errW bytes.Buffer
	if code := cli.Generate(ctx, filepath.Join(dir, "sqletch.yaml"), false,
		cli.RunOptions{AllowDestructive: true}, &out, &errW); code != cli.ExitOK {
		t.Fatalf("generate: exit %d\n%s%s", code, out.String(), errW.String())
	}

	writeShapeModule(t, dir, s)
	cmd := func(args ...string) string {
		c := exec.CommandContext(ctx, "go", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("go %s failed: %v\n%s", strings.Join(args, " "), err, b)
		}
		return string(b)
	}
	cmd("mod", "tidy")
	cmd("run", ".", "results.json")

	raw, err := os.ReadFile(filepath.Join(dir, "results.json"))
	if err != nil {
		t.Fatal(err)
	}
	var results []shapeResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatal(err)
	}

	total := 0
	for _, c := range s.cases {
		total += len(c.calls)
	}
	if total != len(results) {
		t.Fatalf("harness produced %d results for %d calls", len(results), total)
	}
	i := 0
	for _, c := range s.cases {
		t.Run(c.name, func(t *testing.T) {
			for _, call := range c.calls {
				got := results[i]
				i++
				t.Run(call.name, func(t *testing.T) { checkShapeCall(t, call, got) })
			}
		})
	}
}

func checkShapeCall(t *testing.T, call shapeCall, got shapeResult) {
	t.Helper()
	if call.wantErr != "" {
		if !strings.Contains(got.Err, call.wantErr) {
			t.Errorf("error = %q, want it to contain %q", got.Err, call.wantErr)
		}
		if got.Calls != 0 {
			t.Errorf("a rejected call must not reach the driver; it sent:\n%s", got.SQL)
		}
		return
	}
	if got.Err != "" {
		t.Fatalf("unexpected error: %s", got.Err)
	}
	if got.Calls != 1 {
		t.Fatalf("driver saw %d statements, want exactly 1", got.Calls)
	}
	if got.SQL != call.wantSQL {
		t.Errorf("SQL mismatch\n--- got (Go literal) ---\n%s\n--- want ---\n%s",
			strings.ReplaceAll(fmt.Sprintf("%q", got.SQL), `\n`, "\\n\"+\n\""),
			strings.ReplaceAll(fmt.Sprintf("%q", call.wantSQL), `\n`, "\\n\"+\n\""))
	}
	if got.Hook != got.SQL {
		t.Errorf("OnQuery hook saw different SQL than the driver:\nhook:   %q\ndriver: %q", got.Hook, got.SQL)
	}
	if strings.Join(got.Args, "\x00") != strings.Join(call.wantArgs, "\x00") {
		t.Errorf("args mismatch\n got: %#v\nwant: %#v", got.Args, call.wantArgs)
	}
}

func writeShapeModule(t *testing.T, dir string, s shapeSuite) {
	t.Helper()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	parentMod, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	var req strings.Builder
	for _, mod := range append([]string{"github.com/moznion/go-optional"}, s.dialect.requires...) {
		m := regexp.MustCompile(regexp.QuoteMeta(mod) + ` (v[0-9A-Za-z.\-+]+)`).FindStringSubmatch(string(parentMod))
		if m == nil {
			t.Fatalf("%s version not found in parent go.mod", mod)
		}
		req.WriteString("\t" + mod + " " + m[1] + "\n")
	}
	writeFile(t, dir, "go.mod", "module sqletchgen\n\ngo 1.24\n\nrequire (\n"+req.String()+
		"\tgithub.com/moznion/go-sqletch v0.0.0\n)\n\n"+
		"replace github.com/moznion/go-sqletch => "+repoRoot+"\n")
	parentSum, err := os.ReadFile(filepath.Join(repoRoot, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "go.sum", string(parentSum))

	var m strings.Builder
	m.WriteString("package main\n\nimport (\n")
	for _, imp := range append([]string{
		`"context"`, `"encoding/json"`, `"errors"`, `"fmt"`, `"os"`, `"reflect"`, `"strconv"`, `"time"`,
		`"github.com/moznion/go-optional"`,
		`"github.com/moznion/go-sqletch"`,
		`sqletchruntime "github.com/moznion/go-sqletch/runtime"`,
		`gen "sqletchgen/gen"`,
	}, s.dialect.imports...) {
		m.WriteString("\t" + imp + "\n")
	}
	m.WriteString(")\n")
	m.WriteString(shapeMainCommon)
	m.WriteString(s.dialect.recorder)
	m.WriteString("\nfunc main() {\n\tctx := context.Background()\n\trec := &recorder{}\n\tq := newQueries(rec)\n")
	m.WriteString("\tvar hook string\n\tq.OnQuery(func(_, s string) { hook = s })\n\tvar out []result\n")
	m.WriteString("\trun := func(f func() error) {\n\t\trec.sql, rec.args, rec.calls, hook = \"\", nil, 0, \"\"\n" +
		"\t\terr := f()\n\t\tr := result{SQL: rec.sql, Hook: hook, Calls: rec.calls, Args: []string{}}\n" +
		"\t\tfor _, a := range rec.args {\n\t\t\tr.Args = append(r.Args, fmtArg(a))\n\t\t}\n" +
		"\t\tif err != nil && !errors.Is(err, errRecorded) {\n\t\t\tr.Err = err.Error()\n\t\t}\n" +
		"\t\tout = append(out, r)\n\t}\n")
	for _, c := range s.cases {
		for _, call := range c.calls {
			fmt.Fprintf(&m, "\t// %s/%s\n\trun(func() error { return errOf(%s) })\n", c.name, call.name, call.call)
		}
	}
	m.WriteString("\tb, err := json.Marshal(out)\n\tif err != nil {\n\t\tpanic(err)\n\t}\n" +
		"\tif err := os.WriteFile(os.Args[1], b, 0o644); err != nil {\n\t\tpanic(err)\n\t}\n}\n")
	writeFile(t, dir, "main.go", m.String())
}

const shapeMainCommon = `
// Keep every common import used regardless of which cases exist.
var (
	_ = optional.None[int]
	_ = sqletch.Present[int]
	_ sqletchruntime.Tree
	_ = time.Time{}
)

var errRecorded = errors.New("sqletch-shape: statement recorded")

type result struct {
	SQL   string   ` + "`json:\"sql\"`" + `
	Hook  string   ` + "`json:\"hook\"`" + `
	Args  []string ` + "`json:\"args\"`" + `
	Calls int      ` + "`json:\"calls\"`" + `
	Err   string   ` + "`json:\"err\"`" + `
}

// errOf takes any generated method's results and returns the last one
// as the error.
func errOf(vs ...any) error {
	if len(vs) == 0 {
		return nil
	}
	err, _ := vs[len(vs)-1].(error)
	return err
}

func fmtArg(v any) string {
	rv := reflect.ValueOf(v)
	for rv.IsValid() && rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return "NULL"
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return "NULL"
	}
	x := rv.Interface()
	switch x := x.(type) {
	case time.Time:
		return "time.Time(" + x.UTC().Format(time.RFC3339Nano) + ")"
	case string:
		return "string(" + strconv.Quote(x) + ")"
	}
	if rv.Kind() == reflect.Slice {
		return fmt.Sprintf("%#v", x)
	}
	return fmt.Sprintf("%T(%v)", x, x)
}
`

// pgxRecorder satisfies the pgx-flavored generated DBTX directly.
const pgxRecorder = `
type recorder struct {
	sql   string
	args  []any
	calls int
}

func (r *recorder) note(sql string, args []any) { r.sql, r.args = sql, args; r.calls++ }

func (r *recorder) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.note(sql, args)
	return pgconn.CommandTag{}, errRecorded
}

func (r *recorder) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.note(sql, args)
	return nil, errRecorded
}

func (r *recorder) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	r.note(sql, args)
	return errRow{}
}

type errRow struct{}

func (errRow) Scan(...any) error { return errRecorded }

func newQueries(r *recorder) *gen.Queries { return gen.New(r) }
`

// sqlRecorder is a database/sql driver whose connection records the
// statement and fails it. CheckNamedValue accepts every value as-is,
// so the recorded args are exactly what the generated code passed.
const sqlRecorder = `
type recorder struct {
	sql   string
	args  []any
	calls int
}

func (r *recorder) note(sql string, nv []driver.NamedValue) {
	r.sql = sql
	r.args = make([]any, len(nv))
	for i, v := range nv {
		r.args[i] = v.Value
	}
	r.calls++
}

type recDriver struct{ r *recorder }

func (d recDriver) Open(string) (driver.Conn, error) { return recConn(d), nil }

type recConn struct{ r *recorder }

func (recConn) Prepare(string) (driver.Stmt, error)       { return nil, errRecorded }
func (recConn) Close() error                              { return nil }
func (recConn) Begin() (driver.Tx, error)                 { return nil, errRecorded }
func (recConn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (c recConn) QueryContext(_ context.Context, q string, nv []driver.NamedValue) (driver.Rows, error) {
	c.r.note(q, nv)
	return nil, errRecorded
}

func (c recConn) ExecContext(_ context.Context, q string, nv []driver.NamedValue) (driver.Result, error) {
	c.r.note(q, nv)
	return nil, errRecorded
}

func newQueries(r *recorder) *gen.Queries {
	sql.Register("sqletch-shape-recorder", recDriver{r})
	db, err := sql.Open("sqletch-shape-recorder", "")
	if err != nil {
		panic(err)
	}
	db.SetMaxOpenConns(1)
	return gen.New(db)
}
`
