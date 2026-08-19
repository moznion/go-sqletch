// Package otelsqletch exports sqletch runtime metrics through the
// OpenTelemetry metrics API (design doc 18 §6).
//
// It lives in its own module so the OpenTelemetry dependency graph
// never touches the core runtime package or consumers who skip it:
// sqletch's runtime exposes a neutral [runtime.Observer] interface and
// poll-able snapshots, and this package is one adapter over them.
// Serving the Prometheus/OpenMetrics exposition format is the standard
// OTel Prometheus exporter's job, attached to the MeterProvider given
// to [New].
//
// Cardinality policy (doc 18 §7): the shape key never becomes a
// per-event metric label — its tree and arity segments are
// caller-controlled and unbounded. Per-event series are labeled by
// query name (a generated, bounded set), instance, and small closed
// sets (hit, status, error). Full-cardinality shape data travels on
// trace spans ([TraceObserver]) or in the bounded top-N snapshot,
// whose row count is capped by construction.
package otelsqletch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/moznion/go-sqletch/runtime"
)

// ScopeName is the instrumentation scope recorded on every metric.
const ScopeName = "github.com/moznion/go-sqletch/contrib/otel"

// Queries is the slice of a sqletch-generated Queries value this
// package needs: every generated package satisfies it.
type Queries interface {
	SetObserver(runtime.Observer)
	Cache() *runtime.ComposedCache
}

// Option configures New.
type Option func(*config)

type config struct {
	topN      int
	usedBound int
}

// WithTopShapes sets how many rows the per-scrape shape ranking
// (sqletch.shapes.top) exposes. Bounded by construction; default 10,
// values above 20 are clamped — the ranking exists to surface
// dominant shapes without opening the cardinality door.
func WithTopShapes(n int) Option { return func(c *config) { c.topN = n } }

// WithUsedShapeBound sets the per-query bound of the exact
// distinct-shape tracking behind sqletch.shapes.used (default 4096,
// matching the verification budget's default). At the bound the set
// stops growing and the series' `saturated` attribute flips true: the
// count becomes a floor, loudly, never silently (doc 18 D5).
func WithUsedShapeBound(n int) Option { return func(c *config) { c.usedBound = n } }

// Metrics owns the instruments. Create one per MeterProvider and
// Bind each generated Queries instance to it.
type Metrics struct {
	cfg   config
	meter metric.Meter

	composeCalls metric.Int64Counter
	execDuration metric.Float64Histogram
	execRows     metric.Int64Histogram
	rejects      metric.Int64Counter

	cacheHits      metric.Int64ObservableCounter
	cacheMisses    metric.Int64ObservableCounter
	cacheInserts   metric.Int64ObservableCounter
	cacheEvictions metric.Int64ObservableCounter
	cacheEntries   metric.Int64ObservableGauge
	cacheCapacity  metric.Int64ObservableGauge
	cacheSQLBytes  metric.Int64ObservableGauge
	shapesUsed     metric.Int64ObservableGauge
	shapesReach    metric.Int64ObservableGauge
	shapesTop      metric.Int64ObservableGauge
}

