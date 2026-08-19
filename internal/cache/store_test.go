package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFingerprint_Stability(t *testing.T) {
	files := []SchemaFile{
		{Path: "a.sql", Content: []byte("CREATE TABLE a ();")},
		{Path: "b.sql", Content: []byte("CREATE TABLE b ();")},
	}
	fp1 := Fingerprint("postgres", "16", files)
	fp2 := Fingerprint("postgres", "16", files)
	if fp1 != fp2 {
		t.Fatal("fingerprint must be deterministic")
	}
	if Fingerprint("postgres", "17", files) == fp1 {
		t.Error("server version must affect the fingerprint")
	}
	if Fingerprint("mysql", "16", files) == fp1 {
		t.Error("dialect must affect the fingerprint")
	}
	changed := []SchemaFile{files[0], {Path: "b.sql", Content: []byte("CREATE TABLE b (id int);")}}
	if Fingerprint("postgres", "16", changed) == fp1 {
		t.Error("content change must affect the fingerprint")
	}
	reordered := []SchemaFile{files[1], files[0]}
	if Fingerprint("postgres", "16", reordered) == fp1 {
		t.Error("file order must affect the fingerprint")
	}
}

func TestStore_CatalogRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())
	cat := &Catalog{SchemaFP: strings.Repeat("ab", 32), Tables: []Table{
		{Schema: "public", Name: "users", OID: 1, Cols: []Column{{Name: "id", Att: 1, NotNull: true}}},
	}}
	if err := s.SaveCatalog(cat); err != nil {
		t.Fatal(err)
	}
	got, ok := s.LoadCatalog(cat.SchemaFP)
	if !ok || len(got.Tables) != 1 || got.Tables[0].Name != "users" {
		t.Fatalf("round trip failed: ok=%v got=%+v", ok, got)
	}
	if _, ok := s.LoadCatalog(strings.Repeat("cd", 32)); ok {
		t.Error("wrong fingerprint must miss")
	}
}

func TestStore_SaveCatalogWithoutFP(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.SaveCatalog(&Catalog{}); err == nil {
		t.Fatal("saving a catalog without fingerprint must fail")
	}
}

func TestStore_OracleRoundTripAndStoreCompare(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	e := &OracleEntry{
		SchemaFP:    "fp1",
		RenderedSQL: "SELECT $1",
		Params:      []EntryType{{OID: 25, Name: "text"}},
		Columns:     []EntryColumn{{Name: "c", OID: 25, TypeName: "text"}},
	}
	if err := s.SaveOracle(e); err != nil {
		t.Fatal(err)
	}
	got, ok := s.LoadOracle("fp1", "SELECT $1")
	if !ok || got.Params[0].OID != 25 {
		t.Fatalf("round trip failed: ok=%v got=%+v", ok, got)
	}
	if _, ok := s.LoadOracle("fp2", "SELECT $1"); ok {
		t.Error("different fingerprint must miss")
	}
	if _, ok := s.LoadOracle("fp1", "SELECT $2"); ok {
		t.Error("different SQL must miss")
	}

	// Store-and-compare: a doctored file whose content doesn't match
	// its stored keys is treated as a miss, not trusted.
	path := s.oraclePath(queryHash("fp1", "SELECT $1"))
	doctored := strings.Replace(string(mustRead(t, path)), `"rendered_sql": "SELECT $1"`, `"rendered_sql": "SELECT $9"`, 1)
	if err := os.WriteFile(path, []byte(doctored), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LoadOracle("fp1", "SELECT $1"); ok {
		t.Error("mismatched stored keys must be treated as a miss")
	}
}

func TestStore_CanonicalJSON(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	e := &OracleEntry{SchemaFP: "fp", RenderedSQL: "SELECT 1"}
	if err := s.SaveOracle(e); err != nil {
		t.Fatal(err)
	}
	path := s.oraclePath(queryHash("fp", "SELECT 1"))
	first := mustRead(t, path)
	if err := s.SaveOracle(e); err != nil {
		t.Fatal(err)
	}
	second := mustRead(t, path)
	if string(first) != string(second) {
		t.Error("cache files must be byte-stable across saves")
	}
	if !strings.HasSuffix(string(first), "\n") {
		t.Error("cache files must end with a newline")
	}
	if filepath.Ext(path) != ".json" {
		t.Errorf("unexpected path %q", path)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
