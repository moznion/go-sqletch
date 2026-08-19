//go:build devdb

package e2e_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/cli"
	"github.com/moznion/go-sqletch/internal/devdb"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// TestCLIDestructiveResetGuard proves the H1 guard end to end against a
// REAL PostgreSQL: a cold run against a user-supplied database.dsn is
// refused with SQLETCH204 (nothing is dropped) unless --allow-destructive
// confirms the database is disposable.
func TestCLIDestructiveResetGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// A disposable container stands in for "a database the developer
	// pointed sqletch at": from the CLI's view its DSN is user-supplied.
	dsn, cleanup, err := devdb.AcquireDSN(ctx, devdb.Config{
		ServerVersion: "16",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	dir := t.TempDir()
	writeFile(t, dir, "db/schema.sql", "CREATE TABLE users (id bigint PRIMARY KEY, email text NOT NULL);\n")
	writeFile(t, dir, "queries/q.sql", "-- name: GetUser :one\nSELECT u.id, u.email FROM users AS u WHERE u.id = :id;\n")
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

	// Cold check WITHOUT the flag: refused with SQLETCH204, exit 1.
	var out, errW bytes.Buffer
	code := cli.Check(ctx, configPath, false, false, cli.RunOptions{}, &out, &errW)
	if code != cli.ExitDiagnostics {
		t.Fatalf("cold check without --allow-destructive: exit %d, want %d\n%s%s",
			code, cli.ExitDiagnostics, out.String(), errW.String())
	}
	if !strings.Contains(errW.String(), string(diagnostics.CodeDestructiveReset)) {
		t.Errorf("want %s in output:\n%s", diagnostics.CodeDestructiveReset, errW.String())
	}

	// Cold check WITH the flag: the reset proceeds and the query verifies.
	out.Reset()
	errW.Reset()
	code = cli.Check(ctx, configPath, false, false, cli.RunOptions{AllowDestructive: true}, &out, &errW)
	if code != cli.ExitOK {
		t.Fatalf("cold check with --allow-destructive: exit %d\n%s%s",
			code, out.String(), errW.String())
	}
}