// New creates the instrument set on mp's meter for [ScopeName].
func New(mp metric.MeterProvider, opts ...Option) (*Metrics, error) {
	cfg := config{topN: 10, usedBound: 4096}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.topN < 1 {
		cfg.topN = 1
	}
	if cfg.topN > 20 {
		cfg.topN = 20
	}
	if cfg.usedBound < 1 {
		cfg.usedBound = 1
	}

	m := &Metrics{cfg: cfg, meter: mp.Meter(ScopeName)}
	var err error
	fail := func(e error) bool {
		if e != nil && err == nil {
			err = e
		}
		return e != nil
	}

	m.composeCalls, err = m.meter.Int64Counter("sqletch.compose.calls",
		metric.WithUnit("{call}"),
		metric.WithDescription("Composed-SQL cache accesses, by query and hit/miss."))
	if err != nil {
		return nil, err
	}
	var e error
	m.execDuration, e = m.meter.Float64Histogram("sqletch.exec.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Database call duration, by query and status."))
	fail(e)
	m.execRows, e = m.meter.Int64Histogram("sqletch.exec.rows",
		metric.WithUnit("{row}"),
		metric.WithDescription("Rows returned (queries) or affected (execs), by query."))
	fail(e)
	m.rejects, e = m.meter.Int64Counter("sqletch.reject.count",
		metric.WithUnit("{rejection}"),
		metric.WithDescription("Calls refused before any SQL was sent, by query and sentinel error."))
	fail(e)
	m.cacheHits, e = m.meter.Int64ObservableCounter("sqletch.cache.hits",
		metric.WithDescription("Cumulative composed-SQL cache hits (counting starts at first observation)."))
	fail(e)
	m.cacheMisses, e = m.meter.Int64ObservableCounter("sqletch.cache.misses",
		metric.WithDescription("Cumulative compositions performed (raced duplicates included)."))
	fail(e)
	m.cacheInserts, e = m.meter.Int64ObservableCounter("sqletch.cache.inserts",
		metric.WithDescription("Cumulative cache entries created — the distinct-SQL proxy for driver-side prepared statements."))
	fail(e)
	m.cacheEvictions, e = m.meter.Int64ObservableCounter("sqletch.cache.evictions",
		metric.WithDescription("Cumulative second-chance evictions; a steady nonzero rate means the shape set outgrows the capacity."))
	fail(e)
	m.cacheEntries, e = m.meter.Int64ObservableGauge("sqletch.cache.entries",
		metric.WithDescription("Resident cache entries."))
	fail(e)
	m.cacheCapacity, e = m.meter.Int64ObservableGauge("sqletch.cache.capacity",
		metric.WithDescription("Cache capacity."))
	fail(e)
	m.cacheSQLBytes, e = m.meter.Int64ObservableGauge("sqletch.cache.sql_bytes",
		metric.WithUnit("By"),
		metric.WithDescription("Total bytes of composed SQL retained by resident entries."))
	fail(e)
	m.shapesUsed, e = m.meter.Int64ObservableGauge("sqletch.shapes.used",
		metric.WithUnit("{shape}"),
		metric.WithDescription("Distinct shapes composed since start, per query; a floor once `saturated` is true."))
	fail(e)
	m.shapesReach, e = m.meter.Int64ObservableGauge("sqletch.shapes.reachable",
		metric.WithUnit("{shape}"),
		metric.WithDescription("Statically verified reachable shapes of the enumerable dimensions, per query (generate-time constant)."))
	fail(e)
	m.shapesTop, e = m.meter.Int64ObservableGauge("sqletch.shapes.top",
		metric.WithUnit("{hit}"),
		metric.WithDescription("Hit counts of the top-N resident shapes — the one deliberately shape-key-labeled, bounded series."))
	fail(e)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// Bind wires one generated Queries value into the instruments: it
// installs a per-instance observer (compose, exec, reject events) and
// registers a scrape-time callback over the instance's cache stats,
// shape usage, and the ShapeSpace registry (pass the generated
// package's `ShapeSpace` variable). Close the returned Binding to
// unregister the callback.
func (m *Metrics) Bind(instance string, q Queries, space map[string]runtime.ShapeSpaceInfo) (*Binding, error) {
	b := &Binding{
		m:        m,
		instance: attribute.String("instance", instance),
		cache:    q.Cache(),
		space:    space,
	}
	reg, err := m.meter.RegisterCallback(b.collect,
		m.cacheHits, m.cacheMisses, m.cacheInserts, m.cacheEvictions,
		m.cacheEntries, m.cacheCapacity, m.cacheSQLBytes,
		m.shapesUsed, m.shapesReach, m.shapesTop)
	if err != nil {
		return nil, fmt.Errorf("otelsqletch: register callback: %w", err)
	}
	b.reg = reg
	q.SetObserver(b)
	return b, nil
}

// Binding is the per-instance [runtime.Observer] Bind installs.
type Binding struct {
	m        *Metrics
	instance attribute.KeyValue
	cache    *runtime.ComposedCache
	space    map[string]runtime.ShapeSpaceInfo
	reg      metric.Registration

	// attrs caches per-query measurement options: attribute sets are
	// immutable and shared, so the hot path reuses them instead of
	// rebuilding key-value slices per event. Each *queryAttrs also
	// carries that query's bounded distinct-shape set, so distinct
	// tracking is sharded per query (no global lock) and a saturated set
	// is detected lock-free on the compose hot path.
	attrs sync.Map // query string -> *queryAttrs
}

