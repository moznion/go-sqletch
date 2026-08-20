package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

// ---- class (a): required argument shadows imports / package-level vars ----

// A required argument shares the query method's outermost scope with the
// body's imports (runtime, context, time, …) and the per-query package
// vars it references by name (<query>Frags / <query>Shapes). An argument
// spelled like one of those resolves the body's selector/var reference
// to the argument instead, producing generated Go that does not compile.
// argIdent must suffix those away, exactly as it already does for body
// locals.
func TestArgIdent_ShadowsImportsAndPackageVars(t *testing.T) {
	cases := []struct{ param, query, want string }{
		// imported package identifiers
		{"runtime", "Pick", "runtimeArg"},
		{"context", "Pick", "contextArg"},
		{"time", "Pick", "timeArg"},
		{"fmt", "Pick", "fmtArg"},
		{"errors", "Pick", "errorsArg"},
		{"optional", "Pick", "optionalArg"},
		{"sql", "Pick", "sqlArg"},
		{"pgx", "Pick", "pgxArg"},
		// per-query package vars (frags/shapes table)
		{"pick_frags", "Pick", "pickFragsArg"},
		{"pick_shapes", "Pick", "pickShapesArg"},
		// the var names are query-relative: unrelated queries pass through
		{"pick_frags", "Other", "pickFrags"},
		// ordinary names still pass through unchanged
		{"tenant_id", "Pick", "tenantID"},
		{"scope", "Pick", "scope"},
		{"runtimes", "Pick", "runtimes"},
	}
	for _, c := range cases {
		if got := argIdent(c.param, c.query); got != c.want {
			t.Errorf("argIdent(%q, %q) = %q, want %q", c.param, c.query, got, c.want)
		}
	}
}

