package runtime

import (
	"context"
	"sort"
	"time"
)

// Observer receives runtime events from the composed-SQL cache and
// from generated code (design doc 18). Implementations must be safe
// for concurrent use and must return quickly — events fire on the
// query path. A nil observer costs one predictable branch per event
// site; format-specific export (OpenTelemetry, Prometheus) belongs in
// adapter modules, never here.
type Observer interface {
	// ObserveCompose fires once per cache access: hit reports whether
	// a memoized entry was served (true) or composition ran (false).
	// Under a race, two compositions of one shape may both report
	// hit=false while a single entry is inserted — compose events can
	// exceed insertions, never undercount work. The key's slices are
	// shared with the caller: copy them before retaining the key.
	ObserveCompose(query string, key ShapeKey, hit bool)

	// ObserveExec fires from generated code after a database call
	// completes: the call's context (for trace correlation and metric
	// exemplars), its duration, its row count (rows returned for
	// queries, rows affected for execs; -1 when the count was unknown
	// — a driver error or an aborted scan), and the driver error if
	// any. It receives the shape key as its canonical encoding, not a
	// ShapeKey: generated code builds its key on the stack, and
	// passing that through an interface call would heap-allocate the
	// key's slices on every call, observed or not — the encoding is
	// built inside the observer guard instead, so only observed calls
	// pay for it. (ObserveCompose can pass the cache's retained key
	// for free, which is why the two differ. ObserveCompose carries no
	// context because the cache API takes none.)
	ObserveExec(ctx context.Context, query, shapeKey string, d time.Duration, rows int64, err error)

	// ObserveReject fires from generated code when a call is refused
	// before any SQL is sent: [ErrChooseRequired], [ErrOrderKey],
	// [ErrFilterRequired], [ErrTreeTooLarge], [ErrTreePredicate],
	// [ErrShapeKeyLimit]. Classify with errors.Is, never by message.
	ObserveReject(ctx context.Context, query string, err error)
}

// SetObserver installs an observer receiving one ObserveCompose per
// cache access. It must be called before the cache serves traffic:
// installation is not synchronized with in-flight reads (the same
// contract as the generated OnQuery hook). It also enables hit
// counting (see Stats).
func (c *ComposedCache) SetObserver(o Observer) {
	c.obs = o
	c.track.Store(true)
}

// CacheStats is a point-in-time snapshot of the cache's counters,
// taken under the cache mutex — a scrape-time cost, never a hot-path
// one. Counters are cumulative since the cache was created.
type CacheStats struct {
	// Hits counts accesses served from a memoized entry. Summed from
	// per-entry counters plus the folded counts of evicted entries, so
	// eviction never loses history; hit rate = Hits/(Hits+Misses).
	//
	// Hit counting starts at the first SetObserver, Stats, or
	// TopShapes call (sticky): a cache that is never observed pays
	// nothing for the counter on its lock-free hit path. Scrape-based
	// consumers read deltas between scrapes, which the warm-up does
	// not distort past the very first interval.
	Hits uint64
	// Misses counts compositions performed, including raced duplicates
	// whose entry was discarded (the work happened).
	Misses uint64
	// Inserts counts entries created — the driver-side proxy for
	// distinct SQL texts handed to the driver (each is what a
	// per-connection statement cache would prepare).
	Inserts uint64
	// Evictions counts second-chance evictions. A nonzero rate on a
	// steady workload means the shape set outgrows Capacity.
	Evictions uint64
	Entries   int // resident entries
	Capacity  int
	// SQLBytes is Σ len(sql) over resident entries. The lock-free read
	// path's lagging snapshot may briefly retain up to about twice
	// this (see the ComposedCache.fast comment).
	SQLBytes int64
}

// Stats returns cumulative counters and current occupancy. It takes
// the cache mutex and walks resident entries: call it at scrape time,
// not on the query path.
func (c *ComposedCache) Stats() CacheStats {
	c.track.Store(true)
	c.mu.Lock()
	defer c.mu.Unlock()
	s := CacheStats{
		Hits:      c.hitsFolded,
		Misses:    c.misses,
		Inserts:   c.inserts,
		Evictions: c.evictions,
		Entries:   len(c.m),
		Capacity:  c.cap,
		SQLBytes:  c.sqlBytes,
	}
	for _, e := range c.m {
		s.Hits += e.hits.Load()
	}
	return s
}

// ShapeUse is one row of the per-shape usage ranking.
type ShapeUse struct {
	Query string
	Key   string // canonical ShapeKey encoding
	// Hits counts accesses served by the resident entry; it resets if
	// the shape is evicted and later recomposed.
	Hits     uint64
	SQLBytes int // len of the composed SQL
}

// TopShapes returns the n most-used resident shapes, ordered by hits
// descending, then query, key, and SQL length ascending — a total
// order, so equal state yields byte-identical output. Like Stats, it
// is a scrape-time call.
func (c *ComposedCache) TopShapes(n int) []ShapeUse {
	c.track.Store(true)
	if n <= 0 {
		return nil
	}
	c.mu.Lock()
	all := make([]ShapeUse, 0, len(c.m))
	for _, e := range c.m {
		all = append(all, ShapeUse{
			Query:    e.query,
			Key:      e.key.String(),
			Hits:     e.hits.Load(),
			SQLBytes: len(e.sql),
		})
	}
	c.mu.Unlock()
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.Hits != b.Hits {
			return a.Hits > b.Hits
		}
		if a.Query != b.Query {
			return a.Query < b.Query
		}
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		return a.SQLBytes < b.SQLBytes
	})
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// ShapeSpaceInfo describes one query's reachable shape space, computed
// at generate time and emitted into the generated package's ShapeSpace
// registry. It lets an exporter pre-register per-query series and
// compute used-vs-reachable coverage without parsing anything.
type ShapeSpaceInfo struct {
	// Enumerable counts the reachable shapes of the enumerable
	// dimensions (guard sets × @choose ordinals × @order-by
	// selections), saturating at MaxUint64.
	Enumerable uint64
	// Exact is false when the true count exceeded Enumerable's range
	// (large @order-by permutation spaces).
	Exact bool
	// Unbounded marks dimensions the count deliberately excludes —
	// @filter-tree structure, and @in arity on expanding dialects —
	// under which used/reachable coverage is a floor, never a ratio.
	Unbounded bool
}
