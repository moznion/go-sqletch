package cli

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/template"
)

// writeOfflineProject lays out a minimal postgres project and returns
// the loaded config. Extra template files come from the files map
// (path relative to the project root).
func writeOfflineProject(t *testing.T, files map[string]string) config.Config {
	t.Helper()
	dir := t.TempDir()
	base := map[string]string{
		"db/schema.sql": "CREATE TABLE t (id bigint NOT NULL, x text);\n" +
			"CREATE TABLE u (id bigint NOT NULL, name text, score bigint);",
		"sqletch.yaml": `version: 1
dialect: postgres
server_version: "16"
schema:
  files: [db/schema.sql]
queries: [queries/*.sql]
output:
  package: gen
  path: gen
`,
	}
	maps.Copy(base, files)
	for name, content := range base {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, diags := config.Load(filepath.Join(dir, "sqletch.yaml"))
	if len(diags) > 0 {
		t.Fatalf("config diags: %v", diags)
	}
	return cfg
}

const validQuery = `-- name: FindT :many
SELECT t.id FROM t
WHERE TRUE
@if-present(x)
  AND t.x = :x
@endif
;
`

// unanchored WHERE: every conjunct optional (R6, SQLETCH113).
const unanchoredQuery = `-- name: BadAnchor :many
SELECT t.id FROM t
WHERE
@if-present(x)
  AND t.x = :x
@endif
;
`

func hasCode(diags []diagnostics.Diagnostic, code diagnostics.Code) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

func anyCode(res WorkspaceCheck, code diagnostics.Code) bool {
	for _, ds := range res.Diags {
		if hasCode(ds, code) {
			return true
		}
	}
	return false
}

// Cold cache: scanner + lexical + R1 diagnostics are reported, and the
// good file stays clean; oracle-dependent passes stay silent.
func TestOfflineChecker_ColdCache(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{
		"queries/a.sql": validQuery,
		"queries/b.sql": unanchoredQuery,
	})
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	aPath := filepath.Join(cfg.Dir, "queries", "a.sql")
	bPath := filepath.Join(cfg.Dir, "queries", "b.sql")
	if ds := res.Diags[aPath]; len(ds) != 0 {
		t.Errorf("a.sql should be clean, got %v", ds)
	}
	if !hasCode(res.Diags[bPath], diagnostics.CodeUnanchoredClause) {
		t.Errorf("b.sql must carry SQLETCH113, got %v", res.Diags[bPath])
	}
	if res.Files[aPath] == nil || len(res.Files[aPath].Queries) != 1 {
		t.Errorf("scan result for a.sql missing: %v", res.Files[aPath])
	}
	if string(res.Sources[bPath]) != unanchoredQuery {
		t.Errorf("Sources must carry analyzed content")
	}
}

// Duplicate query names across files: the first definition in sorted
// path order wins, the later one is flagged (same as the pipeline).
func TestOfflineChecker_DuplicateNames(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{
		"queries/c1.sql": validQuery,
		"queries/c2.sql": validQuery,
	})
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	c1 := filepath.Join(cfg.Dir, "queries", "c1.sql")
	c2 := filepath.Join(cfg.Dir, "queries", "c2.sql")
	if hasCode(res.Diags[c1], diagnostics.CodeDuplicateQueryName) {
		t.Errorf("first definition must not be flagged: %v", res.Diags[c1])
	}
	if !hasCode(res.Diags[c2], diagnostics.CodeDuplicateQueryName) {
		t.Errorf("second definition must carry SQLETCH004: %v", res.Diags[c2])
	}
}

// Overlay content replaces disk content, and overlay-only paths (an
// unsaved buffer) are analyzed too.
func TestOfflineChecker_Overlay(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{
		"queries/a.sql": validQuery,
	})
	checker := NewOfflineChecker(cfg)
	aPath := filepath.Join(cfg.Dir, "queries", "a.sql")

	res, err := checker.Check(map[string][]byte{aPath: []byte(unanchoredQuery)})
	if err != nil {
		t.Fatal(err)
	}
	if !hasCode(res.Diags[aPath], diagnostics.CodeUnanchoredClause) {
		t.Errorf("overlay must override disk: %v", res.Diags[aPath])
	}

	res, err = checker.Check(map[string][]byte{aPath: []byte(validQuery)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diags[aPath]) != 0 {
		t.Errorf("fixed overlay must be clean: %v", res.Diags[aPath])
	}

	// A buffer for a file the globs do not know yet (headerless SQL:
	// SQLETCH003).
	newPath := filepath.Join(cfg.Dir, "queries", "new.sql")
	res, err = checker.Check(map[string][]byte{newPath: []byte("SELECT 1;\n")})
	if err != nil {
		t.Fatal(err)
	}
	if !hasCode(res.Diags[newPath], diagnostics.CodeMissingHeader) {
		t.Errorf("overlay-only path must be analyzed: %v", res.Diags[newPath])
	}
}

