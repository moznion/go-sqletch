package cli

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
)

const perfLintQueries = `-- name: Wrapped :many
SELECT t.id FROM t WHERE lower(t.x) = :x;

-- name: Allowed :many
-- @nolint SQLETCHL001, SQLETCHL003 (expression index t_lower_x_idx; tiny table)
SELECT t.id FROM t WHERE lower(t.x) = :x;

-- name: Stale :one
-- @nolint SQLETCHL002
SELECT t.id FROM t WHERE t.id = :id;
`

// The catalog-free lints run in the shared scan seam, so the offline
// checker (the LSP's analysis) reports them — as warnings, with @nolint
// honored per query and a stale @nolint reported at its directive.
func TestOffline_PerfLints(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{"queries/p.sql": perfLintQueries})
	cfg.Lint = true
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.Abs("queries/p.sql")
	diags := res.Diags[path]
	if diagnostics.HasErrors(diags) {
		t.Fatalf("performance lints must never be errors: %+v", diags)
	}
	var got []string
	for _, d := range diags {
		got = append(got, string(d.Code)+"@"+perfLintQueries[d.Span.Start:d.Span.End])
	}
	want := []string{
		"SQLETCHL003@-- name: Wrapped :many",
		"SQLETCHL001@lower(t.x)",
		"SQLETCHL006@-- @nolint SQLETCHL002",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}

	// The same warnings serialize through --format json unchanged.
	var buf bytes.Buffer
	PrintDiags(&buf, &Result{Diags: diags, Sources: map[string][]byte{path: []byte(perfLintQueries)}}, true)
	for _, m := range decodeJSONLines(t, buf.String()) {
		if m["severity"] != "warning" || m["hint"] == "" {
			t.Errorf("json diagnostic = %v", m)
		}
	}
}

// Lints are OPT-IN (owner decision 2026-10-03): with `lint` unset the
// offline checker reports none of them — not even a stale @nolint
// (SQLETCHL006), which is a lint verdict too.
func TestOffline_LintsOffByDefault(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{"queries/p.sql": perfLintQueries})
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := res.Diags[cfg.Abs("queries/p.sql")]; len(d) != 0 {
		t.Errorf("lint off: got %+v", d)
	}
}

// `lint: true` in sqletch.yaml turns them on through the real config
// path (what the LSP loads).
func TestOffline_LintFromConfigFile(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{
		"queries/p.sql": "-- name: Q :many\nSELECT t.id FROM t;\n",
		"sqletch.yaml":  offlineYAML + "lint: true\n",
	})
	if !cfg.Lint {
		t.Fatal("lint: true did not load")
	}
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	if findCode(res.Diags[cfg.Abs("queries/p.sql")], diagnostics.CodePerfManyNoLimit) == nil {
		t.Errorf("got %+v", res.Diags)
	}
}

// A malformed @nolint is SQLETCH016 whether or not lints run: a
// template's validity must not depend on config.
func TestOffline_BadNoLintIsAnErrorWithLintOff(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{"queries/p.sql": "-- name: Q :one\n-- @nolint SQLETCH115\nSELECT t.id FROM t;\n"})
	if cfg.Lint {
		t.Fatal("precondition: lint off")
	}
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := findCode(res.Diags[cfg.Abs("queries/p.sql")], diagnostics.CodeBadNoLint); d == nil || d.Severity != diagnostics.Error {
		t.Fatalf("got %+v", res.Diags)
	}
}

// The pre-SQLETCHLnnn spelling of a lint code is not a lint code.
func TestOffline_OldPerfCodeSpellingRejected(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{"queries/p.sql": "-- name: Q :many\n-- @nolint SQLETCH130\nSELECT t.id FROM t;\n"})
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	if findCode(res.Diags[cfg.Abs("queries/p.sql")], diagnostics.CodeBadNoLint) == nil {
		t.Fatalf("got %+v", res.Diags)
	}
}

