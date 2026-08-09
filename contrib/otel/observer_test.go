package otelsqletch

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/moznion/go-sqletch/runtime"
)

// fakeQueries mirrors the generated Queries wiring: SetObserver also
// points the cache at the observer, exactly as generated code does.
type fakeQueries struct{ cache *runtime.ComposedCache }

func (f *fakeQueries) SetObserver(o runtime.Observer) { f.cache.SetObserver(o) }
func (f *fakeQueries) Cache() *runtime.ComposedCache  { return f.cache }

var testFrags = []runtime.Frag{{Kind: runtime.Skel, Text: "SELECT 1"}}

func keyG(g uint64) runtime.ShapeKey { return runtime.ShapeKey{Guards: g} }

func setup(t *testing.T, opts ...Option) (*sdkmetric.ManualReader, *Binding, *fakeQueries) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	m, err := New(mp, opts...)
	if err != nil {
		t.Fatal(err)
	}
	fq := &fakeQueries{cache: runtime.NewComposedCache(16)}
	b, err := m.Bind("main", fq, map[string]runtime.ShapeSpaceInfo{
		"Q":     {Enumerable: 8, Exact: true},
		"Never": {Enumerable: 2, Exact: true, Unbounded: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return reader, b, fq
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != ScopeName {
			continue
		}
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func sumPoints(t *testing.T, m metricdata.Metrics) []metricdata.DataPoint[int64] {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s: not an int64 sum: %T", m.Name, m.Data)
	}
	return sum.DataPoints
}

func gaugePoints(t *testing.T, m metricdata.Metrics) []metricdata.DataPoint[int64] {
	t.Helper()
	g, ok := m.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("%s: not an int64 gauge: %T", m.Name, m.Data)
	}
	return g.DataPoints
}

func attrString(set attribute.Set, key string) (string, bool) {
	v, ok := set.Value(attribute.Key(key))
	if !ok {
		return "", false
	}
	return v.AsString(), true
}

func attrBool(t *testing.T, set attribute.Set, key string) bool {
	t.Helper()
	v, ok := set.Value(attribute.Key(key))
	if !ok {
		t.Fatalf("attribute %q missing in %v", key, set.Encoded(attribute.DefaultEncoder()))
	}
	return v.AsBool()
}

func TestBindRecordsInstruments(t *testing.T) {
	reader, b, fq := setup(t)
	ctx := context.Background()

	fq.cache.Get("Q", testFrags, keyG(0)) // miss
	fq.cache.Get("Q", testFrags, keyG(0)) // hit
	fq.cache.Get("Q", testFrags, keyG(1)) // miss, second distinct shape
	b.ObserveExec(ctx, "Q", "g=0", 5*time.Millisecond, 3, nil)
	b.ObserveExec(ctx, "Q", "g=0", time.Millisecond, -1, context.DeadlineExceeded)
	b.ObserveReject(ctx, "Q", runtime.ErrTreeTooLarge)

	ms := collect(t, reader)
	for _, name := range []string{
		"sqletch.compose.calls", "sqletch.exec.duration", "sqletch.exec.rows",
		"sqletch.reject.count", "sqletch.cache.hits", "sqletch.cache.misses",
		"sqletch.cache.inserts", "sqletch.cache.evictions", "sqletch.cache.entries",
		"sqletch.cache.capacity", "sqletch.cache.sql_bytes", "sqletch.shapes.used",
		"sqletch.shapes.reachable", "sqletch.shapes.top",
	} {
		if _, ok := ms[name]; !ok {
			t.Errorf("instrument %s not collected", name)
		}
	}

	// compose.calls: 2 misses, 1 hit — and never a shape-key label.
	var hits, misses int64
	for _, dp := range sumPoints(t, ms["sqletch.compose.calls"]) {
		if _, leaked := attrString(dp.Attributes, "key"); leaked {
			t.Error("compose.calls carries a shape-key label — the §7 rule")
		}
		if q, _ := attrString(dp.Attributes, "query"); q != "Q" {
			t.Errorf("compose.calls query label: %q", q)
		}
		if attrBool(t, dp.Attributes, "hit") {
			hits += dp.Value
		} else {
			misses += dp.Value
		}
	}
	if hits != 1 || misses != 2 {
		t.Errorf("compose.calls: got %d hits / %d misses, want 1 / 2", hits, misses)
	}

	// exec.duration: one ok, one error; no key label.
	durs := ms["sqletch.exec.duration"].Data.(metricdata.Histogram[float64]).DataPoints
	statuses := map[string]uint64{}
	for _, dp := range durs {
		if _, leaked := attrString(dp.Attributes, "key"); leaked {
			t.Error("exec.duration carries a shape-key label")
		}
		s, _ := attrString(dp.Attributes, "status")
		statuses[s] += dp.Count
	}
	if statuses["ok"] != 1 || statuses["error"] != 1 {
		t.Errorf("exec.duration statuses: %v", statuses)
	}

	// exec.rows records only known counts (the -1 call is excluded).
	rowsPts := ms["sqletch.exec.rows"].Data.(metricdata.Histogram[int64]).DataPoints
	if len(rowsPts) != 1 || rowsPts[0].Count != 1 || rowsPts[0].Sum != 3 {
		t.Errorf("exec.rows datapoints: %+v", rowsPts)
	}

	// reject.count classified by sentinel.
	rejects := sumPoints(t, ms["sqletch.reject.count"])
	if len(rejects) != 1 {
		t.Fatalf("reject.count datapoints: %d", len(rejects))
	}
	if e, _ := attrString(rejects[0].Attributes, "error"); e != "tree_too_large" {
		t.Errorf("reject error label: %q", e)
	}

	// Cache counters agree with Stats.
	s := fq.cache.Stats()
	if got := sumPoints(t, ms["sqletch.cache.hits"])[0].Value; got != int64(s.Hits) {
		t.Errorf("cache.hits: got %d, want %d", got, s.Hits)
	}
	if got := gaugePoints(t, ms["sqletch.cache.entries"])[0].Value; got != int64(s.Entries) {
		t.Errorf("cache.entries: got %d, want %d", got, s.Entries)
	}

	// shapes.used: 2 distinct shapes for Q, not saturated.
	used := gaugePoints(t, ms["sqletch.shapes.used"])
	if len(used) != 1 || used[0].Value != 2 || attrBool(t, used[0].Attributes, "saturated") {
		t.Errorf("shapes.used: %+v", used)
	}

	// shapes.reachable: one row per registry entry, flags carried.
	reach := gaugePoints(t, ms["sqletch.shapes.reachable"])
	if len(reach) != 2 {
		t.Fatalf("shapes.reachable rows: %d", len(reach))
	}
	for _, dp := range reach {
		q, _ := attrString(dp.Attributes, "query")
		switch q {
		case "Q":
			if dp.Value != 8 || attrBool(t, dp.Attributes, "unbounded") {
				t.Errorf("reachable[Q]: %+v", dp)
			}
		case "Never":
			if dp.Value != 2 || !attrBool(t, dp.Attributes, "unbounded") {
				t.Errorf("reachable[Never]: %+v", dp)
			}
		default:
			t.Errorf("unexpected reachable query %q", q)
		}
	}

	// shapes.top is the one deliberately key-labeled series.
	top := gaugePoints(t, ms["sqletch.shapes.top"])
	if len(top) == 0 || len(top) > 10 {
		t.Fatalf("shapes.top rows: %d", len(top))
	}
	if _, ok := attrString(top[0].Attributes, "key"); !ok {
		t.Error("shapes.top lacks its key label")
	}
}

func TestUsedShapeSaturation(t *testing.T) {
	reader, _, fq := setup(t, WithUsedShapeBound(2))
	for g := uint64(0); g < 3; g++ {
		fq.cache.Get("Q", testFrags, keyG(g))
	}
	ms := collect(t, reader)
	used := gaugePoints(t, ms["sqletch.shapes.used"])
	if len(used) != 1 {
		t.Fatalf("shapes.used rows: %d", len(used))
	}
	if used[0].Value != 2 || !attrBool(t, used[0].Attributes, "saturated") {
		t.Errorf("saturation: value %d, saturated %v — the count must become a loud floor",
			used[0].Value, attrBool(t, used[0].Attributes, "saturated"))
	}
}

// recordingNext proves TraceObserver forwards.
type recordingNext struct{ execs, rejects, composes int }

func (r *recordingNext) ObserveCompose(string, runtime.ShapeKey, bool) { r.composes++ }
func (r *recordingNext) ObserveExec(context.Context, string, string, time.Duration, int64, error) {
	r.execs++
}
func (r *recordingNext) ObserveReject(context.Context, string, error) { r.rejects++ }

func TestTraceObserver(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	next := &recordingNext{}
	obs := TraceObserver{Next: next}

	ctx, span := tp.Tracer("test").Start(context.Background(), "handler")
	obs.ObserveExec(ctx, "Q", "g=3;c=1", 2*time.Millisecond, 7, nil)
	obs.ObserveReject(ctx, "Q", runtime.ErrChooseRequired)
	obs.ObserveCompose("Q", keyG(3), true)
	span.End()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans: %d", len(spans))
	}
	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range spans[0].Attributes() {
		attrs[kv.Key] = kv.Value
	}
	if got := attrs["sqletch.shape_key"].AsString(); got != "g=3;c=1" {
		t.Errorf("span shape key: %q", got)
	}
	if got := attrs["sqletch.rows"].AsInt64(); got != 7 {
		t.Errorf("span rows: %d", got)
	}
	events := spans[0].Events()
	if len(events) != 1 || events[0].Name != "sqletch.reject" {
		t.Fatalf("span events: %+v", events)
	}
	if next.execs != 1 || next.rejects != 1 || next.composes != 1 {
		t.Errorf("forwarding: %+v", next)
	}
}