// A skeleton reference into a guarded LEFT JOIN (R3, SQLETCH115) is
// only detectable with the catalog-dependent pass: silent on a cold
// cache, reported once the committed cache holds the catalog and every
// rendering, and silent again when any rendering misses.
func TestOfflineChecker_WarmCacheEnablesResolution(t *testing.T) {
	// The @choose gives the query a second verified rendering, so the
	// "partial" case below has an entry to withhold.
	const scoped = `-- name: FindUser :many
SELECT t.id, u.name FROM t
@if-present(min)
  LEFT JOIN u ON u.id = t.id AND u.score > :min
@endif
WHERE TRUE
@choose(sort)
@case(id_desc)
ORDER BY t.id DESC
@default
ORDER BY t.id ASC
@end
;
`
	run := func(t *testing.T, entries string) WorkspaceCheck {
		t.Helper()
		cfg := writeOfflineProject(t, map[string]string{"queries/q.sql": scoped})
		warmCache(t, cfg, "queries/q.sql", entries)
		res, err := NewOfflineChecker(cfg).Check(nil)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	if res := run(t, "none"); anyCode(res, diagnostics.CodeScopeViolation) {
		t.Errorf("cold cache must not run the resolution pass: %v", res.Diags)
	}
	if res := run(t, "all"); !anyCode(res, diagnostics.CodeScopeViolation) {
		t.Errorf("warm cache must surface SQLETCH115: %v", res.Diags)
	}
	if res := run(t, "partial"); anyCode(res, diagnostics.CodeScopeViolation) {
		t.Errorf("a partial cache must not run the resolution pass: %v", res.Diags)
	}
}

// warmCache commits a catalog and (per entries: "none" | "partial" |
// "all") oracle entries for the renderings of the single query in
// relPath, without any database.
func warmCache(t *testing.T, cfg config.Config, relPath, entries string) {
	t.Helper()
	if entries == "none" {
		return
	}
	schemaPaths, err := cfg.ExpandGlobs(cfg.Schema.Files)
	if err != nil {
		t.Fatal(err)
	}
	var schemaFiles []cache.SchemaFile
	for _, p := range schemaPaths {
		content, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(cfg.Dir, p)
		schemaFiles = append(schemaFiles, cache.SchemaFile{Path: rel, Content: content})
	}
	fp := cache.Fingerprint(cfg.Dialect, cfg.ServerVersion, schemaFiles)
	store := cache.NewStore(cfg.Abs(cfg.Cache.Path))
	if err := store.SaveCatalog(&cache.Catalog{SchemaFP: fp, Tables: []cache.Table{
		{Schema: "public", Name: "t", OID: 101, Cols: []cache.Column{
			{Name: "id", Att: 1, TypeOID: 20, TypeName: "int8", NotNull: true},
			{Name: "x", Att: 2, TypeOID: 25, TypeName: "text"},
		}},
		{Schema: "public", Name: "u", OID: 102, Cols: []cache.Column{
			{Name: "id", Att: 1, TypeOID: 20, TypeName: "int8", NotNull: true},
			{Name: "name", Att: 2, TypeOID: 25, TypeName: "text"},
			{Name: "score", Att: 3, TypeOID: 20, TypeName: "int8"},
		}},
	}}); err != nil {
		t.Fatal(err)
	}

	drv := driverFor(cfg)
	src, err := os.ReadFile(cfg.Abs(relPath))
	if err != nil {
		t.Fatal(err)
	}
	file, diags := template.NewScanner(drv.profile).ScanFile(cfg.Abs(relPath), src)
	if diagnostics.HasErrors(diags) || len(file.Queries) != 1 {
		t.Fatalf("test template must scan cleanly: %v", diags)
	}
	rs, err := ast.Renderings(drv.profile, file.Queries[0])
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range rs {
		if entries == "partial" && i > 0 {
			break
		}
		desc := dialect.Desc{Columns: []dialect.ColumnDesc{
			{Name: "id", Type: dialect.TypeRef{OID: 20, Name: "int8"}, SrcRel: 101, SrcAtt: 1},
			{Name: "name", Type: dialect.TypeRef{OID: 25, Name: "text"}, SrcRel: 102, SrcAtt: 2},
		}}
		if strings.Contains(r.SQL, "$1") {
			desc.Params = []dialect.TypeRef{{OID: 20, Name: "int8"}}
		}
		if err := store.SaveOracle(dialect.EntryFromDesc(fp, r.SQL, desc)); err != nil {
			t.Fatal(err)
		}
	}
}
