//go:build devdb

package corpus

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/devdb"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
)

// TestMySQLCorpusGroundTruth re-derives every MySQL corpus case from
// a real server and byte-compares the answers with the committed
// files. This is what makes the corpus authoritative (design 15 D8):
// ground truth is re-derivation against the engine, never an entry's
// provenance. With SQLETCH_UPDATE_CORPUS set, it rewrites the case
// from the server's answers instead of comparing.
//
// The catalog's schema name is part of the ground truth, so a
// user-supplied SQLETCH_TEST_MYSQL_DSN must use a database named
// "sqletch" (the testcontainers default, devdb/mysql.go).
func TestMySQLCorpusGroundTruth(t *testing.T) {
	cases, err := LoadAll("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Dialect != "mysql" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			var schemaSQL []string
			for _, f := range c.Schema {
				schemaSQL = append(schemaSQL, string(f.Content))
			}
			conn, cleanup, err := devdb.AcquireMySQL(ctx, devdb.Config{
				DSN:              os.Getenv("SQLETCH_TEST_MYSQL_DSN"),
				AllowDestructive: true,
				ServerVersion:    c.ServerVersion,
				SchemaSQL:        schemaSQL,
			})
			if cleanup != nil {
				defer cleanup()
			}
			if err != nil {
				t.Fatal(err)
			}
			o := mysql.NewOracle(conn)

			if os.Getenv("SQLETCH_UPDATE_CORPUS") != "" {
				updateCase(ctx, t, c, o)
				return
			}
			ms, err := Replay(ctx, c, backendFor(o))
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range ms {
				t.Errorf("corpus drift vs real MySQL: %s", m)
			}
		})
	}
}

// updateCase rewrites a case's catalog and entries from the connected
// engine's answers. Filenames are stable (the fingerprint and the
// rendered SQL are unchanged), so an update only refreshes contents.
func updateCase(ctx context.Context, t *testing.T, c *Case, o dialect.Oracle) {
	t.Helper()
	store := cache.NewStore(filepath.Join(c.Dir, "cache"))
	snap, err := o.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snap.SchemaFP = c.FP
	if err := store.SaveCatalog(snap); err != nil {
		t.Fatal(err)
	}
	for _, e := range c.Entries {
		desc, err := o.Describe(ctx, e.E.RenderedSQL)
		if err != nil {
			t.Fatalf("%s: the real engine refuses a corpus input: %v", e.Path, err)
		}
		if err := store.SaveOracle(dialect.EntryFromDesc(c.FP, e.E.RenderedSQL, desc)); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%s: rewrote the catalog and %d entries", c.Name, len(c.Entries))
}
