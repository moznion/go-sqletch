package cli

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

func driftConfig() config.Config {
	cfg := config.Config{Dialect: "postgres", Path: "/proj/sqletch.yaml"}
	cfg.Cache.Path = ".sqletch/cache"
	return cfg
}

func recordedAt(version, raw string) *cache.Env {
	return &cache.Env{
		SchemaFP: strings.Repeat("ab", 32), Dialect: "postgres",
		OracleBackend: "server", ServerVersion: version, ServerVersionRaw: raw,
	}
}

func TestServerDriftDiag_Silent(t *testing.T) {
	cases := []struct {
		name      string
		recorded  *cache.Env
		actualRaw string
	}{
		{
			// A cache committed before the sidecar existed, or the very
			// first generate: adopt whatever we connected to.
			name: "no record yet", recorded: nil, actualRaw: "16.4",
		},
		{
			name: "same version", recorded: recordedAt("16.4", "16.4"), actualRaw: "16.4",
		},
		{
			// The whole point of comparing numeric prefixes: switching
			// the base image is not an environment drift.
			name:     "same version, different build",
			recorded: recordedAt("16.4", "16.4"), actualRaw: "16.4 (Debian 16.4-1.pgdg120+1)",
		},
		{
			// Recorded by a backend that never contacted a server.
			name: "record has no version", recorded: recordedAt("", ""), actualRaw: "16.4",
		},
		{
			// This run used such a backend: nothing to compare against.
			name: "run has no version", recorded: recordedAt("16.4", "16.4"), actualRaw: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, allow := range []bool{false, true} {
				if d, ok := serverDriftDiag(driftConfig(), c.recorded, c.actualRaw, allow); ok {
					t.Errorf("allow=%v: unexpected diagnostic %s: %s", allow, d.Code, d.Message)
				}
			}
		})
	}
}

func TestServerDriftDiag_FailsByDefault(t *testing.T) {
	d, ok := serverDriftDiag(driftConfig(),
		recordedAt("16.4", "16.4 (Debian 16.4-1.pgdg120+1)"), "16.9", false)
	if !ok {
		t.Fatal("a version disagreement must be reported")
	}
	if d.Code != diagnostics.CodeCacheServerDrift {
		t.Errorf("code = %s, want SQLETCH203", d.Code)
	}
	if d.Severity != diagnostics.Error {
		t.Error("without the flag, drift must be an error")
	}
	if d.Span.File != "/proj/sqletch.yaml" {
		t.Errorf("span file = %q, want the config path", d.Span.File)
	}
	// Both sides must be nameable from the message alone, including the
	// build that identifies the recorded server.
	for _, want := range []string{"16.4", "16.9", "Debian 16.4-1.pgdg120+1"} {
		if !strings.Contains(d.Message, want) {
			t.Errorf("message %q does not mention %q", d.Message, want)
		}
	}
	if !strings.Contains(d.Hint, "--allow-server-drift") {
		t.Errorf("hint must spell the escape hatch, got %q", d.Hint)
	}
	if !strings.Contains(d.Hint, ".sqletch/cache") {
		t.Errorf("hint must name the cache directory to regenerate, got %q", d.Hint)
	}
}

func TestServerDriftDiag_FlagDowngradesToWarning(t *testing.T) {
	d, ok := serverDriftDiag(driftConfig(), recordedAt("16.4", "16.4"), "16.9", true)
	if !ok {
		t.Fatal("the flag must not silence the report")
	}
	if d.Severity != diagnostics.Warning {
		t.Error("with the flag, drift must be a warning, never silence")
	}
	if d.Code != diagnostics.CodeCacheServerDrift {
		t.Errorf("code = %s, want SQLETCH203", d.Code)
	}
	if !strings.Contains(d.Hint, "16.9") {
		t.Errorf("hint must say which version is being recorded, got %q", d.Hint)
	}
}

func TestServerDriftDiag_MajorDrift(t *testing.T) {
	// The pin (SQLETCH200) only compares majors and only against
	// sqletch.yaml; a major change that the user also re-pinned still
	// has to be caught here, because the committed entries did not
	// change with it.
	d, ok := serverDriftDiag(driftConfig(), recordedAt("16.4", "16.4"), "17.2", false)
	if !ok || d.Severity != diagnostics.Error {
		t.Fatalf("a major drift must be an error, got ok=%v %+v", ok, d)
	}
}

func TestEnvRecord(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	cfg := driftConfig()
	got := envRecord(cfg, fp, "16.4 (Debian 16.4-1.pgdg120+1)")
	if got.SchemaFP != fp || got.Dialect != "postgres" {
		t.Errorf("unexpected record: %+v", got)
	}
	if got.OracleBackend != config.OracleServer {
		t.Errorf("backend = %q, want %q", got.OracleBackend, config.OracleServer)
	}
	if got.ServerVersion != "16.4" {
		t.Errorf("compared version = %q, want the numeric prefix", got.ServerVersion)
	}
	if got.ServerVersionRaw != "16.4 (Debian 16.4-1.pgdg120+1)" {
		t.Errorf("raw version = %q, want the full reported string", got.ServerVersionRaw)
	}
}

func TestEnvRecord_NativeBackend(t *testing.T) {
	cfg := config.Config{Dialect: "mysql", Path: "/proj/sqletch.yaml"}
	cfg.Database.Oracle = config.OracleNative
	got := envRecord(cfg, strings.Repeat("ab", 32), "")
	if got.OracleBackend != config.OracleNative {
		t.Errorf("backend = %q, want %q", got.OracleBackend, config.OracleNative)
	}
	if got.ServerVersion != "" || got.ServerVersionRaw != "" {
		t.Errorf("a backend that contacts no server must record no version: %+v", got)
	}
}

// The record a run writes must be one a later run reads back as
// agreeing with the same server — the round trip through the sidecar
// must not itself look like drift.
func TestEnvRecord_RoundTripsWithoutDrift(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	cfg := driftConfig()
	store := cache.NewStore(t.TempDir())
	if err := store.SaveEnv(envRecord(cfg, fp, "16.4 (Debian 16.4-1.pgdg120+1)")); err != nil {
		t.Fatal(err)
	}
	recorded, ok := store.LoadEnv(fp)
	if !ok {
		t.Fatal("record must load back")
	}
	if d, ok := serverDriftDiag(cfg, recorded, "16.4 (Debian 16.4-1.pgdg120+1)", false); ok {
		t.Errorf("same server must not drift against its own record: %s", d.Message)
	}
	// ...and an Alpine image of the same version still agrees.
	if d, ok := serverDriftDiag(cfg, recorded, "16.4", false); ok {
		t.Errorf("same version, different build must not drift: %s", d.Message)
	}
}