// End-to-end: a @filter-tree!(runtime) whose required argument would be
// spelled `runtime` must be renamed so the body's `var key
// runtime.ShapeKey` still resolves the import, not the argument.
func TestGenerate_RequiredArgAvoidsImportCollision(t *testing.T) {
	q := scanOne(t, `-- name: Pick :many
SELECT t.id FROM t
WHERE TRUE
  AND @filter-tree!(runtime)
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
	if !strings.Contains(src, "runtimeArg runtime.Tree") {
		t.Errorf("required tree arg not renamed away from the runtime import\n----\n%s", src)
	}
	if regexp.MustCompile(`\bruntime runtime\.Tree\b`).MatchString(src) {
		t.Errorf("required tree arg still shadows the runtime import\n----\n%s", src)
	}
}

// ---- class (b): collision with package-level generated names ----

func TestGenerate_ReservedPackageName(t *testing.T) {
	// A policy parameter `and` generates `type And …`, which collides with
	// the package-level `var And = runtime.And`.
	t.Run("policy_and", func(t *testing.T) {
		q := scanOne(t, `-- name: CountLogs :one
SELECT count(*) AS total FROM audit_logs WHERE audit_logs.and_col = :and;
`)
		q.Params["and"].Policy = "scope"
		_, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
			Q: q, Frags: BuildFrags(postgres.Profile{}, q),
			Columns:    []dialect.ColumnDesc{{Name: "total", Type: dialect.TypeRef{OID: 20}}},
			Nullable:   []bool{false},
			ParamTypes: map[string]dialect.TypeRef{"and": {OID: 20}},
		}})
		if !hasCode(diags, diagnostics.CodeInvalidColumnIdentifier) {
			t.Fatalf("policy param `and` must collide with var And (SQLETCH307), got %+v", diags)
		}
	})

	// A query `Shape` with `@choose(space)` claims enum type `ShapeSpace`,
	// which collides with the package-level `var ShapeSpace`.
	t.Run("choose_shape_space", func(t *testing.T) {
		q := scanOne(t, `-- name: Shape :many
SELECT t.id FROM t
@choose(space)
@case(a)
ORDER BY t.id
@default
ORDER BY t.id DESC
@end;
`)
		_, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
			Q: q, Frags: BuildFrags(postgres.Profile{}, q),
			Columns:    []dialect.ColumnDesc{{Name: "id", Type: dialect.TypeRef{OID: 20}}},
			Nullable:   []bool{false},
			ParamTypes: map[string]dialect.TypeRef{},
		}})
		if !hasCode(diags, diagnostics.CodeInvalidColumnIdentifier) {
			t.Fatalf("enum ShapeSpace must collide with var ShapeSpace (SQLETCH307), got %+v", diags)
		}
	})
}

// ---- class (c): query name collides with a *Queries method ----

func TestGenerate_QueryNameCollidesQueriesMethod(t *testing.T) {
	for _, name := range []string{"Cache", "OnQuery", "SetObserver", "WithTx", "hook", "observeExec"} {
		q := scanOne(t, "-- name: "+name+" :many\nSELECT t.id FROM t;\n")
		_, diags := Generate(Options{Package: "gen"}, postgres.TypeMap{}, []QueryInput{{
			Q: q, Frags: BuildFrags(postgres.Profile{}, q),
			Columns:    []dialect.ColumnDesc{{Name: "id", Type: dialect.TypeRef{OID: 20}}},
			Nullable:   []bool{false},
			ParamTypes: map[string]dialect.TypeRef{},
		}})
		if !hasCode(diags, diagnostics.CodeInvalidColumnIdentifier) {
			t.Errorf("query %q collides with a *Queries method (want SQLETCH307), got %+v", name, diags)
		}
	}
}

// ---- class (d): a column that maps to an unexported field ----

func TestGenerate_UnexportedColumnField(t *testing.T) {
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
	// A column whose Go name is a valid identifier but NOT exported: the
	// consumer in another package cannot read the field.
	for _, bad := range []string{"日本語", "עברית"} {
		if !hasCode(gen(bad), diagnostics.CodeInvalidColumnIdentifier) {
			t.Errorf("column %q maps to an unexported field; want SQLETCH307, got %+v", bad, gen(bad))
		}
	}
}

// ---- compile proof for the rename (class a) ----

// The renamed arguments must actually compile. Generate a package that
// exercises the arg-shadow rename (a @filter-tree!(runtime) argument),
// materialize it as a standalone module, and `go build` it.
func TestGenerate_ArgShadowCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("compile test skipped in -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	q := scanOne(t, `-- name: Pick :many
SELECT t.id FROM t
WHERE TRUE
  AND @filter-tree!(runtime)
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

	dir := t.TempDir()
	genDir := filepath.Join(dir, "gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(genDir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	parentMod, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the parent module's COMPLETE (pruned) require graph rather than a
	// hand-written minimal one. A minimal go.mod forces -mod=mod to recompute
	// the module graph, at which point pgx's transitive github.com/jackc/pgpassfile
	// pulls in its own test-only github.com/stretchr/testify@v1.3.0 requirement;
	// with GOPROXY=off (CI has no module-cache entry for a version the parent
	// never selects) that lookup fails. Carrying the parent's require block pins
	// testify to the version the parent already selects, so a read-only build
	// resolves everything from the module cache offline.
	moduleLine := regexp.MustCompile(`(?m)^module .*$`)
	childMod := moduleLine.ReplaceAllString(string(parentMod), "module sqletchgen")
	childMod += "\nrequire github.com/moznion/go-sqletch v0.0.0\n" +
		"replace github.com/moznion/go-sqletch => " + repoRoot + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(childMod), 0o644); err != nil {
		t.Fatal(err)
	}
	parentSum, err := os.ReadFile(filepath.Join(repoRoot, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), parentSum, 0o644); err != nil {
		t.Fatal(err)
	}

	// Read-only build (the default): the require graph above is complete, so no
	// module resolution is attempted and GOPROXY=off keeps it hermetic/offline.
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=readonly", "GOPROXY=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated module failed to build: %v\n%s", err, out)
	}
}
