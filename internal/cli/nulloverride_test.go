package cli

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// nullableQuery flips both directions against the catalog: email is
// NOT NULL (forced nullable), nick is nullable (forced non-null).
const nullableQuery = `-- name: SearchUsers :many
-- @param status: varchar(32)
-- @param limit: bigint
-- @nullable email: true
-- @nullable nick: false
SELECT u.id, u.email, u.nick
FROM users AS u
WHERE TRUE
@if-present(status)
  AND u.status = :status
@endif
ORDER BY u.id
LIMIT :limit;
`

// writeNullableProject is writeNativeMySQLProject with extra
// sqletch.yaml keys appended (the native backend needs no database).
func writeNullableProject(t *testing.T, query, extraYAML string) config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := writeNativeMySQLProject(t, dir, nativeCLISchema, query)
	if extraYAML == "" {
		return cfg
	}
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Path, append(raw, []byte(extraYAML)...), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, diags := config.Load(cfg.Path)
	if len(diags) > 0 {
		t.Fatalf("config: %+v", diags)
	}
	return cfg
}

// rowFieldType returns the Go type of a field of the generated row
// struct for SearchUsers.
func rowFieldType(t *testing.T, cfg config.Config, field string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(cfg.Dir, "gen", "*.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\t` + field + `\s+(\S+)$`)
	for _, p := range matches {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if m := re.FindSubmatch(src); m != nil {
			return string(m[1])
		}
	}
	t.Fatalf("no field %s in generated code", field)
	return ""
}

// The directive is the in-template spelling of a sqletch.yaml
// `overrides` entry: both directions reach the generated row type.
func TestRun_NullableDirectiveForcesGeneratedType(t *testing.T) {
	cfg := writeNullableProject(t, nullableQuery, "")
	res := mustGenerate(t, cfg)
	if len(res.Diags) != 0 {
		t.Errorf("unexpected diagnostics: %+v", res.Diags)
	}
	if got := rowFieldType(t, cfg, "Email"); got != "optional.Option[string]" {
		t.Errorf("Email = %s, want optional.Option[string] (forced nullable)", got)
	}
	if got := rowFieldType(t, cfg, "Nick"); got != "string" {
		t.Errorf("Nick = %s, want string (forced non-null)", got)
	}
	// Control: without the directives the analyzer's verdicts stand.
	plain := writeNullableProject(t, nativeCLIQuery, "")
	mustGenerate(t, plain)
	if got := rowFieldType(t, plain, "Email"); got != "string" {
		t.Errorf("control Email = %s, want string", got)
	}
	if got := rowFieldType(t, plain, "Nick"); got != "optional.Option[string]" {
		t.Errorf("control Nick = %s, want optional.Option[string]", got)
	}
}

// The directive and the config entry are interchangeable: the same
// declaration from either place generates byte-identical code.
func TestRun_NullableDirectiveMatchesConfigOverride(t *testing.T) {
	viaDirective := writeNullableProject(t, nullableQuery, "")
	mustGenerate(t, viaDirective)
	viaConfig := writeNullableProject(t, nativeCLIQuery, `overrides:
  - {query: SearchUsers, column: email, nullable: true}
  - {query: SearchUsers, column: nick, nullable: false}
`)
	mustGenerate(t, viaConfig)
	for _, field := range []string{"Email", "Nick"} {
		if a, b := rowFieldType(t, viaDirective, field), rowFieldType(t, viaConfig, field); a != b {
			t.Errorf("%s: directive %s != config %s", field, a, b)
		}
	}
}

// One declaration per column: the same (query, column) in sqletch.yaml
// AND a directive is SQLETCH319 at the directive — even when the two
// agree, so a reviewer never has to reconcile two places.
func TestRun_NullableDirectiveConflictsWithConfig(t *testing.T) {
	for _, val := range []string{"true", "false"} {
		cfg := writeNullableProject(t, nullableQuery, `overrides:
  - {query: SearchUsers, column: nick, nullable: `+val+`}
`)
		res, err := Run(context.Background(), cfg, ModeGenerate, RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		d := findCode(res.Diags, diagnostics.CodeNullOverrideConflict)
		if d == nil {
			t.Fatalf("nullable: %s — want SQLETCH319, got %+v", val, res.Diags)
		}
		if d.Severity != diagnostics.Error {
			t.Errorf("SQLETCH319 severity = %v, want error", d.Severity)
		}
		if got := spanText(t, res, d.Span); got != "-- @nullable nick: false" {
			t.Errorf("SQLETCH319 points at %q, want the directive", got)
		}
		if !strings.Contains(d.Message, cfg.Path) && !strings.Contains(d.Hint, cfg.Path) {
			t.Errorf("SQLETCH319 should name the config file: %+v", d)
		}
		if _, err := os.Stat(filepath.Join(cfg.Dir, "gen")); !os.IsNotExist(err) {
			t.Errorf("a conflicting run must not generate (err=%v)", err)
		}
	}
}

// Conflicts are per (query, column): a config entry for another column
// of the query, or the same column of another query, is no conflict.
func TestRun_NullableDirectiveNoConflictAcrossKeys(t *testing.T) {
	cfg := writeNullableProject(t, nullableQuery, `overrides:
  - {query: SearchUsers, column: id, nullable: false}
  - {query: OtherQuery, column: nick, nullable: true}
`)
	res, err := Run(context.Background(), cfg, ModeGenerate, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if hasCode(res.Diags, diagnostics.CodeNullOverrideConflict) || diagnostics.HasErrors(res.Diags) {
		t.Fatalf("unexpected diagnostics: %+v", res.Diags)
	}
	if got := rowFieldType(t, cfg, "Nick"); got != "string" {
		t.Errorf("Nick = %s, want string", got)
	}
}

// A directive naming no result column is dead (SQLETCH312), one naming
// several same-named columns forces them all (SQLETCH313): the config
// hygiene warnings, pointed at the directive instead of the config.
func TestRun_NullableDirectiveHygiene(t *testing.T) {
	const query = `-- name: SearchUsers :many
-- @param limit: bigint
-- @nullable nik: false
-- @nullable email: true
SELECT u.id, u.email, u2.email
FROM users AS u JOIN users AS u2 ON u2.id = u.id
ORDER BY u.id
LIMIT :limit;
`
	cfg := writeNullableProject(t, query, "")
	// Same-named columns also collide as row fields (SQLETCH310), so the
	// run fails anyway; the warnings must still be there to explain it.
	res, err := Run(context.Background(), cfg, ModeGenerate, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dead := findCode(res.Diags, diagnostics.CodeOverrideUnknownColumn)
	if dead == nil {
		t.Fatalf("want SQLETCH312, got %+v", res.Diags)
	}
	if got := spanText(t, res, dead.Span); got != "-- @nullable nik: false" {
		t.Errorf("SQLETCH312 points at %q", got)
	}
	amb := findCode(res.Diags, diagnostics.CodeOverrideAmbiguousColumn)
	if amb == nil {
		t.Fatalf("want SQLETCH313, got %+v", res.Diags)
	}
	if got := spanText(t, res, amb.Span); got != "-- @nullable email: true" {
		t.Errorf("SQLETCH313 points at %q", got)
	}
	for _, d := range []*diagnostics.Diagnostic{dead, amb} {
		if d.Severity != diagnostics.Warning {
			t.Errorf("%s severity = %v, want warning", d.Code, d.Severity)
		}
		if strings.Contains(d.Message, "null_overrides") {
			t.Errorf("%s names a config key that does not exist: %s", d.Code, d.Message)
		}
	}
}

// The editor sees what the CLI sees: a malformed directive (SQLETCH018)
// and a config conflict (SQLETCH319) need no cache; the hygiene
// warnings (catalog-dependent) appear once the cache is warm.
func TestOfflineChecker_NullableDirective(t *testing.T) {
	const query = `-- name: SearchUsers :many
-- @param limit: bigint
-- @nullable nik: false
-- @nullable email: true
SELECT u.id, u.email
FROM users AS u
ORDER BY u.id
LIMIT :limit;
`
	qpath := func(cfg config.Config) string { return cfg.Abs("queries/q.sql") }

	conflict := writeNullableProject(t, query, `overrides:
  - {query: SearchUsers, column: email, nullable: true}
`)
	res, err := NewOfflineChecker(conflict).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasCode(res.Diags[qpath(conflict)], diagnostics.CodeNullOverrideConflict) {
		t.Errorf("LSP: want SQLETCH319 on the query file, got %+v", res.Diags)
	}

	bad := writeNullableProject(t, strings.Replace(query, "email: true", "email: yes", 1), "")
	res, err = NewOfflineChecker(bad).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasCode(res.Diags[qpath(bad)], diagnostics.CodeBadNullable) {
		t.Errorf("LSP: want SQLETCH018, got %+v", res.Diags)
	}

	warm := writeNullableProject(t, query, "")
	mustGenerate(t, warm)
	res, err = NewOfflineChecker(warm).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasCode(res.Diags[qpath(warm)], diagnostics.CodeOverrideUnknownColumn) {
		t.Errorf("LSP: want SQLETCH312 once the cache is warm, got %+v", res.Diags)
	}
}

// A `//sqletch:query` const carries the directive like a .sql file.
func TestOfflineCheck_NullableDirectiveInGoSource(t *testing.T) {
	src := strings.Replace(goRepoSource, "-- name: FindT :many\n", "-- name: FindT :many\n-- @nullable id: true\n", 1)
	cfg := writeOfflineProject(t, goProject(t, src))
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.Abs("repo/users.go")
	if diags := res.Diags[path]; len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	q := res.Files[path].Queries[0]
	if o, ok := q.NullOverrides["id"]; !ok || !o.Nullable {
		t.Errorf("directive not recorded from Go source: %+v", q.NullOverrides)
	}
}

func spanText(t *testing.T, res *Result, s diagnostics.Span) string {
	t.Helper()
	src, ok := res.Sources[s.File]
	if !ok {
		t.Fatalf("no source for %s", s.File)
	}
	return string(src[s.Start:s.End])
}
