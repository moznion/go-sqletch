package cache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// storeSymlinkFixture is a cache directory next to an "outside"
// directory that a committed symlink inside the cache tree points at.
// The cache tree is attacker-shaped (a cloned repo), the outside
// directory is the victim.
type storeSymlinkFixture struct {
	store   *Store
	dir     string
	outside string
}

func newStoreSymlinkFixture(t *testing.T) storeSymlinkFixture {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "project", ".sqletch", "cache")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{dir, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return storeSymlinkFixture{store: NewStore(dir), dir: dir, outside: outside}
}

// link plants rel (cache-relative, slash form) as a symlink to the
// outside directory.
func (f storeSymlinkFixture) link(t *testing.T, rel string) {
	t.Helper()
	p := filepath.Join(f.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.outside, p); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// assertOutsideEmpty: nothing — no entry, no temp file, no directory —
// may appear in the victim directory.
func (f storeSymlinkFixture) assertOutsideEmpty(t *testing.T) {
	t.Helper()
	var got []string
	_ = filepath.WalkDir(f.outside, func(p string, _ os.DirEntry, err error) error {
		if err == nil && p != f.outside {
			got = append(got, p)
		}
		return nil
	})
	if len(got) != 0 {
		t.Errorf("the store wrote outside its tree through a committed symlink: %v", got)
	}
}

func assertSymlinkRefusal(t *testing.T, err error, wantRel string) {
	t.Helper()
	var se *SymlinkError
	if !errors.As(err, &se) {
		t.Fatalf("want a *SymlinkError refusal, got %v", err)
	}
	if se.Rel != wantRel {
		t.Errorf("SymlinkError.Rel = %q, want %q", se.Rel, wantRel)
	}
}

var symlinkRef = OracleRef{Target: "app/gen", Query: "GetUser", Shape: "maximal"}

// TestSaveOracle_RefusesSymlinkedOracleDir pins F8: a committed
// `oracle -> /elsewhere` symlink used to have MkdirAll/CreateTemp/Rename
// resolve straight through it, creating entries (and directories)
// outside the project on every miss-fill — in `check` too, and with no
// database at all on SQLite/native MySQL. Sweep and Walk skip symlinks,
// so nothing would ever clean the escaped files up either.
func TestSaveOracle_RefusesSymlinkedOracleDir(t *testing.T) {
	f := newStoreSymlinkFixture(t)
	f.link(t, OracleDir)
	err := f.store.SaveOracle(symlinkRef, &OracleEntry{SchemaFP: "fp", RenderedSQL: "SELECT 1"})
	assertSymlinkRefusal(t, err, OracleDir)
	f.assertOutsideEmpty(t)
}

// TestSaveOracle_RefusesSymlinkedIntermediateDir: the same, further
// down — every directory between the cache root and the leaf (a target
// slug segment, the query directory) is checked, not just `oracle`.
func TestSaveOracle_RefusesSymlinkedIntermediateDir(t *testing.T) {
	for _, rel := range []string{
		"oracle/app",
		"oracle/app/gen",
		"oracle/app/gen/GetUser",
	} {
		t.Run(rel, func(t *testing.T) {
			f := newStoreSymlinkFixture(t)
			f.link(t, rel)
			err := f.store.SaveOracle(symlinkRef, &OracleEntry{SchemaFP: "fp", RenderedSQL: "SELECT 1"})
			assertSymlinkRefusal(t, err, rel)
			f.assertOutsideEmpty(t)
		})
	}
}

// TestSaveOracle_ReplacesSymlinkedLeaf keeps WriteFileAtomic's leaf
// guarantee for the store's own writer: a symlink sitting AT the entry
// path is replaced by a regular file, never written through.
func TestSaveOracle_ReplacesSymlinkedLeaf(t *testing.T) {
	f := newStoreSymlinkFixture(t)
	victim := filepath.Join(f.outside, "victim.json")
	if err := os.WriteFile(victim, []byte("DO NOT TOUCH"), 0o644); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(f.dir, filepath.FromSlash(OracleFileName(symlinkRef)))
	if err := os.MkdirAll(filepath.Dir(leaf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, leaf); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := f.store.SaveOracle(symlinkRef, &OracleEntry{SchemaFP: "fp", RenderedSQL: "SELECT 1"}); err != nil {
		t.Fatalf("SaveOracle: %v", err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "DO NOT TOUCH" {
		t.Errorf("leaf symlink was written through: victim = %q", got)
	}
	fi, err := os.Lstat(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("entry is %v, want a regular file", fi.Mode())
	}
	if _, ok := f.store.LoadOracle(symlinkRef, "fp", "SELECT 1"); !ok {
		t.Error("the written entry does not load back")
	}
	ents, err := os.ReadDir(filepath.Dir(leaf))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		t.Errorf("leftover files next to the entry: %d entries", len(ents))
	}
}

// TestStore_SingletonWritesAreRootConfined: catalog.json and env.json
// go through the same writer; a symlink at their leaf is replaced, and
// the write leaves no temp file behind in the cache root.
func TestStore_SingletonWritesAreRootConfined(t *testing.T) {
	f := newStoreSymlinkFixture(t)
	for _, name := range []string{CatalogFile, EnvFile} {
		if err := os.Symlink(filepath.Join(f.outside, name), filepath.Join(f.dir, name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	if err := f.store.SaveCatalog(&Catalog{SchemaFP: "fp"}); err != nil {
		t.Fatalf("SaveCatalog: %v", err)
	}
	if err := f.store.SaveEnv(&Env{SchemaFP: "fp"}); err != nil {
		t.Fatalf("SaveEnv: %v", err)
	}
	f.assertOutsideEmpty(t)
	ents, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		if !e.Type().IsRegular() {
			t.Errorf("%s is %v, want a regular file", e.Name(), e.Type())
		}
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != CatalogFile+","+EnvFile {
		t.Errorf("cache root holds %v, want exactly the two singletons", names)
	}
}

// TestStore_WritesCreateAbsentRoot: the store still creates its own
// directory (and the oracle tree) on a cold first run.
func TestStore_WritesCreateAbsentRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "cache")
	s := NewStore(dir)
	if err := s.SaveOracle(symlinkRef, &OracleEntry{SchemaFP: "fp", RenderedSQL: "SELECT 1"}); err != nil {
		t.Fatalf("SaveOracle: %v", err)
	}
	if _, ok := s.LoadOracle(symlinkRef, "fp", "SELECT 1"); !ok {
		t.Error("the written entry does not load back")
	}
}

// TestStore_CheckWritable is the pipeline's preflight: it reports the
// first symlinked directory on any ref's path BEFORE a single file is
// written, so a refusal leaves the committed tree exactly as it was. A
// clean tree, an absent tree, and a symlinked LEAF (which the write
// replaces) all pass.
func TestStore_CheckWritable(t *testing.T) {
	other := OracleRef{Target: "app/gen", Query: "ListUsers", Shape: "maximal"}

	f := newStoreSymlinkFixture(t)
	if err := f.store.CheckWritable([]OracleRef{symlinkRef, other}); err != nil {
		t.Fatalf("empty tree: %v", err)
	}
	if err := NewStore(filepath.Join(f.dir, "absent")).CheckWritable([]OracleRef{symlinkRef}); err != nil {
		t.Fatalf("absent tree: %v", err)
	}

	f.link(t, "oracle/app/gen/ListUsers")
	assertSymlinkRefusal(t, f.store.CheckWritable([]OracleRef{symlinkRef, other}), "oracle/app/gen/ListUsers")
	if err := f.store.CheckWritable([]OracleRef{symlinkRef}); err != nil {
		t.Errorf("a ref whose path avoids the link must pass: %v", err)
	}

	g := newStoreSymlinkFixture(t)
	g.link(t, OracleFileName(symlinkRef))
	if err := g.store.CheckWritable([]OracleRef{symlinkRef}); err != nil {
		t.Errorf("a symlinked leaf is replaced, not followed, so it must pass: %v", err)
	}

	h := newStoreSymlinkFixture(t)
	h.link(t, OracleDir)
	assertSymlinkRefusal(t, h.store.CheckWritable(nil), OracleDir)
}
