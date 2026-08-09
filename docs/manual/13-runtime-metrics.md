# Runtime metrics

Generated executors can report what they do in production: how well
the composed-SQL cache performs, which query shapes traffic actually
exercises, how long calls take, and what gets refused before any SQL
is sent. The design (and its settled decisions) is
[design doc 17](../design/17-runtime-metrics.md).

Two layers exist so the core stays dependency-free:

1. **The neutral surface** — `runtime.Observer` (an interface your
   code or an adapter implements) plus poll-able snapshots on the
   cache: `Stats()`, `TopShapes(n)`. No imports beyond the runtime.
2. **The OpenTelemetry adapter** —
   `github.com/moznion/go-sqletch/contrib/otel` (package
   `otelsqletch`), a separate Go module. OpenMetrics/Prometheus
   exposition comes from the standard OTel Prometheus exporter; OTLP
   push pipelines work unchanged.

## Quick start (OpenTelemetry + Prometheus scrape)

```go
import (
    "net/http"

    "github.com/prometheus/client_golang/prometheus/promhttp"
    otelprom "go.opentelemetry.io/otel/exporters/prometheus"
    sdkmetric "go.opentelemetry.io/otel/sdk/metric"

    otelsqletch "github.com/moznion/go-sqletch/contrib/otel"
)

exporter, _ := otelprom.New()
mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))

metrics, _ := otelsqletch.New(mp)
q := gen.New(pool)
binding, _ := metrics.Bind("main", q, gen.ShapeSpace)
defer binding.Close()

http.Handle("/metrics", promhttp.Handler())
```

`Bind` installs the observer on the `Queries` value (it travels
through `WithTx`), registers scrape-time callbacks over its cache,
and takes the generated package's `ShapeSpace` registry for
used-vs-reachable coverage. One `Bind` per `gen.New(...)` instance,
each with its own `instance` label.

To attach the full shape key to trace spans, decorate:

```go
q.SetObserver(otelsqletch.TraceObserver{Next: binding})
```

## Without OpenTelemetry

`Stats` and `TopShapes` are plain method calls — wire them into
expvar, a debug endpoint, or any metrics library:

```go
s := q.Cache().Stats()
// s.Hits, s.Misses, s.Inserts, s.Evictions, s.Entries, s.Capacity, s.SQLBytes
for _, u := range q.Cache().TopShapes(10) {
    // u.Query, u.Key, u.Hits, u.SQLBytes
}
```

Implement `runtime.Observer` yourself for per-event data (compose
hit/miss, exec duration/rows/error, rejects). Install observers
before serving traffic; a `Queries` value with no observer pays
nothing on its hot path — hit counting itself starts at the first
`SetObserver`/`Stats`/`TopShapes` call, so the very first scrape
interval undercounts hits and every later delta is exact.

## Instruments (adapter)

| Instrument | Kind | Labels | Meaning |
| --- | --- | --- | --- |
| `sqletch.compose.calls` | counter | instance, query, hit | Cache accesses; hit=false means composition ran |
| `sqletch.exec.duration` | histogram (s) | instance, query, status | Database call latency |
| `sqletch.exec.rows` | histogram | instance, query | Rows returned/affected (unknown counts excluded) |
| `sqletch.reject.count` | counter | instance, query, error | Calls refused before any SQL was sent, by sentinel |
| `sqletch.cache.hits` / `.misses` / `.inserts` / `.evictions` | counter (async) | instance | Cumulative cache counters |
| `sqletch.cache.entries` / `.capacity` / `.sql_bytes` | gauge (async) | instance | Occupancy and retained SQL bytes |
| `sqletch.shapes.used` | gauge (async) | instance, query, saturated | Distinct shapes composed since start (exact up to a bound) |
| `sqletch.shapes.reachable` | gauge (async) | instance, query, exact, unbounded | Statically verified shape count (generate-time constant) |
| `sqletch.shapes.top` | gauge (async) | instance, query, key, rank | Hit counts of the top-N resident shapes |

Reading them:

- **Hit rate** = `hits / (hits + misses)`. A low rate *with* steady
  `evictions` means the shape set outgrows the cache capacity (256
  per `Queries`); a low rate without evictions is just cold traffic.
- **`inserts`** counts distinct composed SQL texts — the driver-side
  proxy for prepared statements (below).
- **Coverage** = `shapes.used / shapes.reachable` per query, valid
  only while `unbounded=false` and `saturated=false`; shapes verified
  but never used are pruning candidates.
- **`reject.count`** spiking on `tree_too_large` or
  `shape_key_limit` means callers are pushing past the configured
  caps — misuse or abuse, worth alerting on.

## Cardinality policy

The shape key never appears as a per-event metric label: its
`;t=` (tree structure) and `;n=` (arity) segments are
caller-controlled and unbounded, and an unbounded label set is a
memory leak in your metrics pipeline. Per-event series are labeled by
query name (a bounded, generated set) and small closed sets only.
Full-cardinality shape data goes to trace spans (`TraceObserver`) or
the bounded `shapes.top` snapshot. `shapes.used` counts exactly up to
a bound (default 4096, `WithUsedShapeBound`), then freezes and flips
its `saturated` label — a floor, never a silent truncation.

## Prepared statements: what sqletch can and cannot report

sqletch hands SQL text to the driver; *preparation* happens inside
pgx / database/sql, per connection, and the server holds the
authoritative count. Join three sources:

- **sqletch**: `sqletch.cache.inserts` (cumulative distinct SQL
  texts) and `sqletch.cache.entries` (currently live ones). Each
  distinct text is what a per-connection statement cache will
  prepare, so `entries × pool size` bounds driver-side statements.
- **Driver**: pgx's per-connection statement cache
  (`pgxpool.Stat()` for pool health); database/sql prepares per
  distinct text per connection.
- **Server**: `pg_prepared_statements` (PostgreSQL),
  `Prepared_stmt_count` (MySQL).

On the expanding dialects (MySQL, SQLite), every distinct `@in` arity
is a distinct SQL text: an unbounded arity spread inflates all three
layers. `sqletch.shapes.used` on the `@in` query is the early
warning; consider binding arity at the call site (e.g. chunking to a
fixed size) if it grows without bound.

## Notes

- **Statically expanded queries** (`static_expansion`) never touch
  the cache: they emit exec and reject events only, and their absence
  from compose metrics is expected.
- **`OnQuery` is unchanged** — it remains the zero-setup debugging
  tap. The observer supersedes it for production metrics.
- **Overhead**: with no observer installed, the only additions to a
  call are nil checks and one atomic load; the hit path stays
  allocation-free (pinned by benchmarks). With metrics enabled, the
  dominant costs are one shape-key encoding per executed call and
  per-entry hit-counter contention on very hot shapes —
  `BenchmarkGeneratedCallParallelObserved` quantifies the worst case.
- **`ObserveExec` receives the key's canonical encoding** (a string),
  not a `runtime.ShapeKey` — the structured key is available on
  `ObserveCompose`. This asymmetry keeps unobserved generated calls
  free of heap allocation; see doc 17 §4.
