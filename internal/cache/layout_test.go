package cache

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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
		got, ok := parseOracleRef(OracleFileName(want))
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
		if _, ok := parseOracleRef(rel); ok {
			t.Errorf("parseOracleRef(%q) must be rejected", rel)
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

// writeStoreFile plants a file at a store-relative slash path.
func writeStoreFile(t *testing.T, dir, rel string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// storeTree lists every file under dir, as sorted slash paths.
func storeTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestStore_Walk(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	for _, rel := range []string{
		"oracle/gen/B/maximal.json",
		"oracle/gen/A/maximal.json",
		"oracle/gen/A/case-sort-x.json",
		"oracle/deadbeefdeadbeefdeadbeef.json", // v1 flat: not an entry path
		"oracle/gen/A/NOTES.md",                // not JSON
		"catalog.json",                         // not under oracle/
	} {
		writeStoreFile(t, dir, rel)
	}

	var got []string
	if err := s.Walk(func(ref OracleRef, p string) error {
		got = append(got, OracleFileName(ref))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Only entry paths, in walk order (lexical, parents first).
	want := []string{
		"oracle/gen/A/case-sort-x.json",
		"oracle/gen/A/maximal.json",
		"oracle/gen/B/maximal.json",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Walk visited %v, want %v", got, want)
	}

	// An absent tree is not an error: a store nothing has written yet.
	if err := NewStore(t.TempDir()).Walk(func(OracleRef, string) error { return nil }); err != nil {
		t.Errorf("Walk on an empty store: %v", err)
	}
}

// TestStore_Sweep is the mechanism behind design 21 §4: what the caller
// did not declare live goes, and what the store does not write stays.
func TestStore_Sweep(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	live := OracleRef{Target: "gen", Query: "Keep", Shape: "maximal"}
	for _, rel := range []string{
		OracleFileName(live),
		"oracle/gen/Gone/maximal.json",           // deleted query
		"oracle/gen/Keep/case-sort-gone.json",    // deleted shape
		"oracle/former/target/Keep/maximal.json", // renamed target
		"oracle/deadbeefdeadbeefdeadbeef.json",   // v1 entry
		"catalog-a662b82ee1b8313dbbd5c652.json",  // v1 catalog
		"env-a662b82ee1b8313dbbd5c652.json",      // v1 sidecar
		"catalog.json", "env.json",               // the store's own
		".gitignore", "package.json", // not the store's
		"oracle/gen/NOTES.md", "other/tool/data.json", // not the store's
	} {
		writeStoreFile(t, dir, rel)
	}

	if err := s.Sweep(map[OracleRef]bool{live: true}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		".gitignore",
		"catalog.json",
		"env.json",
		"oracle/gen/Keep/maximal.json",
		"oracle/gen/NOTES.md",
		"other/tool/data.json",
		"package.json",
	}
	if got := storeTree(t, dir); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("after Sweep:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A directory emptied by the sweep leaves no husk.
	if _, err := os.Stat(filepath.Join(dir, "oracle", "former")); !os.IsNotExist(err) {
		t.Errorf("an emptied directory must be removed, stat err = %v", err)
	}
}

func TestStore_SweepEmptyStore(t *testing.T) {
	if err := NewStore(t.TempDir()).Sweep(nil); err != nil {
		t.Errorf("Sweep on a store nothing has written: %v", err)
	}
}

// TestStore_SweepLeavesSymlinksAlone: the cache tree is committed, so a
// clone can plant one. The sweep never follows or deletes it.
func TestStore_SweepLeavesSymlinksAlone(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(secret, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, OracleDir), 0o755); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(dir, OracleDir, "planted.json")
	if err := os.Symlink(secret, planted); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := NewStore(dir).Sweep(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Errorf("the symlink target was removed: %v", err)
	}
	if _, err := os.Lstat(planted); err != nil {
		t.Errorf("a non-regular file was removed: %v", err)
	}
}
