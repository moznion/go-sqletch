package corpus

import (
	"bytes"
	"context"
	"testing"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
)

// TestNativeCatalogMatchesCorpus is the offline differential gate for
// the native catalog builder (design 15 §7.2): building the catalog
// from each MySQL case's schema DDL must reproduce the committed,
// server-captured catalog byte for byte.
func TestNativeCatalogMatchesCorpus(t *testing.T) {
	for _, c := range loadCommitted(t) {
		if c.Dialect != "mysql" {
			continue
		}
		cat, err := mysql.BuildCatalog(c.Schema)
		if err != nil {
			t.Errorf("%s: BuildCatalog: %v", c.Name, err)
			continue
		}
		cat.SchemaFP = c.FP
		got, err := cache.EncodeCatalog(cat)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, c.CatalogBytes) {
			t.Errorf("%s: native catalog differs from the server-captured one:\n%s",
				c.Name, firstDiff(c.CatalogBytes, got))
		}
	}
}

// TestNativeMySQLReplaysCorpus is THE offline differential gate
// (design 15 §7.2): the native backend must reproduce every committed
// server answer byte for byte, with no Docker anywhere.
func TestNativeMySQLReplaysCorpus(t *testing.T) {
	for _, c := range loadCommitted(t) {
		if c.Dialect != "mysql" {
			continue
		}
		backend := func(_ context.Context, c *Case) (dialect.Oracle, func(), error) {
			o, err := mysql.NewNativeOracle(c.Schema, c.ServerVersion)
			return o, nil, err
		}
		ms, err := Replay(context.Background(), c, backend)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		for _, m := range ms {
			t.Errorf("%s: native disagrees with the server ground truth: %s", c.Name, m)
		}
	}
}
