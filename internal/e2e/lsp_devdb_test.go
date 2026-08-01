//go:build devdb

package e2e_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/moznion/sqletch/internal/cli"
	"github.com/moznion/sqletch/internal/config"
	"github.com/moznion/sqletch/internal/devdb"
	"github.com/moznion/sqletch/internal/diagnostics"
)

const lspSchema = `
CREATE TABLE t (id bigint NOT NULL, x text);
CREATE TABLE u (id bigint NOT NULL, name text, score bigint);
`

// lspQuery projects u.name, which only exists under the optional join —
// an R3 scope violation (SQLETCH115). Catching it needs the catalog, so
// it is exactly the class of diagnostic the LSP can only report from a
// warm cache. The @choose gives the query a second verified rendering,
// so the all-or-nothing cache rule below has an entry to withhold.
const lspQuery = `-- name: FindUser :many
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

func codesOf(diags []diagnostics.Diagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, string(d.Code))
	}
	sort.Strings(out)
	return out
}

func workspaceCodes(res cli.WorkspaceCheck) []string {
	var out []string
	for _, ds := range res.Diags {
		out = append(out, codesOf(ds)...)
	}
	sort.Strings(out)
	return out
}

// TestLSPWarmCacheAgreesWithPipeline closes the loop between the two
// consumers of resolvedChecks. The unit tests drive the OfflineChecker
// with a hand-built catalog and hand-built Descs; here the cache is the
// one the real pipeline wrote from a real server, and the LSP's answer
// must equal the CLI's on the same workspace. Divergence means the
// shared catalog-dependent pass has been forked.
func TestLSPWarmCacheAgreesWithPipeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn, cleanup, err := devdb.AcquireDSN(ctx, devdb.Config{
		DSN:           os.Getenv("SQLETCH_TEST_DSN"),
		ServerVersion: "16",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	dir := t.TempDir()
	writeFile(t, dir, "db/schema.sql", lspSchema)
	writeFile(t, dir, "queries/q.sql", lspQuery)
	writeConfig := func(dsn string) config.Config {
		t.Helper()
		writeFile(t, dir, "sqletch.yaml", `version: 1
dialect: postgres
server_version: "16"
database:
  dsn: `+dsn+`
schema:
  files: [db/schema.sql]
queries: [queries/*.sql]
output:
  package: gen
  path: gen
cache:
  path: .sqletch/cache
`)
		cfg, diags := config.Load(filepath.Join(dir, "sqletch.yaml"))
		if diagnostics.HasErrors(diags) {
			t.Fatalf("config: %v", diags)
		}
		return cfg
	}

	// 1. Cold run against the real server: fills the cache and produces
	//    the reference diagnostics.
	res, err := cli.Run(ctx, writeConfig(dsn), cli.ModeCheck)
	if err != nil {
		t.Fatalf("cold run: %v", err)
	}
	want := codesOf(res.Diags)
	if !contains(want, string(diagnostics.CodeScopeViolation)) {
		t.Fatalf("fixture must produce %s; got %v", diagnostics.CodeScopeViolation, want)
	}

	// 2. The LSP over the committed cache, with a DSN it could never
	//    reach — proving no connection is opened — must agree exactly.
	offlineCfg := writeConfig("postgres://unreachable.invalid:1/nope")
	got, err := cli.NewOfflineChecker(offlineCfg).Check(nil)
	if err != nil {
		t.Fatalf("offline check: %v", err)
	}
	if g, w := strings.Join(workspaceCodes(got), ","), strings.Join(want, ","); g != w {
		t.Errorf("LSP and CLI disagree on the same workspace:\n LSP: %s\n CLI: %s", g, w)
	}

	// 3. All-or-nothing: with one rendering evicted, the query's
	//    catalog-dependent pass must be skipped wholesale rather than
	//    producing half-true answers from partial oracle data.
	entries, err := filepath.Glob(filepath.Join(dir, ".sqletch/cache/oracle/*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("fixture must cache several renderings, got %d", len(entries))
	}
	if err := os.Remove(entries[0]); err != nil {
		t.Fatal(err)
	}
	partial, err := cli.NewOfflineChecker(offlineCfg).Check(nil)
	if err != nil {
		t.Fatalf("partial check: %v", err)
	}
	if c := workspaceCodes(partial); contains(c, string(diagnostics.CodeScopeViolation)) {
		t.Errorf("a partial cache must not run the resolution pass, got %v", c)
	}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
