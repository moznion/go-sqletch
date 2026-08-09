# sqletch Design — 17: Runtime metrics for generated executors

**Status: ACCEPTED — decisions D1–D8 settled 2026-08-09 (all
recommendations adopted).** This document designs an observability
mechanism for the SQL-executing layer: the generated `Queries`
packages and the `runtime.ComposedCache` they drive. The goal is that
an operator of a service built on sqletch-generated code can answer,
from a metrics endpoint: how many shapes each query can reach, how
many it actually used, how well the composed-SQL cache performs, how
much memory it retains, and which shapes dominate traffic.

## 1. What this buys — the benefit case

sqletch's runtime story is "deterministic composition of pre-verified
fragments, memoized per shape". Every operational question about that
story is currently unanswerable in production:

1. **Cache sizing is blind.** `New(db)` hard-codes a 256-entry
   `ComposedCache`. Whether that thrashes (shape space larger than
   capacity, evictions churning) or is 100× oversized is invisible —
   hit rate and eviction counters are the two numbers that decide it.
2. **Shape-space growth is invisible until it hurts.** On expanding
   dialects every distinct `@in` arity is a distinct shape and — one
   layer down — a distinct prepared statement per connection. A
   caller iterating arities 1..N inflates both silently. The same
   applies to `@filter-tree` structural variety.
3. **Verified-but-dead shapes cannot be found.** The compiler proves
   every reachable shape sound; it cannot say which shapes traffic
   actually exercises. A used/reachable coverage signal turns "can we
   delete this @choose case?" from archaeology into a dashboard read.
4. **Runtime rejections are silent.** `ErrTreeTooLarge`,
   `ErrChooseRequired`, `ErrOrderKey`, `ErrFilterRequired` and friends
   return to the caller and vanish. A spike in cap rejections is a
   misuse — or abuse — signal worth alerting on.
5. **The existing `OnQuery` hook is a debugging tap, not a metrics
   surface**: it fires pre-execution, carries no query name, no
   hit/miss bit, no duration, no error, and its `key.String()`
   argument allocates per call.

Honest limits, stated up front:

- **Prepared-statement counts are not sqletch's to report.** sqletch
  hands SQL text to pgx / database/sql; preparation and its caching
  happen per-connection inside the driver (and the server keeps the
  authoritative number: `pg_prepared_statements`, MySQL's
  `Prepared_stmt_count`). What sqletch *can* report is the driver-side
  upper-bound driver of that number: distinct composed SQL texts
  (cumulative cache insertions, current residents). The manual chapter
  must spell out how to join these with `pgxpool.Stat` / server
  status variables rather than pretend to own the metric.
- **"Shapes used" is exactly countable only while it is small.** With
  `@filter-tree` or expanding `@in` the shape space is unbounded, so
  any exact distinct-set tracking must be bounded and must say when
  it saturated (§6, D5). Silent truncation would read as "covered
  everything" — the existing no-silent-caps discipline applies.

## 2. Position in the architecture — additive, dependency-neutral

```
runtime/            observation surface: Observer interface, cache
                    counters, Stats()/TopShapes() snapshots   (NEW, D1)
internal/codegen    generated deltas: SetObserver wiring, per-query
                    shape-space registry, exec-side observation (NEW)
contrib/otel        OTel adapter, its own go.mod               (NEW, D2)
```

Hard constraints, all existing invariants:

- **`runtime/` gains no dependencies.** It is the public package every
  consumer imports; an OTel (or Prometheus) import there would force
  the dependency graph on all generated code. The core exposes a
  neutral interface + poll-able snapshots; format-specific export
  lives in an adapter module with its own `go.mod` (the same isolation
  the doc 16 analyzer uses for `x/tools`).
- **The lock-free hit path stays lock-free and allocation-free.** The
  `ComposedCache` read path deliberately serves hits from an atomic
  snapshot with no mutex and no allocation, and `touch()` deliberately
  load-guards its store to avoid cross-core cache-line traffic. The
  metrics design must not reintroduce what that code exists to avoid
  (D4); `runtime/bench_test.go` is the referee.
