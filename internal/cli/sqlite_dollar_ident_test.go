package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

const sqliteDollarYAML = `version: 1
dialect: sqlite
server_version: "3"
schema:
  files: [db/schema.sql]
targets:
  - queries: [queries/*.sql]
    output:
      package: gen
      path: gen
`

const sqliteDollarSchema = "CREATE TABLE acct$1 (id INTEGER PRIMARY KEY, a$b TEXT NOT NULL, a_b TEXT);\n" +
	"CREATE TABLE orders (id INTEGER PRIMARY KEY, tenant_id INTEGER NOT NULL, s$ TEXT);"

// `$` inside unquoted SQLite identifiers (the engine accepts them; the
// rqlite frontend needs the dollar_ident.go rewrite) goes through the
// whole in-process pipeline: the analysis sees `a$b` and `a_b` as the
// distinct columns they are (NOT NULL vs nullable), and the template
// text reaching the engine keeps the original names.
func TestRun_SQLiteDollarIdentifiers(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{
		"sqletch.yaml":  sqliteDollarYAML,
		"db/schema.sql": sqliteDollarSchema,
		"queries/q.sql": "-- name: ListAcct :many\n-- @param id: integer\n" +
			"SELECT x$.a$b AS ab, x$.a_b FROM acct$1 AS x$ WHERE x$.id > :id ORDER BY x$.id LIMIT 10;\n",
	})
	res, err := Run(context.Background(), cfg, ModeGenerate, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("diagnostics: %+v", res.Diags)
	}
	b, err := os.ReadFile(filepath.Join(cfg.Dir, "gen", "list_acct.sql.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{"Ab string", "AB optional.Option[string]", "FROM acct$1 AS x$ WHERE x$.id > "} {
		if !strings.Contains(strings.Join(strings.Fields(src), " "), want) {
			t.Errorf("generated code lacks %q:\n%s", want, src)
		}
	}
}

// A `$` result-column name has no Go field spelling that cannot collide
// (`a$b` vs `a_b`): codegen keeps refusing it (SQLETCH307, alias hint).
// A `$` alias bound to a policy placeholder keeps SQLETCH125 (no quoting
// facility, doc 14 §11.3) — the frontend workaround must not widen what
// the weaver accepts.
func TestRun_SQLiteDollarIdentifierLimits(t *testing.T) {
	for _, tc := range []struct{ name, yaml, query, code string }{
		{"result column", sqliteDollarYAML,
			"-- name: ListAcct :many\nSELECT a$b FROM acct$1 LIMIT 10;\n", "SQLETCH307"},
		{"policy alias", sqliteDollarYAML + "policies:\n  - name: tenant_scope\n    tables: [orders]\n    predicate: \"{}.tenant_id = :tenant_id\"\n    param:\n      name: tenant_id\n      type: integer\n",
			"-- name: ListOrders :many\n-- @param s: text\nSELECT o$.id FROM orders AS o$ WHERE o$.s$ = :s ORDER BY o$.id LIMIT 10;\n", "SQLETCH125"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writeOfflineProject(t, map[string]string{
				"sqletch.yaml":  tc.yaml,
				"db/schema.sql": sqliteDollarSchema,
				"queries/q.sql": tc.query,
			})
			res, err := Run(context.Background(), cfg, ModeGenerate, RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !hasCode(res.Diags, diagnostics.Code(tc.code)) {
				t.Fatalf("want %s, got %+v", tc.code, res.Diags)
			}
		})
	}
}
