package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// benchWorkspace lays down a self-contained project using the native
// MySQL oracle, which needs no database — so the benchmark measures the
// compiler itself rather than a server round trip.
func benchWorkspace(tb testing.TB) config.Config {
	tb.Helper()
	src := filepath.Join("..", "..", "examples", "mysql")
	dir := tb.TempDir()
	for _, rel := range []string{"db/schema.sql", "queries/users.sql"} {
		data, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			tb.Fatal(err)
		}
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "sqletch.yaml"), []byte(`version: 1
dialect: mysql
server_version: "8.4"
database:
  oracle: native
schema:
  files: [db/schema.sql]
queries: [queries/*.sql]
output:
  package: gen
  path: gen
`), 0o644); err != nil {
		tb.Fatal(err)
	}
	cfg, diags := config.Load(filepath.Join(dir, "sqletch.yaml"))
	if len(diags) > 0 {
		tb.Fatalf("config: %+v", diags)
	}
	return cfg
}

// BenchmarkRunGenerate is the whole compile pipeline a user waits on:
// scan, weave, render, verify against the oracle, and emit the module.
func BenchmarkRunGenerate(b *testing.B) {
	cfg := benchWorkspace(b)
	b.ReportAllocs()
	for b.Loop() {
		res, err := Run(context.Background(), cfg, ModeGenerate)
		if err != nil {
			b.Fatal(err)
		}
		if diagnostics.HasErrors(res.Diags) {
			b.Fatalf("diagnostics: %+v", res.Diags)
		}
	}
}

// BenchmarkRunCheck is the same pipeline without emission — what `check`
// and CI runs cost.
func BenchmarkRunCheck(b *testing.B) {
	cfg := benchWorkspace(b)
	b.ReportAllocs()
	for b.Loop() {
		res, err := Run(context.Background(), cfg, ModeCheck)
		if err != nil {
			b.Fatal(err)
		}
		if diagnostics.HasErrors(res.Diags) {
			b.Fatalf("diagnostics: %+v", res.Diags)
		}
	}
}

// BenchmarkOfflineCheck is the LSP's analysis seam: it runs per edit, so
// its cost is editor latency. The overlay stands in for an unsaved
// buffer, which is the case that cannot be served from a memo.
func BenchmarkOfflineCheck(b *testing.B) {
	cfg := benchWorkspace(b)
	src, err := os.ReadFile(filepath.Join(filepath.Dir(cfg.Path), "queries", "users.sql"))
	if err != nil {
		b.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(cfg.Path), "queries", "users.sql")

	c := NewOfflineChecker(cfg)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		// The buffer must differ every iteration, or the content-hash
		// memo serves it and the benchmark measures the memo instead of
		// the analysis. A trailing comment varies the content without
		// changing what the file means.
		edited := append(append([]byte(nil), src...), []byte("\n-- edit "+strconv.Itoa(i)+"\n")...)
		if _, err := c.Check(map[string][]byte{path: edited}); err != nil {
			b.Fatal(err)
		}
	}
}