// Close unregisters the scrape callback. The observer keeps counting
// into the synchronous instruments until the Queries value is
// re-pointed elsewhere.
func (b *Binding) Close() error { return b.reg.Unregister() }

// usedSet is one query's bounded distinct-shape set. Its own mutex
// guards keys (so tracking is sharded per query, never behind one global
// lock), and saturated is atomic so the compose hot path can skip the
// lock and the key encoding entirely once the bound is reached.
type usedSet struct {
	mu        sync.Mutex
	keys      map[string]struct{}
	saturated atomic.Bool
}

type queryAttrs struct {
	hit, miss     metric.MeasurementOption
	durOK, durErr metric.MeasurementOption
	rows          metric.MeasurementOption
	usedPlain     attribute.Set // + saturated flag variants, scrape-time only
	usedSaturated attribute.Set
	// used is this query's distinct-shape set, created lazily on the
	// first compose event (nil until then, so scrape only reports queries
	// that actually composed). Held via an atomic pointer so the hot path
	// reads it without a lock.
	used atomic.Pointer[usedSet]
}

func (b *Binding) queryAttrs(query string) *queryAttrs {
	if qa, ok := b.attrs.Load(query); ok {
		return qa.(*queryAttrs)
	}
	q := attribute.String("query", query)
	qa := &queryAttrs{
		hit:           metric.WithAttributeSet(attribute.NewSet(b.instance, q, attribute.Bool("hit", true))),
		miss:          metric.WithAttributeSet(attribute.NewSet(b.instance, q, attribute.Bool("hit", false))),
		durOK:         metric.WithAttributeSet(attribute.NewSet(b.instance, q, attribute.String("status", "ok"))),
		durErr:        metric.WithAttributeSet(attribute.NewSet(b.instance, q, attribute.String("status", "error"))),
		rows:          metric.WithAttributeSet(attribute.NewSet(b.instance, q)),
		usedPlain:     attribute.NewSet(b.instance, q, attribute.Bool("saturated", false)),
		usedSaturated: attribute.NewSet(b.instance, q, attribute.Bool("saturated", true)),
	}
	actual, _ := b.attrs.LoadOrStore(query, qa)
	return actual.(*queryAttrs)
}

// ObserveCompose implements [runtime.Observer]. The compose event has
// no caller context (the cache API takes none), so the counter records
// against the background context.
func (b *Binding) ObserveCompose(query string, key runtime.ShapeKey, hit bool) {
	qa := b.queryAttrs(query)
	opt := qa.miss
	if hit {
		opt = qa.hit
	}
	b.m.composeCalls.Add(context.Background(), 1, opt)

	// Exact distinct tracking, bounded (doc 18 D5). Once a query's set is
	// saturated it never grows again, so the hot path returns here
	// without encoding the key or taking any lock — the common
	// steady-state cost of an observed cache is just the counter Add
	// above. The lock, when taken, is this query's own (sharded), never a
	// global one shared across every query and goroutine.
	us := qa.used.Load()
	if us == nil {
		ns := &usedSet{keys: map[string]struct{}{}}
		if qa.used.CompareAndSwap(nil, ns) {
			us = ns
		} else {
			us = qa.used.Load()
		}
	}
	if us.saturated.Load() {
		return
	}
	// The encoding is built here, adapter-side: the core hands over its
	// retained key for free and stays free of unbounded state.
	enc := key.String()
	us.mu.Lock()
	if _, seen := us.keys[enc]; !seen {
		if len(us.keys) >= b.m.cfg.usedBound {
			us.saturated.Store(true)
		} else {
			us.keys[enc] = struct{}{}
		}
	}
	us.mu.Unlock()
}

