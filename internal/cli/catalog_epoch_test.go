package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/config"
)

// A connected run must never mix the committed catalog with entries it
// describes against a database whose object identities differ from it
// (a reset PostgreSQL dev DB assigns fresh OIDs every time): provenance
// keys on those identities, so the new entries' result columns would
// fail to match and silently widen to Option. The native MySQL oracle
// is deterministic and in-process, so it stands in for "same epoch"
// here; the real reset-DB case is the devdb TestCatalogEpoch_* suite.

func readCacheFiles(t *testing.T, cfg config.Config) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, rel := range cacheTree(t, cfg) {
		b, err := os.ReadFile(filepath.Join(cfg.Abs(cfg.Cache.Path), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		out[rel] = string(b)
	}
	return out
}

func addSigninQuery(t *testing.T, cfg config.Config) {
	t.Helper()
	p := filepath.Join(cfg.Dir, "app/signin/queries/more.sql")
	src := strings.Replace(nativeCLIQuery, "SearchUsers", "SearchMore", 1)
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Same epoch: the committed entries stay hits and keep every byte; only
// the new query is described.
func TestCatalogEpoch_SameEpochKeepsHits(t *testing.T) {
	cfg := writeFanOutProject(t, "")
	mustGenerate(t, cfg)
	before := readCacheFiles(t, cfg)

	addSigninQuery(t, cfg)
	res := mustGenerate(t, cfg)
	if res.OracleHits != 2 || res.OracleMiss != 1 {
		t.Fatalf("hits/misses = %d/%d, want 2/1", res.OracleHits, res.OracleMiss)
	}
	after := readCacheFiles(t, cfg)
	for rel, b := range before {
		if after[rel] != b {
			t.Errorf("%s changed although the database matches the committed catalog", rel)
		}
	}
}

// Different epoch: the committed catalog no longer describes the live
// database, so the run adopts a fresh snapshot and re-describes EVERY
// rendering — no entry is reused against a catalog it was not
// described with.
func TestCatalogEpoch_DifferentEpochRedescribesAll(t *testing.T) {
	cfg := writeFanOutProject(t, "")
	mustGenerate(t, cfg)
	fresh := readCacheFiles(t, cfg)

	// Simulate a committed cache from another epoch: same fingerprint,
	// different object identities, consistently in catalog AND entries.
	tamper := func(rel string) {
		t.Helper()
		s := fresh[rel]
		s2 := strings.ReplaceAll(s, `"oid": 1,`, `"oid": 901,`)
		s2 = strings.ReplaceAll(s2, `"src_rel": 1,`, `"src_rel": 901,`)
		if s2 == s {
			t.Fatalf("%s: nothing to tamper:\n%s", rel, s)
		}
		writeCacheFile(t, cfg, rel, s2)
	}
	tamper("catalog.json")
	tamper("oracle/app/signin/gen/SearchUsers/maximal.json")
	tamper("oracle/app/signup/gen/SearchUsers/maximal.json")

	addSigninQuery(t, cfg)
	res := mustGenerate(t, cfg)
	if res.OracleHits != 0 || res.OracleMiss != 3 {
		t.Fatalf("hits/misses = %d/%d, want 0/3 (every rendering re-described)", res.OracleHits, res.OracleMiss)
	}
	after := readCacheFiles(t, cfg)
	for rel, b := range fresh {
		if after[rel] != b {
			t.Errorf("%s was not rewritten from the live database:\n%s\nwant:\n%s", rel, after[rel], b)
		}
	}
}