- **Compose conformance is untouched.** Nothing here changes
  composition, shape keys, bind plans, or emitted SQL.
  `TestComposeConformance` must pass unmodified.
- **Determinism**: every snapshot the surface returns is sorted
  (`TopShapes` by count desc, then map key asc) — byte-identical
  output for identical state, per the repo-wide rule.
- **v1 API compatibility**: all additions to `runtime/` and to
  generated code are additive. `OnQuery` keeps its exact signature and
  semantics.

## 3. Format decision: OpenTelemetry API, OpenMetrics as an exposition

OpenTelemetry and OpenMetrics are not alternatives; they are layers.
OpenMetrics is a wire/exposition format (the standardized Prometheus
text format) — and as a standalone spec it was archived by the CNCF
in 2024 and folded back into Prometheus. OpenTelemetry is an
instrumentation API whose Prometheus exporter *serves* that format.

Decision (D2): instrument against the **OTel metrics API** in the
adapter module. Consumers who want a Prometheus/OpenMetrics scrape
endpoint attach the OTel Prometheus exporter; consumers on an OTLP
pipeline push. Consumers who want neither depend only on `runtime/`
and read the neutral snapshots themselves. Choosing hand-rolled
OpenMetrics text instead would cost us histograms-with-exemplars,
OTLP, and the trace-attribute escape hatch for high-cardinality data
(§7) — and would still need an HTTP server story the adapter gets for
free.

## 4. Observation surface in `runtime/`

```go
// Observer receives runtime events from the composed-SQL cache and
// from generated code. All methods must be safe for concurrent use.
// A nil observer costs one predictable branch per event site.
type Observer interface {
	// ObserveCompose fires once per cache access: hit=true for a
	// served memoized entry, false when composition ran. The key is
	// valid only for the duration of the call; implementations that
	// retain it must copy (Trees/Orders share backing arrays).
	ObserveCompose(query string, key ShapeKey, hit bool)

	// ObserveExec fires after a database call completes: duration,
	// rows (returned for queries, affected for execs; -1 when the
	// count was unknown), and the driver error if any. It takes the
	// key's canonical ENCODING, not a ShapeKey: generated code builds
	// its key on the stack, and passing that value through an
	// interface call would heap-allocate the key's slices on every
	// call, observed or not — the string is built inside the observer
	// guard, so only observed calls pay (implementation finding,
	// 2026-08-09; ObserveCompose can pass the cache's retained key
	// for free, which is why the two signatures differ).
	ObserveExec(query, shapeKey string, d time.Duration, rows int64, err error)

	// ObserveReject fires when a call is refused before any SQL is
	// sent: ErrChooseRequired, ErrOrderKey, ErrFilterRequired,
	// ErrTreeTooLarge, ErrTreePredicate, ErrShapeKeyLimit. Classify
	// with errors.Is, never by string.
	ObserveReject(query string, err error)
}

// SetObserver installs an observer on the cache (compose events).
// Must be called before the cache serves traffic; installing an
// observer mid-flight is not synchronized with in-progress reads.
func (c *ComposedCache) SetObserver(o Observer)

// CacheStats is a point-in-time snapshot, taken under the cache
// mutex — a scrape-time cost, never a hot-path one.
type CacheStats struct {
	Hits, Misses uint64 // cumulative; hit rate = Hits/(Hits+Misses)
	Inserts      uint64 // cumulative entries created (distinct-SQL proxy)
	Evictions    uint64 // cumulative second-chance evictions
	Entries      int    // current resident entries
	Capacity     int
	SQLBytes     int64  // Σ len(sql) over resident entries; the fast
	                    // snapshot may briefly retain up to ~2× this
	                    // (documented lag of the lock-free read path)
}
func (c *ComposedCache) Stats() CacheStats

// ShapeUse is one entry of the per-shape usage ranking.
type ShapeUse struct {
	Query    string
	Key      string // canonical ShapeKey encoding
	Hits     uint64 // hits on the resident entry (resets on eviction)
	SQLBytes int
}
// TopShapes returns the n most-hit resident shapes, ordered by hits
// descending then key ascending — deterministic for equal state.
func (c *ComposedCache) TopShapes(n int) []ShapeUse
```

