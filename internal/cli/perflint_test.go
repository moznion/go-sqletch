package cli

import (
	"bufio"
	"bytes"
	"io"
	"path/filepath"
	"slices"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
)

const perfLintQueries = `-- name: Wrapped :many
SELECT t.id FROM t WHERE lower(t.x) = :x;

-- name: Allowed :many
-- @allow SQLETCH128, SQLETCH130 (expression index t_lower_x_idx; tiny table)
SELECT t.id FROM t WHERE lower(t.x) = :x;

-- name: Stale :one
-- @allow SQLETCH129
SELECT t.id FROM t WHERE t.id = :id;
`

// The catalog-free lints run in the shared scan seam, so the offline
// checker (the LSP's analysis) reports them — as warnings, with @allow
// honored per query and a stale @allow reported at its directive.
func TestOffline_PerfLints(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{"queries/p.sql": perfLintQueries})
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
		"SQLETCH130@-- name: Wrapped :many",
		"SQLETCH128@lower(t.x)",
		"SQLETCH133@-- @allow SQLETCH129",
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

// A bad @allow is an error (SQLETCH016) — it would otherwise be a
// suppression that silently suppresses nothing.
func TestOffline_BadAllowIsAnError(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{"queries/p.sql": "-- name: Q :one\n-- @allow SQLETCH115\nSELECT t.id FROM t;\n"})
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	d := findCode(res.Diags[cfg.Abs("queries/p.sql")], diagnostics.CodeBadAllow)
	if d == nil || d.Severity != diagnostics.Error {
		t.Fatalf("got %+v", res.Diags)
	}
}

// SQLETCH132 runs in the shared catalog-dependent pass, against the
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
		"-- name: Q :one\n-- @allow SQLETCH132\nSELECT id FROM users WHERE id = :p::numeric;\n", numeric)
	if len(diags) != 0 {
		t.Errorf("suppressed: %+v", diags)
	}
	_, diags = runResolvedChecks(t, "postgres",
		"-- name: Q :one\nSELECT id FROM users WHERE id = :p;\n", []dialect.TypeRef{{OID: 20, Name: "int8"}})
	if len(diags) != 0 {
		t.Errorf("index-safe pair flagged: %+v", diags)
	}
}

// The LSP publishes performance lints as LSP warnings (severity 2).
func TestLSP_PerfLintIsAWarning(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{"queries/a.sql": validQuery})
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