// CLI --lint / --lint=false override sqletch.yaml for one invocation;
// absent, the config decides. Exercised through Run on in-process
// SQLite: its ListUsers :many has no LIMIT (SQLETCHL003).
func TestRun_LintSwitch(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name   string
		config bool
		flag   *bool
		want   bool
	}{
		{"default", false, nil, false},
		{"config on", true, nil, true},
		{"flag on", false, &on, true},
		{"flag off beats config", true, &off, false},
		{"flag on agrees", true, &on, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := writeSQLiteProject(t, dir, "3", "dev.sqlite3")
			if c.config {
				appendFile(t, cfgPath, "lint: true\n")
			}
			cfg, diags := config.Load(cfgPath)
			if diagnostics.HasErrors(diags) {
				t.Fatalf("config: %v", diags)
			}
			res, err := Run(context.Background(), cfg, ModeCheck, RunOptions{AllowDestructive: true, Lint: c.flag})
			if err != nil {
				t.Fatal(err)
			}
			if diagnostics.HasErrors(res.Diags) {
				t.Fatalf("lints must never be errors: %v", res.Diags)
			}
			if got := findCode(res.Diags, diagnostics.CodePerfManyNoLimit) != nil; got != c.want {
				t.Errorf("SQLETCHL003 reported = %v, want %v (%v)", got, c.want, res.Diags)
			}
		})
	}
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// A bad @nolint is an error (SQLETCH016) — it would otherwise be a
// suppression that silently suppresses nothing.
func TestOffline_BadNoLintIsAnError(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{"queries/p.sql": "-- name: Q :one\n-- @nolint SQLETCH115\nSELECT t.id FROM t;\n"})
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	d := findCode(res.Diags[cfg.Abs("queries/p.sql")], diagnostics.CodeBadNoLint)
	if d == nil || d.Severity != diagnostics.Error {
		t.Fatalf("got %+v", res.Diags)
	}
}

// SQLETCHL005 runs in the shared catalog-dependent pass, against the
// oracle's parameter types.
func TestResolvedChecks_PerfTypeMismatch(t *testing.T) {
	numeric := []dialect.TypeRef{{OID: 1700, Name: "numeric"}}
	_, diags := runResolvedChecks(t, "postgres",
		"-- name: Q :one\nSELECT id FROM users WHERE id = :p::numeric;\n", numeric)
	d := findCode(diags, diagnostics.CodePerfTypeMismatch)
	if d == nil || d.Severity != diagnostics.Warning {
		t.Fatalf("got %+v", diags)
	}
	_, diags = runResolvedChecks(t, "postgres",
		"-- name: Q :one\n-- @nolint SQLETCHL005\nSELECT id FROM users WHERE id = :p::numeric;\n", numeric)
	if len(diags) != 0 {
		t.Errorf("suppressed: %+v", diags)
	}
	_, diags = runResolvedChecks(t, "postgres",
		"-- name: Q :one\nSELECT id FROM users WHERE id = :p;\n", []dialect.TypeRef{{OID: 20, Name: "int8"}})
	if len(diags) != 0 {
		t.Errorf("index-safe pair flagged: %+v", diags)
	}
	// Lint off: the resolved pass reports nothing, a stale
	// `@nolint SQLETCHL005` included.
	_, diags = runResolvedChecksLint(t, false, "postgres",
		"-- name: Q :one\n-- @nolint SQLETCHL005\nSELECT id FROM users WHERE id = :p::numeric;\n", numeric)
	if len(diags) != 0 {
		t.Errorf("lint off: %+v", diags)
	}
	_, diags = runResolvedChecksLint(t, false, "postgres",
		"-- name: Q :one\nSELECT id FROM users WHERE id = :p::numeric;\n", numeric)
	if len(diags) != 0 {
		t.Errorf("lint off: %+v", diags)
	}
}

// The LSP publishes performance lints as LSP warnings (severity 2).
func TestLSP_PerfLintIsAWarning(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{"queries/a.sql": validQuery, "sqletch.yaml": offlineYAML + "lint: true\n"})
	configPath := filepath.Join(cfg.Dir, "sqletch.yaml")
	uri := "file://" + filepath.Join(cfg.Dir, "queries", "a.sql")

	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- LSP(configPath, c2sR, s2cW, io.Discard) }()
	t.Cleanup(func() {
		_ = c2sW.Close()
		_ = s2cR.Close()
	})
	client := &lspClient{t: t, w: c2sW, r: bufio.NewReader(s2cR)}
	client.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	client.recv()
	client.send(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})
	client.send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didOpen", "params": map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": "sql", "version": 1,
			"text": "-- name: Q :many\nSELECT t.id FROM t;\n"},
	}})
	diags := client.recv()["params"].(map[string]any)["diagnostics"].([]any)
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %v", diags)
	}
	d := diags[0].(map[string]any)
	if d["code"] != string(diagnostics.CodePerfManyNoLimit) || d["severity"] != 2.0 {
		t.Errorf("got %v", d)
	}
	client.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "shutdown"})
	client.recv()
	client.send(map[string]any{"jsonrpc": "2.0", "method": "exit"})
	if code := <-done; code != 0 {
		t.Errorf("exit code = %d", code)
	}
}
