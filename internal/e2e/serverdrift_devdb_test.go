//go:build devdb

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/cli"
	"github.com/moznion/go-sqletch/internal/devdb"
)

// TestCLIServerDrift covers SQLETCH203 against a REAL PostgreSQL: the
// committed cache records the server that produced it, and a later run
// that connects to a different one refuses to extend it.
//
// PostgreSQL is the interesting engine here because `SHOW
// server_version` carries a build suffix on some images ("16.4 (Debian
// …)") and none on others — the recorded value must survive that, or
// every base-image change would read as a drifted environment.
func TestCLIServerDrift(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn, cleanup, err := devdb.AcquireDSN(ctx, devdb.Config{
		DSN:              os.Getenv("SQLETCH_TEST_DSN"),
		AllowDestructive: true,
		ServerVersion:    "16",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	dir := t.TempDir()
	writeFile(t, dir, "db/schema.sql",
		"CREATE TABLE users (id bigserial PRIMARY KEY, status text NOT NULL);\n")
	writeFile(t, dir, "queries/users.sql", `-- name: ListUsers :many
SELECT u.id FROM users AS u WHERE u.status = :status;
`)
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
	configPath := filepath.Join(dir, "sqletch.yaml")

	check := func(exhaustive bool, opts cli.RunOptions) (int, string, string) {
		var out, errW bytes.Buffer
		code := cli.Check(ctx, configPath, exhaustive, false, opts, &out, &errW)
		return code, out.String(), errW.String()
	}

	// 1. Cold generate records what it connected to. The config's dsn is
	// a disposable container, so the DB-touching runs opt into the reset.
	var out, errW bytes.Buffer
	if code := cli.Generate(ctx, configPath, false, cli.RunOptions{AllowDestructive: true}, &out, &errW); code != cli.ExitOK {
		t.Fatalf("cold generate: exit %d\n%s%s", code, out.String(), errW.String())
	}

	sidecars, err := filepath.Glob(filepath.Join(dir, ".sqletch/cache/env-*.json"))
	if err != nil || len(sidecars) != 1 {
		t.Fatalf("want exactly one env sidecar, got %v (%v)", sidecars, err)
	}
	sidecar := sidecars[0]
	recorded := readEnv(t, sidecar)
	if recorded.Dialect != "postgres" || recorded.OracleBackend != "server" {
		t.Errorf("unexpected record: %+v", recorded)
	}
	if !strings.HasPrefix(recorded.ServerVersion, "16.") {
		t.Errorf("recorded version %q does not look like the server's", recorded.ServerVersion)
	}
	// Whatever the image spells, the compared value is the numeric
	// prefix of the raw string the server reported.
	if want := cache.NumericVersionPrefix(recorded.ServerVersionRaw); recorded.ServerVersion != want {
		t.Errorf("recorded %q, but the raw string %q reduces to %q",
			recorded.ServerVersion, recorded.ServerVersionRaw, want)
	}

	// 2. Reconnecting to the same server is not drift, however the
	// version happens to be spelled.
	if code, out, errOut := check(true, cli.RunOptions{AllowDestructive: true}); code != cli.ExitOK {
		t.Fatalf("same server must not drift: exit %d\n%s%s", code, out, errOut)
	}

	// 3. Doctor the record: the committed cache now claims to come from
	// a server this run is not talking to.
	writeEnvVersion(t, sidecar, "16.0")
	code, _, errOut := check(true, cli.RunOptions{AllowDestructive: true})
	if code != cli.ExitDiagnostics {
		t.Fatalf("drift must fail with diagnostics: exit %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "SQLETCH203") {
		t.Errorf("want SQLETCH203 in:\n%s", errOut)
	}
	if got := readEnv(t, sidecar).ServerVersion; got != "16.0" {
		t.Errorf("a refused run rewrote the committed record to %q", got)
	}

	// 4. The escape hatch accepts it, warns, and adopts the connected
	// server — a build that no single environment produced, on purpose.
	code, _, errOut = check(true, cli.RunOptions{AllowServerDrift: true, AllowDestructive: true})
	if code != cli.ExitOK {
		t.Fatalf("--allow-server-drift: exit %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "SQLETCH203") || !strings.Contains(errOut, "warning") {
		t.Errorf("the flag must downgrade to a warning, not silence:\n%s", errOut)
	}
	if got := readEnv(t, sidecar).ServerVersion; got != recorded.ServerVersion {
		t.Errorf("accepting drift must adopt the connected server: %q, want %q",
			got, recorded.ServerVersion)
	}
}

func readEnv(t *testing.T, path string) cache.Env {
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

func writeEnvVersion(t *testing.T, path, version string) {
	t.Helper()
	e := readEnv(t, path)
	e.ServerVersion, e.ServerVersionRaw = version, version
	data, err := cache.EncodeEnv(&e)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
