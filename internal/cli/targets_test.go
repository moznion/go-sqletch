package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// writeFanOutProject lays out a two-app project whose single target
// captures the app directory, so one config generates one package per
// app — on the native MySQL backend, which needs no database at all.
// Both apps deliberately define a query of the SAME name: duplicate
// detection is scoped to the target, not the workspace.
func writeFanOutProject(t *testing.T, targets string) config.Config {
	t.Helper()
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("db/schema.sql", nativeCLISchema)
	mustWrite("app/signin/queries/q.sql", nativeCLIQuery)
	mustWrite("app/signup/queries/q.sql", nativeCLIQuery)
	if targets == "" {
		targets = `targets:
  - queries: ["(app/*)/queries/*.sql"]
    output:
      package: gen
      path: $1/gen
`
	}
	mustWrite("sqletch.yaml", `version: 1
dialect: mysql
server_version: "8.4"
database:
  oracle: native
schema:
  files: [db/schema.sql]
`+targets)
	cfg, diags := config.Load(filepath.Join(dir, "sqletch.yaml"))
	if diagnostics.HasErrors(diags) {
		t.Fatalf("config: %+v", diags)
	}
	return cfg
}

func mustGenerate(t *testing.T, cfg config.Config) *Result {
	t.Helper()
	res, err := Run(context.Background(), cfg, ModeGenerate, RunOptions{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("generate diagnostics: %+v", res.Diags)
	}
	return res
}

// One config, one capturing target, two generated packages — and the
// same query name in both, which is legal because the name's scope is
// the target.
func TestRun_CaptureFanOutGeneratesOnePackagePerApp(t *testing.T) {
	cfg := writeFanOutProject(t, "")
	res := mustGenerate(t, cfg)
	if res.QueryCount != 2 {
		t.Fatalf("QueryCount = %d, want 2", res.QueryCount)
	}
	for _, app := range []string{"signin", "signup"} {
		dir := filepath.Join(cfg.Dir, "app", app, "gen")
		for _, f := range []string{"db.gen.go", "querier.gen.go", "search_users.sql.gen.go"} {
			if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
				t.Errorf("%s/%s: %v", app, f, err)
			}
		}
		src, err := os.ReadFile(filepath.Join(dir, "db.gen.go"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), "package gen\n") {
			t.Errorf("%s/db.gen.go does not declare `package gen`", app)
		}
	}
}

// Each target gets its own explain/expanded namespace, so two
// same-named queries cannot overwrite each other's derived output.
func TestRun_ExplainDataIsNamespacedByTarget(t *testing.T) {
	cfg := writeFanOutProject(t, "")
	mustGenerate(t, cfg)
	for _, app := range []string{"signin", "signup"} {
		p := filepath.Join(cfg.Dir, ".sqletch", "explain", "app", app, "gen", "SearchUsers.json")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("explain data for %s: %v", app, err)
		}
	}
}

// Two different packages may define SearchUsers; two files in ONE
// package may not.
func TestRun_DuplicateNameIsScopedToItsTarget(t *testing.T) {
	cfg := writeFanOutProject(t, "")
	p := filepath.Join(cfg.Dir, "app", "signin", "queries", "dup.sql")
	if err := os.WriteFile(p, []byte(nativeCLIQuery), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), cfg, ModeCheck, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasCode(res.Diags, diagnostics.CodeDuplicateQueryName) {
		t.Fatalf("want SQLETCH004 within one target, got %+v", res.Diags)
	}
}

