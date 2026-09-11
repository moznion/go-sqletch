package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// project writes a sqletch.yaml with the given targets block plus the
// listed template files, and loads it.
func project(t *testing.T, targets string, files ...string) Config {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("-- name: Q :many\nSELECT 1;\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(t, dir, "db/schema.sql", "CREATE TABLE t (id int);\n")
	path := write(t, dir, "sqletch.yaml", `version: 1
dialect: postgres
server_version: "16"
schema:
  files: [db/schema.sql]
`+targets)
	cfg, diags := Load(path)
	if diagnostics.HasErrors(diags) {
		t.Fatalf("config did not load: %+v", diags)
	}
	return cfg
}

func codes(diags []diagnostics.Diagnostic) []string {
	out := make([]string, len(diags))
	for i, d := range diags {
		out[i] = string(d.Code)
	}
	return out
}

func hasCode(diags []diagnostics.Diagnostic, code diagnostics.Code) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

// shape renders a resolution as "path:package[file …]" lines, so an
// assertion reads as the packages the run will emit.
func shape(t *testing.T, cfg Config, r Resolution) []string {
	t.Helper()
	var out []string
	for _, tg := range r.Targets {
		rels := make([]string, len(tg.Files))
		for i, f := range tg.Files {
			rel, err := filepath.Rel(cfg.Dir, f)
			if err != nil {
				t.Fatal(err)
			}
			rels[i] = filepath.ToSlash(rel)
		}
		out = append(out, tg.Path+":"+tg.Package+"["+strings.Join(rels, " ")+"]")
	}
	return out
}

func TestResolveTargets_SingleTarget(t *testing.T) {
	cfg := project(t, `targets:
  - queries: [queries/*.sql]
    output: {package: gen, path: gen}
`, "queries/a.sql", "queries/b.sql", "queries/c.txt")
	res, diags := cfg.ResolveTargets()
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	want := []string{"gen:gen[queries/a.sql queries/b.sql]"}
	if got := shape(t, cfg, res); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// The capture fans one entry out into one package per matched
// directory, and the targets come back ordered by output path.
func TestResolveTargets_CaptureFanOut(t *testing.T) {
	cfg := project(t, `targets:
  - queries: ["(app/**/queries)/*.sql"]
    output: {package: gen, path: $1/gen}
`, "app/signup/queries/a.sql", "app/signin/queries/b.sql", "app/signin/queries/c.sql")
	res, diags := cfg.ResolveTargets()
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	want := []string{
		"app/signin/queries/gen:gen[app/signin/queries/b.sql app/signin/queries/c.sql]",
		"app/signup/queries/gen:gen[app/signup/queries/a.sql]",
	}
	if got := shape(t, cfg, res); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Two patterns of one entry that expand to the same output merge:
// the grouping key is the substituted output, not the config entry.
func TestResolveTargets_PatternsMergeByOutput(t *testing.T) {
	cfg := project(t, `targets:
  - queries: ["(app/*)/queries/*.sql", "(app/*)/shared/*.sql"]
    output: {package: gen, path: $1/gen}
`, "app/x/queries/a.sql", "app/x/shared/b.sql")
	res, diags := cfg.ResolveTargets()
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	want := []string{"app/x/gen:gen[app/x/queries/a.sql app/x/shared/b.sql]"}
	if got := shape(t, cfg, res); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// The same file reached twice by one entry's patterns, landing on the
// same output, is a duplicate — not an overlap.
func TestResolveTargets_SameFileSameOutputIsNotAnOverlap(t *testing.T) {
	cfg := project(t, `targets:
  - queries: [queries/*.sql, "queries/**/*.sql"]
    output: {package: gen, path: gen}
`, "queries/a.sql")
	res, diags := cfg.ResolveTargets()
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	want := []string{"gen:gen[queries/a.sql]"}
	if got := shape(t, cfg, res); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestResolveTargets_FileClaimedByTwoTargets(t *testing.T) {
	cfg := project(t, `targets:
  - queries: [queries/*.sql]
    output: {package: gen, path: a}
  - queries: [queries/a.sql]
    output: {package: gen, path: b}
`, "queries/a.sql")
	_, diags := cfg.ResolveTargets()
	if !hasCode(diags, diagnostics.CodeTargetFileOverlap) {
		t.Fatalf("want SQLETCH314, got %v", codes(diags))
	}
}

func TestResolveTargets_SamePathDifferentPackage(t *testing.T) {
	cfg := project(t, `targets:
  - queries: [a/*.sql]
    output: {package: one, path: gen}
  - queries: [b/*.sql]
    output: {package: two, path: gen}
`, "a/x.sql", "b/y.sql")
	_, diags := cfg.ResolveTargets()
	if !hasCode(diags, diagnostics.CodeTargetCollision) {
		t.Fatalf("want SQLETCH315, got %v", codes(diags))
	}
}

// Two entries that agree on package AND path are one package.
func TestResolveTargets_SamePathSamePackageMerges(t *testing.T) {
	cfg := project(t, `targets:
  - queries: [a/*.sql]
    output: {package: gen, path: gen}
  - queries: [b/*.sql]
    output: {package: gen, path: gen}
`, "a/x.sql", "b/y.sql")
	res, diags := cfg.ResolveTargets()
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	want := []string{"gen:gen[a/x.sql b/y.sql]"}
	if got := shape(t, cfg, res); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A pattern matching nothing WARNS (a monorepo app may not exist yet)
// and contributes no package — it is not an error, and it does not
// stop the other targets.
func TestResolveTargets_NoMatchWarns(t *testing.T) {
	cfg := project(t, `targets:
  - queries: [queries/*.sql]
    output: {package: gen, path: gen}
  - queries: [nothere/*.sql]
    output: {package: gen, path: other}
`, "queries/a.sql")
	res, diags := cfg.ResolveTargets()
	if diagnostics.HasErrors(diags) {
		t.Fatalf("a zero-match pattern must not be an error: %+v", diags)
	}
	if !hasCode(diags, diagnostics.CodeTargetNoMatch) {
		t.Fatalf("want SQLETCH316, got %v", codes(diags))
	}
	want := []string{"gen:gen[queries/a.sql]"}
	if got := shape(t, cfg, res); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A `$n` the output substitutes must exist in every pattern of the
// entry, and this is caught at LOAD — no filesystem access needed.
func TestLoad_CaptureArity(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "db/schema.sql", "CREATE TABLE t (id int);\n")
	path := write(t, dir, "sqletch.yaml", `version: 1
dialect: postgres
server_version: "16"
schema:
  files: [db/schema.sql]
targets:
  - queries: ["(a/*)/q/*.sql", "b/q/*.sql"]
    output: {package: gen, path: $1/gen}
`)
	_, diags := Load(path)
	if !diagnostics.HasErrors(diags) {
		t.Fatal("want an error: the second pattern supplies no $1")
	}
	if !strings.Contains(diags[0].Message, "capture group") {
		t.Errorf("message = %q, want it to name the missing capture group", diags[0].Message)
	}
}

func TestLoad_MalformedCaptureReference(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "db/schema.sql", "CREATE TABLE t (id int);\n")
	path := write(t, dir, "sqletch.yaml", `version: 1
dialect: postgres
server_version: "16"
schema:
  files: [db/schema.sql]
targets:
  - queries: ["(a/*)/q/*.sql"]
    output: {package: gen, path: $x/gen}
`)
	_, diags := Load(path)
	if !diagnostics.HasErrors(diags) {
		t.Fatal("want an error for a malformed capture reference")
	}
}

// The output package is emitted verbatim as `package <name>`, so it
// must be a plain Go identifier — both in the literal spelling and
// after substitution.
func TestTargets_PackageMustBeAnIdentifier(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "db/schema.sql", "CREATE TABLE t (id int);\n")
	path := write(t, dir, "sqletch.yaml", `version: 1
dialect: postgres
server_version: "16"
schema:
  files: [db/schema.sql]
targets:
  - queries: [queries/*.sql]
    output: {package: "gen\nfunc x() {}", path: gen}
`)
	if _, diags := Load(path); !diagnostics.HasErrors(diags) {
		t.Fatal("want an error for a non-identifier package name")
	}

	cfg := project(t, `targets:
  - queries: ["(app/*)/queries/*.sql"]
    output: {package: $1, path: $1/gen}
`, "app/not-an-ident/queries/a.sql")
	if _, diags := cfg.ResolveTargets(); !diagnostics.HasErrors(diags) {
		t.Fatal("want an error for a substituted non-identifier package name")
	}
}

// A pattern may not climb out of the project, and a match reached
// through an escaping symlink is refused (SQLETCH306) — the
// clone-and-run file-disclosure vector.
func TestTargets_PathEscape(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "db/schema.sql", "CREATE TABLE t (id int);\n")
	path := write(t, dir, "sqletch.yaml", `version: 1
dialect: postgres
server_version: "16"
schema:
  files: [db/schema.sql]
targets:
  - queries: ["../*.sql"]
    output: {package: gen, path: gen}
`)
	if _, diags := Load(path); !diagnostics.HasErrors(diags) {
		t.Fatal("want an error for a climbing pattern")
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.sql"), []byte("-- name: S :many\nSELECT 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := project(t, `targets:
  - queries: ["link/*.sql"]
    output: {package: gen, path: gen}
`)
	if err := os.Symlink(outside, filepath.Join(cfg.Dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, diags := cfg.ResolveTargets()
	if !hasCode(diags, diagnostics.CodePathEscape) {
		t.Fatalf("want SQLETCH306 for a symlinked escape, got %v", codes(diags))
	}
}

// An output path that escapes through a capture is refused with the
// same rule as a literal one: the substituted form is what is written.
func TestTargets_SubstitutedOutputPathIsChecked(t *testing.T) {
	cfg := project(t, `targets:
  - queries: ["(app/*)/queries/*.sql"]
    output: {package: gen, path: $1/../../../outside}
`, "app/x/queries/a.sql")
	_, diags := cfg.ResolveTargets()
	if !hasCode(diags, diagnostics.CodePathEscape) {
		t.Fatalf("want SQLETCH306, got %v", codes(diags))
	}
}

func TestLoad_LegacyKeysGetAMigrationDiagnostic(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "db/schema.sql", "CREATE TABLE t (id int);\n")
	path := write(t, dir, "sqletch.yaml", `version: 1
dialect: postgres
server_version: "16"
schema:
  files: [db/schema.sql]
queries: [queries/*.sql]
output:
  package: gen
  path: gen
`)
	_, diags := Load(path)
	if !diagnostics.HasErrors(diags) {
		t.Fatal("the pre-targets spelling must be refused")
	}
	d := diags[0]
	if d.Code != diagnostics.CodeConfigInvalid || !strings.Contains(d.Message, "targets") {
		t.Fatalf("want a SQLETCH301 naming `targets`, got %+v", d)
	}
	// The hint must carry the rewrite, with the user's own values.
	for _, want := range []string{"queries/*.sql", "package: gen", "path: gen"} {
		if !strings.Contains(d.Hint, want) {
			t.Errorf("hint %q does not carry %q", d.Hint, want)
		}
	}
}

func TestResolvedTarget_Slug(t *testing.T) {
	cases := []struct{ path, want string }{
		{"gen", "gen"},
		{"internal/db/gen", "internal/db/gen"},
		{"/abs/out", "abs/out"},
		{"../out", "_/out"},
		{"", "_"},
	}
	for _, c := range cases {
		if got := (ResolvedTarget{Path: c.path}).Slug(); got != c.want {
			t.Errorf("Slug(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestResolveTargets_RecordsDirsForMemoization(t *testing.T) {
	cfg := project(t, `targets:
  - queries: ["app/**/queries/*.sql"]
    output: {package: gen, path: gen}
`, "app/x/queries/a.sql")
	res, _ := cfg.ResolveTargets()
	if len(res.Dirs) == 0 {
		t.Fatal("Dirs is empty: the LSP memo would never invalidate")
	}
	for _, d := range res.Dirs {
		if !filepath.IsAbs(d) && !strings.HasPrefix(d, cfg.Dir) {
			t.Errorf("Dirs entry %q is not resolvable from the process cwd", d)
		}
	}
}
