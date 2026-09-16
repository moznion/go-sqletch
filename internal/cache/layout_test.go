package cache

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestOracleFileName_Layout(t *testing.T) {
	cases := []struct {
		ref  OracleRef
		want string
	}{
		{OracleRef{Target: "gen", Query: "GetUser", Shape: "maximal"},
			"oracle/gen/GetUser/maximal.json"},
		{OracleRef{Target: "internal/db/gen", Query: "SearchUsers", Shape: "case-sort-email_asc"},
			"oracle/internal/db/gen/SearchUsers/case-sort-email_asc.json"},
	}
	for _, c := range cases {
		if got := OracleFileName(c.ref); got != c.want {
			t.Errorf("OracleFileName(%+v) = %q, want %q", c.ref, got, c.want)
		}
	}
}

// TestOracleFileName_NoTraversal is a security property, not an
// aesthetic one: these components come from a repo-controlled config
// and repo-controlled templates, and the tree they name is written
// to. No spelling may climb out of the cache directory.
func TestOracleFileName_NoTraversal(t *testing.T) {
	refs := []OracleRef{
		{Target: "../../etc", Query: "..", Shape: ".."},
		{Target: "/abs/path", Query: "a/b", Shape: "c/d"},
		{Target: "", Query: "", Shape: ""},
		{Target: "gen", Query: "q", Shape: "..%2f..%2fetc%2fpasswd"},
	}
	for _, r := range refs {
		got := OracleFileName(r)
		if !strings.HasPrefix(got, "oracle/") {
			t.Errorf("%+v: %q does not stay under oracle/", r, got)
		}
		for _, seg := range strings.Split(got, "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Errorf("%+v: %q has a traversing component", r, got)
			}
		}
		if clean := filepath.ToSlash(filepath.Clean(got)); clean != got {
			t.Errorf("%+v: %q is not already clean (%q)", r, got, clean)
		}
	}
}

func TestParseOracleRef_RoundTrip(t *testing.T) {
	refs := []OracleRef{
		{Target: "gen", Query: "GetUser", Shape: "maximal"},
		{Target: "internal/db/gen", Query: "q", Shape: "tree-empty-scope"},
		{Target: "_corpus", Query: "agree-000", Shape: "maximal"},
	}
	for _, want := range refs {
		got, ok := ParseOracleRef(OracleFileName(want))
		if !ok || got != want {
			t.Errorf("round trip of %+v = %+v, ok=%v", want, got, ok)
		}
	}
}

func TestParseOracleRef_Rejects(t *testing.T) {
	for _, rel := range []string{
		"catalog.json",
		"env.json",
		"oracle/gen/q.json",     // no shape component
		"oracle/q/maximal.json", // no target component
		"oracle/gen/q/maximal",  // not a .json file
		"other/gen/q/maximal.json",
	} {
		if _, ok := ParseOracleRef(rel); ok {
			t.Errorf("ParseOracleRef(%q) must be rejected", rel)
		}
	}
}

// TestStore_SingletonFileNames pins that the fingerprint is not part of
// any file NAME (doc 21 D1): two fingerprints write the same paths, so
// a schema change shows up as a modification.
func TestStore_SingletonFileNames(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if got := s.catalogPath(); got != filepath.Join(dir, "catalog.json") {
		t.Errorf("catalogPath = %q", got)
	}
	if got := s.envPath(); got != filepath.Join(dir, "env.json") {
		t.Errorf("envPath = %q", got)
	}

	fpA, fpB := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	if err := s.SaveCatalog(&Catalog{SchemaFP: fpA}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCatalog(&Catalog{SchemaFP: fpB}); err != nil {
		t.Fatal(err)
	}
	// The second save replaced the first: a fingerprint mismatch is a
	// miss, never a second file.
	if _, ok := s.LoadCatalog(fpA); ok {
		t.Error("a superseded fingerprint must miss")
	}
	if _, ok := s.LoadCatalog(fpB); !ok {
		t.Error("the current fingerprint must hit")
	}
}