Implementation notes, pinned here because they carry the performance
argument:

- **Counter placement (D4).** `Misses`, `Inserts`, `Evictions`,
  `SQLBytes` mutate only under the existing mutex — plain fields,
  zero new contention. `Hits` is the hot one: it is counted on the
  entry (`cacheEntry.hits atomic.Uint64`, incremented next to
  `touch()`), and `Stats()` reports a cache-level accumulator plus
  the residents' sum; `remove()` folds an evicted entry's count into
  the accumulator under the mutex so cumulative hits survive
  eviction (hits racing a fold can drop — an accepted skew on a
  rates-and-ratios counter). Per-entry placement means cores contend
  only when hammering the *same shape*. **Measured outcome
  (2026-08-09, M4 Pro)**: the ungated increment cost the parallel
  hit path ~2.5× (12.8→34 ns/op), so the escalation clause fired:
  counting gates behind a sticky `track atomic.Bool` set by the
  first `SetObserver`/`Stats`/`TopShapes` call. Unobserved traffic
  pays one shared load (12.0–12.3 ns/op, at baseline); with metrics
  enabled the same worst-case workload runs ~130 ns/op
  (`BenchmarkGeneratedCallParallelObserved` pins both). `Stats`
  documents that hit counting starts at first observation — delta
  readers lose nothing past the first scrape interval.
- **`ObserveCompose` receives the entry's retained key, never the
  caller's.** An interface call's arguments escape; threading the
  caller's key through one heap-allocates every generated call
  site's key slices (measured: +1 alloc/op on the hit path).
  `keysEqual` has already proven entry key and caller key identical,
  so the substitution is observationally invisible. The `nil` check
  is the only cost when unused; the observer is a plain field set
  before traffic per the contract above (mirrors `OnQuery`).
- **`ShapeKey` retention hazard** is real (slices share backing
  arrays with the cache entry); the interface comment carries the
  copy rule, and the adapter (§6) only reads scalars/encodes.

## 5. Generated-code deltas

Per-package (`db.gen.go`):

```go
// SetObserver installs a runtime observer receiving compose, exec,
// and reject events for every query on this Queries value (and any
// WithTx derivative — the observer travels with the shared cache).
func (q *Queries) SetObserver(o runtime.Observer) {
	q.obs = o
	q.cache.SetObserver(o)
}

// Cache exposes the composed-SQL cache for scrape-time Stats and
// TopShapes — the wiring an exporter needs, without opening the
// field itself.
func (q *Queries) Cache() *runtime.ComposedCache { return q.cache }
```

Per query method, around the existing call:

```go
sqlText, argIdx := q.cache.Get("SearchUsers", searchUsersFrags, key)
// ... existing arg build + OnQuery hook, unchanged ...
start := time.Time{}
if q.obs != nil { start = time.Now() }
rows, err := q.db.Query(ctx, sqlText, args...)
// ... existing scan loop counts n ...
if q.obs != nil { q.obs.ObserveExec("SearchUsers", key, time.Since(start), n, err) }
```

and every early-return validation branch (`ChooseOrdinal`,
`OrderSeq`, required-tree, caps) reports `ObserveReject` before
returning. `:one` reports rows 1; `:exec`/`:execrows` report
`RowsAffected` (the database/sql flavor consults it only under the
observer guard — some drivers make it a round-trip); any errored call
reports rows -1. A @filter-tree query's exec event routes through a
generated `observeExecTree` helper that folds the `;t=` key segment
in under the observer guard — the call site never re-derives it,
exactly the hookTree discipline.

**Statically expanded queries** (`runtime.Lookup` path) have no cache
and no composition cost: they emit `ObserveExec`/`ObserveReject` only.
The adapter documents that their compose series are legitimately
absent, not missing.

**Shape-space registry** — the generate-time side of coverage
(D6). `internal/shape.Count` already computes the reachable count as
a `*big.Int` (order-permutation spaces overflow int64 by design);
codegen emits, per package:

