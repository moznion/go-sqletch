package cli

import (
	"testing"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/config"
)

// Design 26 D4: loadOracleEntry — the one committed oracle read shared
// by pipeline.Run and the OfflineChecker (loadDescs) — treats an entry
// written before PostgreSQL Describe recorded type kinds as a miss.
func TestLoadOracleEntry_StaleIsMiss(t *testing.T) {
	store := cache.NewStore(t.TempDir())
	ref := cache.OracleRef{Target: "gen", Query: "Q", Shape: "maximal"}
	save := func(e cache.OracleEntry) {
		t.Helper()
		e.SchemaFP, e.RenderedSQL = "fp", "SELECT"
		if err := store.SaveOracle(ref, &e); err != nil {
			t.Fatal(err)
		}
	}
	pg := driverFor(config.Config{Dialect: "postgres"})
	my := driverFor(config.Config{Dialect: "mysql"})

	preDesign := cache.OracleEntry{Params: []cache.EntryType{{OID: 16400, Name: "event_kind"}}}
	save(preDesign)
	if _, ok := loadOracleEntry(pg, store, ref, "fp", "SELECT"); ok {
		t.Error("postgres: a pre-design user-defined-type entry must miss")
	}
	// MySQL type codes carry flag bits above 16384; the rule is
	// PostgreSQL's alone.
	if _, ok := loadOracleEntry(my, store, ref, "fp", "SELECT"); !ok {
		t.Error("mysql: the staleness rule must not apply")
	}

	save(cache.OracleEntry{Params: []cache.EntryType{{OID: 16400, Name: "event_kind", Schema: "public", Kind: "e", Labels: []string{"A"}}}})
	if _, ok := loadOracleEntry(pg, store, ref, "fp", "SELECT"); !ok {
		t.Error("postgres: a described entry must hit")
	}
	if _, ok := loadOracleEntry(pg, store, ref, "other-fp", "SELECT"); ok {
		t.Error("the store's key comparison still applies")
	}
}
