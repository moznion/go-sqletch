package corpus

import (
	"bytes"
	"testing"

	"github.com/moznion/go-sqletch/internal/cache"
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