```go
// ShapeSpace describes each query's reachable shape space, computed
// at generate time. Enumerable is saturated at MaxUint64 when the
// exact count exceeds it (Exact=false); Unbounded marks dimensions
// the count deliberately quotients out (@filter-tree structure; @in
// arity on expanding dialects), under which "coverage" is a floor,
// never a ratio.
var ShapeSpace = map[string]runtime.ShapeSpaceInfo{
	"SearchUsers": {Enumerable: 64, Exact: true, Unbounded: false},
	...
}
```

with `runtime.ShapeSpaceInfo` as the shared struct. Keys are emitted
in sorted query-name order (determinism). The registry is what lets
an exporter pre-register per-query label values and compute
used-vs-reachable coverage without parsing anything.

## 6. The adapter module: `contrib/otel`

A separate Go module (working name
`github.com/moznion/go-sqletch/contrib/otel`, package `otelsqletch`),
so the OTel dependency graph never touches `runtime/` or consumers
who skip it. Surface:

```go
// New returns an Observer recording to the given MeterProvider, plus
// a Registration whose Collect callbacks poll cache.Stats() and
// TopShapes() at scrape time.
func New(mp metric.MeterProvider, opts ...Option) *Observer
func (o *Observer) Bind(name string, q interface{ SetObserver(runtime.Observer) }, c *runtime.ComposedCache, space map[string]runtime.ShapeSpaceInfo)
```

(`Bind`'s exact shape is implementation detail; the doc-level point is
one call wires a generated `Queries` + its cache + its `ShapeSpace`
into the meter, with an `instance` label per bind — `New(db)` creates
a cache per `Queries`, so multi-instance apps either bind each or
share one `Queries`.)

Instruments (OTel names; the Prometheus exporter renders the usual
`sqletch_*_total` forms). `query` is the only per-event label — it is
bounded by the generated package. `error` labels use the sentinel
name resolved via `errors.Is`, else the driver error's coarse class.

| Instrument | Kind | Labels | Source |
| --- | --- | --- | --- |
| `sqletch.compose.calls` | counter | query, hit | ObserveCompose |
| `sqletch.exec.duration` | histogram | query, status | ObserveExec |
| `sqletch.exec.rows` | histogram | query | ObserveExec |
| `sqletch.reject.count` | counter | query, error | ObserveReject |
| `sqletch.cache.hits` / `.misses` / `.inserts` / `.evictions` | counter (async) | instance | Stats() |
| `sqletch.cache.entries` / `.capacity` / `.sql_bytes` | gauge (async) | instance | Stats() |
| `sqletch.shapes.used` | gauge (async) | query, saturated | adapter set (D5) |
| `sqletch.shapes.reachable` | gauge (async) | query, exact, unbounded | ShapeSpace |
| `sqletch.shapes.top` | gauge (async) | query, key, rank | TopShapes(N) |

