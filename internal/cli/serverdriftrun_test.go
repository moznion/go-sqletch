package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// The SQLite dev database is fully in-process, so the whole drift path
// — connect, record, compare, refuse — runs in a plain `go test`.

func loadDriftConfig(t *testing.T, cfgPath string) config.Config {
	t.Helper()
	cfg, diags := config.Load(cfgPath)
	if diagnostics.HasErrors(diags) {
		t.Fatalf("config: %v", diags)
	}
	return cfg
}

func envSidecarPath(t *testing.T, dir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".sqletch", "cache", "env-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("want exactly one env sidecar, got %v", matches)
	}
	return matches[0]
}

func readEnvSidecar(t *testing.T, path string) cache.Env {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var e cache.Env
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func rewriteEnvVersion(t *testing.T, path, version string) {
	t.Helper()
	e := readEnvSidecar(t, path)
	e.ServerVersion, e.ServerVersionRaw = version, version
	data, err := cache.EncodeEnv(&e)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A run that contacts a server records what it connected to, so a
// later run has something to compare against.
func TestRun_RecordsGenerationEnvironment(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSQLiteProject(t, dir, "3", "dev.sqlite3")

	res, err := Run(context.Background(), loadDriftConfig(t, cfgPath), ModeCheck, RunOptions{AllowDestructive: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("unexpected diagnostics: %v", res.Diags)
	}

	env := readEnvSidecar(t, envSidecarPath(t, dir))
	if env.Dialect != "sqlite" || env.OracleBackend != config.OracleServer {
		t.Errorf("unexpected record: %+v", env)
	}
	if !strings.HasPrefix(env.ServerVersion, "3.") {
		t.Errorf("recorded version %q does not look like the engine's", env.ServerVersion)
	}
	if env.ServerVersionRaw == "" {
		t.Error("the raw reported string must be recorded too")
	}
}

// A warm offline run must stay offline: it never connects, so it has
// nothing to compare and must not go looking.
func TestRun_WarmRunDoesNotTouchTheRecord(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSQLiteProject(t, dir, "3", "dev.sqlite3")
	if _, err := Run(context.Background(), loadDriftConfig(t, cfgPath), ModeCheck, RunOptions{AllowDestructive: true}); err != nil {
		t.Fatal(err)
	}
	sidecar := envSidecarPath(t, dir)
	// A version no engine will ever report: a warm run must not notice.
	rewriteEnvVersion(t, sidecar, "1.2.3")
	before := readEnvSidecar(t, sidecar)

	res, err := Run(context.Background(), loadDriftConfig(t, cfgPath), ModeCheck, RunOptions{AllowDestructive: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Offline {
		t.Fatal("the second run must be offline; the rest of this test assumes it")
	}
	if len(res.Diags) != 0 {
		t.Errorf("a warm offline run must not report drift: %v", res.Diags)
	}
	if got := readEnvSidecar(t, sidecar); got != before {
		t.Errorf("a warm offline run must not rewrite the record: %+v", got)
	}
}

// The motivating case: the committed cache came from somewhere else.
func TestRun_ServerDriftFailsAndLeavesTheCacheAlone(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSQLiteProject(t, dir, "3", "dev.sqlite3")
	if _, err := Run(context.Background(), loadDriftConfig(t, cfgPath), ModeCheck, RunOptions{AllowDestructive: true}); err != nil {
		t.Fatal(err)
	}
	sidecar := envSidecarPath(t, dir)
	rewriteEnvVersion(t, sidecar, "3.0.0")
	before := readEnvSidecar(t, sidecar)

	// --exhaustive always connects, so it is the lane that can see drift
	// even when every entry is a cache hit.
	res, err := Run(context.Background(), loadDriftConfig(t, cfgPath), ModeCheckExhaustive, RunOptions{AllowDestructive: true})
	if err != nil {
		t.Fatalf("drift is a diagnostic, not an environment error: %v", err)
	}
	d := findCode(res.Diags, diagnostics.CodeCacheServerDrift)
	if d == nil {
		t.Fatalf("want %s, got %v", diagnostics.CodeCacheServerDrift, res.Diags)
	}
	if d.Severity != diagnostics.Error {
		t.Error("drift must fail the run by default")
	}
	if !strings.Contains(d.Message, "3.0.0") {
		t.Errorf("message must name the recorded version: %q", d.Message)
	}
	if d.Span.File != cfgPath {
		t.Errorf("span file = %q, want the config file", d.Span.File)
	}
	if got := readEnvSidecar(t, sidecar); got != before {
		t.Errorf("a refused run must leave the committed record untouched: %+v", got)
	}
}

func TestRun_ServerDriftAcceptedByFlag(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSQLiteProject(t, dir, "3", "dev.sqlite3")
	if _, err := Run(context.Background(), loadDriftConfig(t, cfgPath), ModeCheck, RunOptions{AllowDestructive: true}); err != nil {
		t.Fatal(err)
	}
	sidecar := envSidecarPath(t, dir)
	real := readEnvSidecar(t, sidecar).ServerVersion
	rewriteEnvVersion(t, sidecar, "3.0.0")

	res, err := Run(context.Background(), loadDriftConfig(t, cfgPath), ModeCheckExhaustive,
		RunOptions{AllowServerDrift: true, AllowDestructive: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if diagnostics.HasErrors(res.Diags) {
		t.Fatalf("the flag must let the run succeed: %v", res.Diags)
	}
	d := findCode(res.Diags, diagnostics.CodeCacheServerDrift)
	if d == nil || d.Severity != diagnostics.Warning {
		t.Fatalf("the flag must downgrade, never silence: %v", res.Diags)
	}
	if got := readEnvSidecar(t, sidecar).ServerVersion; got != real {
		t.Errorf("accepting drift must adopt the connected server: recorded %q, want %q", got, real)
	}
}

// Caches committed before the sidecar existed must keep working: no
// record means adopt, not fail.
func TestRun_MissingRecordIsAdoptedSilently(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSQLiteProject(t, dir, "3", "dev.sqlite3")
	if _, err := Run(context.Background(), loadDriftConfig(t, cfgPath), ModeCheck, RunOptions{AllowDestructive: true}); err != nil {
		t.Fatal(err)
	}
	sidecar := envSidecarPath(t, dir)
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}

	res, err := Run(context.Background(), loadDriftConfig(t, cfgPath), ModeCheckExhaustive, RunOptions{AllowDestructive: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Diags) != 0 {
		t.Errorf("an absent record must be adopted silently: %v", res.Diags)
	}
	if !strings.HasPrefix(readEnvSidecar(t, envSidecarPath(t, dir)).ServerVersion, "3.") {
		t.Error("the record must be rewritten from the connected server")
	}
}
