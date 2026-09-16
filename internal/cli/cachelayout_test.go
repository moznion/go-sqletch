package cli

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// cacheTree lists every file under the project's cache directory, as
// slash-separated cache-relative paths, sorted.
func cacheTree(t *testing.T, cfg config.Config) []string {
	t.Helper()
	root := cfg.Abs(cfg.Cache.Path)
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
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

func writeCacheFile(t *testing.T, cfg config.Config, rel, content string) string {
	t.Helper()
	p := filepath.Join(cfg.Abs(cfg.Cache.Path), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestGenerate_NamedCacheLayout pins the committed tree's shape
// (docs/design/21-cache-layout.md §3): singleton catalog/env files with
// no fingerprint in their names, and one entry per rendering under
// <target-slug>/<query>/<shape>.json. The reviewer of a pull request
// reads these paths, so they are part of the contract.
func TestGenerate_NamedCacheLayout(t *testing.T) {
	cfg := writeFanOutProject(t, "")
	mustGenerate(t, cfg)

	want := []string{
		"catalog.json",
		"env.json",
		"oracle/app/signin/gen/SearchUsers/maximal.json",
		"oracle/app/signup/gen/SearchUsers/maximal.json",
	}
	got := cacheTree(t, cfg)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("cache tree:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestGenerate_CacheTreeIsStable: regenerating an unchanged project
// must not move or rewrite a single byte — the property that makes the
// tree reviewable in the first place.
func TestGenerate_CacheTreeIsStable(t *testing.T) {
	cfg := writeFanOutProject(t, "")
	mustGenerate(t, cfg)
	before := map[string]string{}
	for _, rel := range cacheTree(t, cfg) {
		data, err := os.ReadFile(filepath.Join(cfg.Abs(cfg.Cache.Path), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		before[rel] = string(data)
	}

	mustGenerate(t, cfg)
	for _, rel := range cacheTree(t, cfg) {
		data, err := os.ReadFile(filepath.Join(cfg.Abs(cfg.Cache.Path), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		prev, ok := before[rel]
		if !ok {
			t.Errorf("%s appeared on a second generate", rel)
			continue
		}
		if prev != string(data) {
			t.Errorf("%s changed on a second generate", rel)
		}
		delete(before, rel)
	}
	for rel := range before {
		t.Errorf("%s disappeared on a second generate", rel)
	}
}

// TestGenerate_PrunesWhatItDidNotWrite is design 21 §4: after a
// generate the committed tree is the live set and nothing else — no
// entry for a deleted query, no leftover from the pre-v2 hash-named
// layout, no catalog from a superseded fingerprint.
func TestGenerate_PrunesWhatItDidNotWrite(t *testing.T) {
	cfg := writeFanOutProject(t, "")
	mustGenerate(t, cfg)

	stale := []string{
		"oracle/1f0c7ab2ddc0ffee1f0c7ab2.json",              // v1 layout
		"oracle/app/signin/gen/DeletedQuery/maximal.json",   // deleted query
		"oracle/app/signin/gen/SearchUsers/case-x-y.json",   // deleted shape
		"oracle/former/target/gen/SearchUsers/maximal.json", // renamed target
		"catalog-577379344cb73d81090d4e34.json",             // v1 catalog
		"env-577379344cb73d81090d4e34.json",                 // v1 sidecar
	}
	for _, rel := range stale {
		writeCacheFile(t, cfg, rel, "{}\n")
	}
	// Files sqletch does not write are not sqletch's to delete: any
	// non-JSON, and anything outside oracle/ that is not a catalog or
	// env file (cache.path is config — it may point into a directory
	// somebody else also owns).
	writeCacheFile(t, cfg, ".gitignore", "*.tmp\n")
	writeCacheFile(t, cfg, "oracle/app/NOTES.md", "why this package exists\n")
	writeCacheFile(t, cfg, "package.json", "{}\n")
	writeCacheFile(t, cfg, "other-tool/state.json", "{}\n")

	mustGenerate(t, cfg)

	want := []string{
		".gitignore",
		"catalog.json",
		"env.json",
		"oracle/app/NOTES.md",
		"oracle/app/signin/gen/SearchUsers/maximal.json",
		"oracle/app/signup/gen/SearchUsers/maximal.json",
		"other-tool/state.json",
		"package.json",
	}
	if got := cacheTree(t, cfg); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("after prune:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A directory emptied by the sweep leaves no husk.
	if _, err := os.Stat(filepath.Join(cfg.Abs(cfg.Cache.Path), "oracle", "former")); !os.IsNotExist(err) {
		t.Errorf("an emptied directory must be removed, stat err = %v", err)
	}
}

// TestCheck_DoesNotPrune: `check` fills misses but must never mutate
// the tree it is checking (design 21 D3) — a CI check that starts
// deleting files is a surprise nobody asked for.
func TestCheck_DoesNotPrune(t *testing.T) {
	cfg := writeFanOutProject(t, "")
	mustGenerate(t, cfg)
	stalePath := writeCacheFile(t, cfg, "oracle/app/signin/gen/Gone/maximal.json", "{}\n")

	for _, mode := range []Mode{ModeCheck, ModeCheckExhaustive} {
		res, err := Run(context.Background(), cfg, mode, RunOptions{})
		if err != nil {
			t.Fatalf("mode %v: %v", mode, err)
		}
		if diagnostics.HasErrors(res.Diags) {
			t.Fatalf("mode %v: %+v", mode, res.Diags)
		}
		if _, err := os.Stat(stalePath); err != nil {
			t.Fatalf("mode %v removed a cache entry: %v", mode, err)
		}
	}
}

// TestPruneCache_OutsideProjectIsRefused: a repo-controlled config does
// not get to make sqletch delete files outside the project it was
// pointed at. The refusal is a warning plus an untouched tree.
func TestPruneCache_OutsideProjectIsRefused(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	outside := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "precious.json")
	if err := os.WriteFile(victim, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Dir: project, Path: filepath.Join(project, "sqletch.yaml")}
	cfg.Cache.Path = filepath.Join("..", "elsewhere")
	diags, err := pruneCache(cfg, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) != 1 || diags[0].Code != diagnostics.CodePathEscape || diags[0].Severity != diagnostics.Warning {
		t.Fatalf("want one SQLETCH306 warning, got %+v", diags)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("a file outside the project was removed: %v", err)
	}
}

// TestPruneCache_NoCacheDirectory: a project that has never generated
// has nothing to sweep, and that is not an error.
func TestPruneCache_NoCacheDirectory(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{Dir: dir, Path: filepath.Join(dir, "sqletch.yaml")}
	cfg.Cache.Path = filepath.Join(".sqletch", "cache")
	diags, err := pruneCache(cfg, map[string]bool{})
	if err != nil || len(diags) != 0 {
		t.Fatalf("pruneCache on a fresh project: diags=%+v err=%v", diags, err)
	}
}

// TestPruneCache_LeavesSymlinksAlone: the cache tree is committed, so a
// clone can plant one. The sweep never follows or deletes it.
func TestPruneCache_LeavesSymlinksAlone(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, ".sqletch", "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "secret.json")
	if err := os.WriteFile(secret, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(cacheDir, "planted.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := config.Config{Dir: dir, Path: filepath.Join(dir, "sqletch.yaml")}
	cfg.Cache.Path = filepath.Join(".sqletch", "cache")
	if _, err := pruneCache(cfg, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("the symlink target was removed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cacheDir, "planted.json")); err != nil {
		t.Fatalf("a non-regular file was removed: %v", err)
	}
}

// TestOracleRef_MatchesStoreNaming keeps the pipeline's ref builder and
// the store's path builder from drifting apart: the LSP looks entries
// up through the same pair, and a silent divergence would make every
// editor lookup a miss.
func TestOracleRef_MatchesStoreNaming(t *testing.T) {
	ref := cache.OracleRef{Target: "app/signin/gen", Query: "SearchUsers", Shape: "maximal"}
	if got := cache.OracleFileName(ref); got != "oracle/app/signin/gen/SearchUsers/maximal.json" {
		t.Fatalf("OracleFileName = %q", got)
	}
}
