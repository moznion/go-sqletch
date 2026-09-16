package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// oracleRef names one rendering's committed cache entry
// (docs/design/21-cache-layout.md §3). The target slug is the same
// spelling the derived-output trees use (`.sqletch/explain/<slug>/`),
// and target-plus-query is exactly the compiler's uniqueness scope
// (SQLETCH004: a query name is unique per target).
//
// This is the single place the pipeline and the LSP agree on where an
// entry lives; like cli.scanChecks and cli.resolvedChecks, it is shared
// rather than forked, so the editor can never look somewhere the
// compiler does not write.
func oracleRef(slug, query string, r ast.Rendering) cache.OracleRef {
	return cache.OracleRef{Target: slug, Query: query, Shape: r.Shape}
}

// pruneCache removes the cache files that the run did not write or
// hit — entries for a deleted query, a renamed target, a superseded
// schema, or the pre-v2 hash-named layout.
//
// Only `generate` calls it (doc 21 D3), mirroring removeStaleGenerated:
// `check` fills misses but must never mutate the tree it is checking,
// and the LSP never writes at all. live holds store-relative,
// slash-separated paths.
//
// It deletes only files sqletch itself writes — the same discipline
// removeStaleGenerated applies to `*.gen.go`. That is entries under
// `oracle/`, plus the singleton `catalog.json`/`env.json` and their
// fingerprint-named v1 predecessors at the cache root. A `.gitignore`,
// a README, a sibling tool's data file, and every other subdirectory
// survive; so does a planted symlink, which is left alone rather than
// followed. It matters because `cache.path` is config: pointed at a
// directory full of somebody else's files, a broader sweep would eat
// them.
//
// A cache directory outside the project is not swept at all — deleting
// files outside the repository sqletch was pointed at is not something
// a committed config gets to ask for (the same refusal
// removeStaleGenerated gives an out-of-project output directory).
func pruneCache(cfg config.Config, live map[string]bool) ([]diagnostics.Diagnostic, error) {
	dir := cfg.Abs(cfg.Cache.Path)
	rel, relErr := filepath.Rel(cfg.Dir, dir)
	outside := relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
	if filepath.IsAbs(cfg.Cache.Path) || outside {
		return []diagnostics.Diagnostic{diagnostics.Warnf(diagnostics.CodePathEscape,
			diagnostics.Span{File: cfg.Path},
			"cache directory %q is outside the project directory, so superseded cache entries there are not removed", cfg.Cache.Path).
			WithHint("move the cache inside the project so sqletch can keep it consistent, or delete obsolete entries yourself")}, nil
	}

	// The cache root: the two singletons and whatever an older layout
	// left beside them. Not recursive — a sibling subdirectory in there
	// is not sqletch's.
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // nothing generated yet: nothing to prune
	}
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if e.IsDir() || !e.Type().IsRegular() || live[e.Name()] || !prunableRootFile(e.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return nil, err
		}
	}

	// The entry tree.
	oracleRoot := filepath.Join(dir, oracleDirName)
	var dirs []string
	err = filepath.WalkDir(oracleRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != oracleRoot {
				dirs = append(dirs, p)
			}
			return nil
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		r, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if live[filepath.ToSlash(r)] {
			return nil
		}
		return os.Remove(p)
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	// Deepest first, so a renamed target or query leaves no husk. A
	// directory that still holds something fails to remove, which is
	// exactly the intended outcome.
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		_ = os.Remove(d)
	}
	return nil, nil
}

// oracleDirName mirrors the store's entry subdirectory. The sweep has
// to name it independently of cache.OracleFileName because it works
// from paths on disk rather than from refs.
const oracleDirName = "oracle"

// prunableRootFile reports whether a cache-root file is one sqletch
// writes there: the current singletons, or the fingerprint-named
// catalog/env files of the v1 layout.
func prunableRootFile(name string) bool {
	if name == cache.CatalogFile || name == cache.EnvFile {
		return true
	}
	return (strings.HasPrefix(name, "catalog-") || strings.HasPrefix(name, "env-")) &&
		strings.HasSuffix(name, ".json")
}