// ObserveExec implements [runtime.Observer]. The shape key encoding is
// deliberately NOT a label (doc 18 §7); it reaches traces via
// [TraceObserver].
func (b *Binding) ObserveExec(ctx context.Context, query, _ string, d time.Duration, rows int64, err error) {
	qa := b.queryAttrs(query)
	opt := qa.durOK
	if err != nil {
		opt = qa.durErr
	}
	b.m.execDuration.Record(ctx, d.Seconds(), opt)
	if rows >= 0 {
		b.m.execRows.Record(ctx, rows, qa.rows)
	}
}

// ObserveReject implements [runtime.Observer].
func (b *Binding) ObserveReject(ctx context.Context, query string, err error) {
	b.m.rejects.Add(ctx, 1, metric.WithAttributes(
		b.instance,
		attribute.String("query", query),
		attribute.String("error", rejectName(err))))
}

// rejectName classifies a rejection by sentinel, per the Observer
// contract (errors.Is, never message matching). The set is closed, so
// the label is bounded.
func rejectName(err error) string {
	switch {
	case errors.Is(err, runtime.ErrChooseRequired):
		return "choose_required"
	case errors.Is(err, runtime.ErrOrderKey):
		return "order_key"
	case errors.Is(err, runtime.ErrFilterRequired):
		return "filter_required"
	case errors.Is(err, runtime.ErrTreeTooLarge):
		return "tree_too_large"
	case errors.Is(err, runtime.ErrTreePredicate):
		return "tree_predicate"
	case errors.Is(err, runtime.ErrShapeKeyLimit):
		return "shape_key_limit"
	case errors.Is(err, runtime.ErrShapeNotExpanded):
		return "shape_not_expanded"
	default:
		return "other"
	}
}

// collect is the scrape-time callback: cache stats, shape coverage,
// and the bounded top-N ranking.
func (b *Binding) collect(_ context.Context, o metric.Observer) error {
	inst := metric.WithAttributes(b.instance)
	s := b.cache.Stats()
	o.ObserveInt64(b.m.cacheHits, clampUint64(s.Hits), inst)
	o.ObserveInt64(b.m.cacheMisses, clampUint64(s.Misses), inst)
	o.ObserveInt64(b.m.cacheInserts, clampUint64(s.Inserts), inst)
	o.ObserveInt64(b.m.cacheEvictions, clampUint64(s.Evictions), inst)
	o.ObserveInt64(b.m.cacheEntries, int64(s.Entries), inst)
	o.ObserveInt64(b.m.cacheCapacity, int64(s.Capacity), inst)
	o.ObserveInt64(b.m.cacheSQLBytes, s.SQLBytes, inst)

	// Sorted iteration: byte-identical scrape output for equal state,
	// the repo-wide determinism discipline.
	names := make([]string, 0, len(b.space))
	for name := range b.space {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		info := b.space[name]
		o.ObserveInt64(b.m.shapesReach, clampUint64(info.Enumerable), metric.WithAttributes(
			b.instance,
			attribute.String("query", name),
			attribute.Bool("exact", info.Exact),
			attribute.Bool("unbounded", info.Unbounded)))
	}

	type usedRow struct {
		name  string
		set   attribute.Set
		count int64
	}
	var rows []usedRow
	b.attrs.Range(func(k, v any) bool {
		qa := v.(*queryAttrs)
		us := qa.used.Load()
		if us == nil { // observed (e.g. exec) but never composed a shape
			return true
		}
		sat := us.saturated.Load()
		us.mu.Lock()
		count := int64(len(us.keys))
		us.mu.Unlock()
		set := qa.usedPlain
		if sat {
			set = qa.usedSaturated
		}
		rows = append(rows, usedRow{name: k.(string), set: set, count: count})
		return true
	})
	// Sorted output: byte-identical scrapes for equal state, the
	// repo-wide determinism discipline.
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	for _, r := range rows {
		o.ObserveInt64(b.m.shapesUsed, r.count, metric.WithAttributeSet(r.set))
	}

	for i, u := range b.cache.TopShapes(b.m.cfg.topN) {
		o.ObserveInt64(b.m.shapesTop, clampUint64(u.Hits), metric.WithAttributes(
			b.instance,
			attribute.String("query", u.Query),
			attribute.String("key", u.Key),
			attribute.Int("rank", i+1)))
	}
	return nil
}

func clampUint64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}
