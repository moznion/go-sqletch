package cli

import (
	"path/filepath"
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

// outsideProject reports whether a configured write path leaves the
// project directory. sqletch sweeps stale files out of directories it
// owns, and a committed config does not get to point that sweep at the
// rest of the filesystem: both callers (the cache sweep and the
// *.gen.go sweep) refuse with SQLETCH306 instead.
func outsideProject(cfg config.Config, configured string) bool {
	rel, err := filepath.Rel(cfg.Dir, cfg.Abs(configured))
	return filepath.IsAbs(configured) || err != nil ||
		rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// pruneCache removes the cache entries that the run did not write or
// hit — entries for a deleted query, a renamed target, a superseded
// schema, or the pre-v2 hash-named layout.
//
// Only `generate` calls it (doc 21 D3), mirroring removeStaleGenerated:
// `check` fills misses but must never mutate the tree it is checking,
// and the LSP never writes at all. What to delete is the store's own
// business (cache.Store.Sweep); what this adds is the policy — a cache
// directory outside the project is not swept at all, because deleting
// files outside the repository sqletch was pointed at is not something
// a committed config gets to ask for.
func pruneCache(cfg config.Config, live map[cache.OracleRef]bool) ([]diagnostics.Diagnostic, error) {
	if outsideProject(cfg, cfg.Cache.Path) {
		return []diagnostics.Diagnostic{diagnostics.Warnf(diagnostics.CodePathEscape,
			diagnostics.Span{File: cfg.Path},
			"cache directory %q is outside the project directory, so superseded cache entries there are not removed", cfg.Cache.Path).
			WithHint("move the cache inside the project so sqletch can keep it consistent, or delete obsolete entries yourself")}, nil
	}
	return nil, cache.NewStore(cfg.Abs(cfg.Cache.Path)).Sweep(live)
}