// A generated file this run did not write is stale — a query that was
// deleted or renamed. It is removed; a hand-written neighbour is not.
func TestRun_RemovesStaleGeneratedFiles(t *testing.T) {
	cfg := writeFanOutProject(t, "")
	mustGenerate(t, cfg)

	genDir := filepath.Join(cfg.Dir, "app", "signin", "gen")
	stale := filepath.Join(genDir, "old_query.sql.gen.go")
	handwritten := filepath.Join(genDir, "helpers.go")
	for _, p := range []string{stale, handwritten} {
		if err := os.WriteFile(p, []byte("package gen\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sub := filepath.Join(genDir, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	mustGenerate(t, cfg)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale *.gen.go was not removed (err=%v)", err)
	}
	if _, err := os.Stat(handwritten); err != nil {
		t.Errorf("a hand-written file must survive: %v", err)
	}
	if _, err := os.Stat(sub); err != nil {
		t.Errorf("a subdirectory must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(genDir, "db.gen.go")); err != nil {
		t.Errorf("this run's own output must survive: %v", err)
	}
}

// `check` verifies; it never writes and never deletes.
func TestRun_CheckDoesNotRemoveStaleFiles(t *testing.T) {
	cfg := writeFanOutProject(t, "")
	mustGenerate(t, cfg)
	stale := filepath.Join(cfg.Dir, "app", "signin", "gen", "old_query.sql.gen.go")
	if err := os.WriteFile(stale, []byte("package gen\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), cfg, ModeCheck, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("check must not delete anything: %v", err)
	}
}

// A target whose patterns match nothing warns and generates no
// package — it must not fail the run, and must not leave an empty
// directory behind.
func TestRun_TargetWithNoMatchesWarnsAndEmitsNothing(t *testing.T) {
	cfg := writeFanOutProject(t, `targets:
  - queries: ["(app/*)/queries/*.sql"]
    output:
      package: gen
      path: $1/gen
  - queries: [nowhere/*.sql]
    output:
      package: other
      path: other/gen
`)
	res := mustGenerate(t, cfg)
	if !hasCode(res.Diags, diagnostics.CodeTargetNoMatch) {
		t.Errorf("want a SQLETCH316 warning, got %+v", res.Diags)
	}
	if _, err := os.Stat(filepath.Join(cfg.Dir, "other", "gen")); !os.IsNotExist(err) {
		t.Errorf("an empty target must not create its output directory (err=%v)", err)
	}
}

// overrides/static_expansion key on a query NAME, which is unique only
// within a target since design 19 — an entry that hits several targets
// says so.
func TestRun_NameSpanningTargetsWarns(t *testing.T) {
	cfg := writeFanOutProject(t, `targets:
  - queries: ["(app/*)/queries/*.sql"]
    output:
      package: gen
      path: $1/gen
overrides:
  - {query: SearchUsers, column: nick, nullable: true}
`)
	res := mustGenerate(t, cfg)
	if !hasCode(res.Diags, diagnostics.CodeTargetNameSpan) {
		t.Errorf("want a SQLETCH317 warning, got %+v", res.Diags)
	}
}

// `sqletch explain` reads the namespaced tree and prints every target
// that defines the name, each under its own heading.
func TestExplain_AcrossTargets(t *testing.T) {
	cfg := writeFanOutProject(t, "")
	mustGenerate(t, cfg)

	var out, errW bytes.Buffer
	code := Explain(context.Background(), cfg.Path, []string{"SearchUsers"}, ExplainOptions{}, &out, &errW)
	if code != ExitOK {
		t.Fatalf("explain exited %d: %s", code, errW.String())
	}
	got := out.String()
	for _, want := range []string{"# target app/signin/gen", "# target app/signup/gen"} {
		if !strings.Contains(got, want) {
			t.Errorf("explain output does not carry %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "SearchUsers"); n < 2 {
		t.Errorf("want both targets' SearchUsers, got %d mentions", n)
	}
}

// The LSP resolves the same targets as the pipeline, so a name defined
// once per package is not a duplicate there either.
func TestOfflineChecker_DuplicateNameScopedPerTarget(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{
		"sqletch.yaml": `version: 1
dialect: postgres
server_version: "16"
schema:
  files: [db/schema.sql]
targets:
  - queries: ["(app/*)/queries/*.sql"]
    output:
      package: gen
      path: $1/gen
`,
		"app/a/queries/q.sql": validQuery,
		"app/b/queries/q.sql": validQuery,
	})
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	for p, diags := range res.Diags {
		if hasCode(diags, diagnostics.CodeDuplicateQueryName) {
			t.Errorf("%s: two packages may each define the query: %+v", p, diags)
		}
	}
	if len(res.Files) != 2 {
		t.Errorf("checked %d files, want 2", len(res.Files))
	}
}

// Resolving walks the filesystem, so a keystroke must not pay for it:
// the resolution is memoized behind the stat signature of every
// directory the walk consulted, and a new template file invalidates it.
func TestOfflineChecker_TargetMemoAvoidsReWalks(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{"queries/a.sql": validQuery})
	c := NewOfflineChecker(cfg)
	if _, err := c.Check(nil); err != nil {
		t.Fatal(err)
	}
	if c.targetWalks != 1 {
		t.Fatalf("targetWalks = %d after the first Check, want 1", c.targetWalks)
	}
	if _, err := c.Check(nil); err != nil {
		t.Fatal(err)
	}
	if c.targetWalks != 1 {
		t.Errorf("an unchanged Check re-walked the tree: targetWalks = %d", c.targetWalks)
	}

	// A new template file changes its directory's mtime, which is what
	// the memo watches.
	if err := os.WriteFile(filepath.Join(cfg.Dir, "queries", "b.sql"),
		[]byte(strings.Replace(validQuery, "FindT", "FindT2", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := c.Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.targetWalks != 2 {
		t.Errorf("a new template file did not invalidate the memo: targetWalks = %d", c.targetWalks)
	}
	if len(res.Files) != 2 {
		t.Errorf("checked %d files, want 2", len(res.Files))
	}
}