- **Used-shape distinct tracking lives in the adapter** (D5): it sees
  `(query, key)` on every `ObserveCompose` and maintains a bounded
  per-query set of canonical encodings (default bound 4096, matching
  the verification budget's default); at the bound the set stops
  growing and the `saturated` label flips — the count becomes a
  floor, loudly. Core `runtime/` stays free of unbounded state.
- **`sqletch.shapes.top` is the one deliberate exception** to the
  no-shape-key-labels rule: it is bounded by construction (N ≤ 20,
  default 10), refreshed per scrape from `TopShapes`, and exists
  precisely to expose the dominant keys without opening the
  cardinality door on the per-event series.
- The adapter also ships a `TraceObserver` decorator that attaches
  `sqletch.shape_key` / `sqletch.cache_hit` as span attributes on the
  ambient trace span — the correct home for full-cardinality shape
  data (§7).

## 7. Cardinality policy

Pinned as a rule, because it is the mistake this design most invites:

- **The shape key never becomes a per-event metric label.** Its `;t=`
  (tree structure) and `;n=` (arity) segments are caller-controlled
  and unbounded; a label would let any request mint a time series
  (a memory DoS on the metrics pipeline — the same class of concern
  the tree caps address in the composer).
- Bounded labels only: query name (generated set), sentinel error
  name (closed set), hit/status bits, rank (≤ N).
- Full-cardinality shape detail travels on **traces/logs** (span
  attributes via the decorator, or the user's own `ObserveCompose`),
  or in the **bounded top-N snapshot**.

## 8. Decisions (settled 2026-08-09 — recommendations adopted as written)

### D1 — Instrumentation home: neutral core surface + adapter module

- (a) OTel API directly in `runtime/`.
- (b) **Neutral `Observer`/`Stats` in `runtime/`; OTel confined to
  `contrib/otel` with its own `go.mod`.**

**Recommendation: (b).** (a) taxes every consumer of generated code
with the OTel graph and freezes us to one ecosystem; (b) costs one
small interface and keeps `runtime/`'s dependency story (none) intact.

### D2 — Export format: OTel API, OpenMetrics via exporter

- (a) Hand-rolled OpenMetrics/Prometheus text endpoint.
- (b) **OTel metrics API; Prometheus/OpenMetrics via the standard
  exporter, OTLP for push pipelines.**

**Recommendation: (b)** — §3's argument: OpenMetrics is an exposition
format, not an API; standalone it is archived; (b) yields it as a
subset while keeping histograms, exemplars, and the trace escape
hatch.

### D3 — Hook evolution: new Observer, `OnQuery` untouched

- (a) Widen `OnQuery` (breaking) or overload it with variants.
- (b) **Add `SetObserver`; `OnQuery` keeps its signature, semantics,
  and callers.**

**Recommendation: (b).** v1 API compatibility is contractual
(`runtime` doc header); `OnQuery` remains the simple debugging tap,
and the two do not interact.

### D4 — Hit counting without poisoning the lock-free path

- (a) Global `atomic.Uint64` incremented on every hit.
- (b) **Per-entry counter + mutex-side fold-in on eviction; other
  counters under the existing mutex.**
- (c) Sharded/padded global counters.

**Recommendation: (b).** (a) makes one cache line exclusive-owned by
every core on every query — precisely the traffic `touch()`'s load
guard exists to avoid. (b) contends only where the workload already
shares a line. (c) is the escalation if benchmarks demand it; do not
start there. Gate: `BenchmarkCacheHit`-parallel must show no
significant regression with observer nil. *Outcome: the gate fired —
even the per-entry increment regressed the parallel hit path ~2.5×,
so counting is additionally gated behind first observability use
(§4); with that, the unobserved path measures at baseline.*

### D5 — "Shapes actually used": bounded exact set in the adapter

- (a) Exact distinct-set tracking in `ComposedCache` (unbounded).
- (b) Cumulative `Inserts` as the only proxy.
- (c) **Bounded exact per-query set in the adapter (default 4096),
  explicit `saturated` signal; `Inserts` kept as the distinct-SQL
  proxy; approximate counting (HLL) deferred until someone saturates
  in practice.**

**Recommendation: (c).** (a) puts caller-controlled unbounded memory
in the core — the exact thing the tree caps exist to prevent; (b)
overcounts under eviction churn and answers a different question.
(c) is honest about its bound, and the bound is generous: beyond
thousands of distinct shapes the number stops being readable anyway.

### D6 — Reachable-count representation in generated code

- (a) Emit `shape.Count`'s `big.Int` as a decimal string.
- (b) **`Enumerable uint64` saturating at MaxUint64 + `Exact bool` +
  `Unbounded bool`, in a per-package `ShapeSpace` registry.**

**Recommendation: (b).** Metrics consumers need a number, not a
bignum; a saturated uint64 with an explicit flag loses nothing a
dashboard could render. The string form can always be added to
`sqletch explain` (which already owns human-facing detail).

### D7 — Exec observation from generated code vs driver tracers

- (a) Leave duration/rows/errors entirely to pgx tracers /
  driver-level instrumentation.
- (b) **Observe in generated code, labeled by query name and carrying
  the shape key.**

**Recommendation: (b).** The driver sees SQL text; only sqletch knows
query identity and shape. (a) forces operators to reverse-map SQL
strings to queries and cannot label by shape at all. The two
compose: pgx tracer metrics remain the right place for pool/connection
health, and the manual says so.

### D8 — Scope of v1: metrics only, no config surface

- (a) Add `sqletch.yaml` knobs now (cache capacity, top-N, bounds).
- (b) **No config changes: the surface is programmatic (options on
  the adapter), cache capacity stays as-is; revisit capacity
  configurability as its own decision once eviction metrics exist to
  justify it.**

**Recommendation: (b).** Every knob added before the metric that
motivates it exists is speculative API; the whole point of shipping
eviction/hit-rate counters is to learn whether 256 is ever wrong. No
new diagnostics codes are needed under (b).

## 9. Relationship to other mechanisms

- **`OnQuery`** — unchanged; remains the zero-setup debugging tap
  (and the devdb E2E assertion hook). The observer supersedes it for
  production metrics but does not replace it.
- **Doc 16 scoped executors** — the scoped layer calls the ordinary
  generated methods, so every scoped call is observed with no extra
  work. When doc 16 lands, its middleware gains the natural
  companion counters (extraction denials per scope, scoped vs. raw
  call counts) as a doc 16 §8 extension — deliberately out of scope
  here.
- **Tree caps / `@in` bounds** — `ObserveReject` is their
  observability half: caps make oversized inputs fail; the reject
  counter makes the failing visible and alertable.
- **Static expansion** — expanded queries bypass the cache by design;
  their absence from compose metrics is documented, not patched over.

## 10. Testing plan

Per the working conventions — test-first, every layer:

- **`runtime/`**: Stats/TopShapes correctness under a scripted
  hit/miss/evict sequence (hits survive eviction via the fold-in;
  SQLBytes tracks insert/remove exactly); TopShapes ordering
  determinism (equal counts → key order); observer receives
  compose/reject events with correct hit bits, including the raced
  double-compose path (`cache_concurrency_test.go` gains observer
  assertions — events may exceed inserts, never undercount);
  nil-observer paths allocation-free (`testing.AllocsPerRun`);
  benchmark guard for D4.
- **Conformance**: `TestComposeConformance` passes unmodified —
  asserted, because §2 claims it.
- **Codegen golden tests**: `SetObserver`, exec/reject observation
  sites, and the `ShapeSpace` registry for fixture packages, all
  three dialect flavors; sorted-order determinism; a saturating
  `ShapeSpace.Enumerable` fixture (large @order-by permutation
  space).
- **Generated-module behavior** (compiled fixture): a recording
  observer sees compose(miss→hit) on repeat calls, exec rows/status
  for :many/:one/:exec, rejects for each sentinel; `WithTx` carries
  the observer.
- **`contrib/otel`**: unit tests against the OTel SDK's manual
  reader — instrument names/labels as tabled in §6, saturation flip
  of the used-shapes set, top-N bounded, no shape-key label on any
  per-event series (asserted negatively, it is the §7 rule).
- **E2E (devdb, all three dialects)**: the generated-module run
  additionally binds the adapter and asserts hit-rate > 0 after the
  warm pass and that `sqletch.shapes.used` ≤ resident+bound; the
  existing OnQuery-based assertions stay.
- **Docs**: manual chapter "Runtime metrics" (instrument table, the
  prepared-statement join recipe with pgxpool/server counters, the
  cardinality policy); `docs/manual` diagnostics chapter untouched
  (no new codes).

## 11. Phasing

1. `runtime/` surface: Observer, cache counters, Stats/TopShapes,
   benches (D4 gate). Useful alone: consumers can hand-wire expvar.
2. Codegen: SetObserver wiring, exec/reject sites, ShapeSpace
   registry; golden + behavioral tests.
3. `contrib/otel` module + manual chapter.
4. Deferred: HLL escalation for D5 saturation, cache-capacity config
   (D8 revisit), doc 16 scope-layer counters (after doc 16), trace
   exemplars on the exec histogram.
