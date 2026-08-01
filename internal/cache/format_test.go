package cache

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestFormatVersion(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	fp := strings.Repeat("ab", 32)

	cat := &Catalog{SchemaFP: fp, Tables: []Table{{Schema: "public", Name: "t", OID: 1}}}
	if err := s.SaveCatalog(cat); err != nil {
		t.Fatal(err)
	}
	e := &OracleEntry{SchemaFP: fp, RenderedSQL: "SELECT 1"}
	if err := s.SaveOracle(e); err != nil {
		t.Fatal(err)
	}

	// Written files self-describe their format version.
	catData, err := os.ReadFile(s.catalogPath(fp))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(catData), "\"format\": "+strconv.Itoa(FormatVersion)) {
		t.Fatalf("catalog file missing format marker:\n%s", catData)
	}

	// Round trip.
	if _, ok := s.LoadCatalog(fp); !ok {
		t.Fatal("catalog round trip failed")
	}
	if _, ok := s.LoadOracle(fp, "SELECT 1"); !ok {
		t.Fatal("oracle round trip failed")
	}

	// A future (or missing) format version is a MISS, never a misread:
	// the pipeline falls back to the database and rewrites the entry.
	bump := func(path string) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = []byte(strings.Replace(string(data),
			"\"format\": "+strconv.Itoa(FormatVersion),
			"\"format\": "+strconv.Itoa(FormatVersion+1), 1))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bump(s.catalogPath(fp))
	bump(s.oraclePath(queryHash(fp, "SELECT 1")))
	if _, ok := s.LoadCatalog(fp); ok {
		t.Error("newer-format catalog must be a miss")
	}
	if _, ok := s.LoadOracle(fp, "SELECT 1"); ok {
		t.Error("newer-format oracle entry must be a miss")
	}
}
