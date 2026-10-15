//go:build devdb

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/cli"
	"github.com/moznion/go-sqletch/internal/devdb"
)

// TestCatalogEpoch_ResetDevDBKeepsNullability is the counterexample for
// the mixed-epoch bug: against a configured DSN every connected run
// resets the public schema, so PostgreSQL assigns fresh OIDs. A run that
// added one query used to describe it under the NEW OIDs while keeping
// the committed catalog's OLD ones — provenance could not match, and the
// new query's NOT NULL columns silently became optional.Option. A
// connected run must now leave catalog and entries in one epoch.
func TestCatalogEpoch_ResetDevDBKeepsNullability(t *testing.T) {
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
	writeFile(t, dir, "db/schema.sql", "CREATE TABLE t (id bigint PRIMARY KEY, name text NOT NULL, note text);\n")
	writeFile(t, dir, "queries/a.sql", "-- name: A :many\nSELECT id, name, note FROM t;\n")
	writeFile(t, dir, "sqletch.yaml", `version: 1
dialect: postgres
server_version: "16"
database:
  dsn: `+dsn+`
schema:
  files: [db/schema.sql]
targets:
  - queries: [queries/*.sql]
    output: {package: gen, path: gen}
cache:
  path: .sqletch/cache
`)
	configPath := filepath.Join(dir, "sqletch.yaml")
	generate := func() string {
		t.Helper()
		var out, errW bytes.Buffer
		if code := cli.Generate(ctx, configPath, false, cli.RunOptions{AllowDestructive: true}, &out, &errW); code != cli.ExitOK {
			t.Fatalf("generate: exit %d\n%s%s", code, out.String(), errW.String())
		}
		return out.String()
	}
	rowPattern := regexp.MustCompile(`ID\s+int64\n\s+Name\s+string\n\s+Note\s+optional\.Option\[string\]`)

	generate()
	if src := readFile(t, dir, "gen/a.sql.gen.go"); !rowPattern.MatchString(src) {
		t.Fatalf("cold A row:\n%s", src)
	}

	// Second connected run: the schema reset hands out new OIDs.
	writeFile(t, dir, "queries/b.sql", "-- name: B :many\nSELECT id, name, note FROM t;\n")
	if out := generate(); !strings.Contains(out, "0 hits") {
		t.Errorf("a new epoch must re-describe every rendering: %s", out)
	}
	for _, f := range []string{"gen/a.sql.gen.go", "gen/b.sql.gen.go"} {
		if src := readFile(t, dir, f); !rowPattern.MatchString(src) {
			t.Errorf("%s: NOT NULL columns must stay plain after a mixed run:\n%s", f, src)
		}
	}

	// Every committed entry's provenance resolves in the committed
	// catalog — the invariant the offline runs rely on.
	var cat struct {
		Tables []struct {
			OID uint32 `json:"oid"`
		} `json:"tables"`
	}
	if err := json.Unmarshal([]byte(readFile(t, dir, ".sqletch/cache/catalog.json")), &cat); err != nil {
		t.Fatal(err)
	}
	tables := map[uint32]bool{}
	for _, tb := range cat.Tables {
		tables[tb.OID] = true
	}
	err = filepath.WalkDir(filepath.Join(dir, ".sqletch/cache/oracle"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var e struct {
			Columns []struct {
				Name   string `json:"name"`
				SrcRel uint32 `json:"src_rel"`
			} `json:"columns"`
		}
		if err := json.Unmarshal(b, &e); err != nil {
			return err
		}
		for _, c := range e.Columns {
			if c.SrcRel != 0 && !tables[c.SrcRel] {
				t.Errorf("%s: column %s src_rel %d is not in the committed catalog", p, c.Name, c.SrcRel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
