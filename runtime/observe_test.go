package runtime

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingObserver is concurrency-safe: compose events arrive from the
// lock-free hit path of many goroutines at once.
type recordingObserver struct {
	mu       sync.Mutex
	composes []composeEvent
	execs    []execEvent
	rejects  []rejectEvent
}

type composeEvent struct {
	query string
	key   string // canonical encoding, copied (the ShapeKey is not retained)
	hit   bool
}

type execEvent struct {
	query string
	rows  int64
	err   error
}

type rejectEvent struct {
	query string
	err   error
}

func (r *recordingObserver) ObserveCompose(query string, key ShapeKey, hit bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.composes = append(r.composes, composeEvent{query: query, key: key.String(), hit: hit})
}

func (r *recordingObserver) ObserveExec(query string, _ ShapeKey, _ time.Duration, rows int64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.execs = append(r.execs, execEvent{query: query, rows: rows, err: err})
}

func (r *recordingObserver) ObserveReject(query string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rejects = append(r.rejects, rejectEvent{query: query, err: err})
}

func (r *recordingObserver) hitCounts() (hits, misses int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.composes {
		if e.hit {
			hits++
		} else {
			misses++
		}
	}
	return
}

// countingObserver only touches atomics: the allocation test needs an
// observer whose own bookkeeping cannot allocate.
type countingObserver struct{ composes, hits atomic.Uint64 }

func (o *countingObserver) ObserveCompose(_ string, _ ShapeKey, hit bool) {
	o.composes.Add(1)
	if hit {
		o.hits.Add(1)
	}
}
func (o *countingObserver) ObserveExec(string, ShapeKey, time.Duration, int64, error) {}
func (o *countingObserver) ObserveReject(string, error)                               {}

func keyG(g uint64) ShapeKey { return ShapeKey{Guards: g, Choices: []uint8{1}} }

// TestCacheStatsScripted walks a fixed hit/miss/evict sequence and pins
// every CacheStats field, including the CLOCK second-chance outcome.
func TestCacheStatsScripted(t *testing.T) {
	frags := benchFrags()
	c := NewComposedCache(2)
	c.Stats() // first observability call: enables hit counting (sticky)

	sqlOf := func(g uint64) string {
		s, _ := ComposeStyle(StyleDollar, frags, keyG(g))
		return s
	}

	c.Get("Q", frags, keyG(0)) // miss: insert A
	c.Get("Q", frags, keyG(0)) // hit: A.ref set
	c.Get("Q", frags, keyG(1)) // miss: insert B
	// Miss: insert C over capacity. CLOCK from the back: A was touched,
	// so it gets a second chance and B (never hit) is evicted.
	c.Get("Q", frags, keyG(2))

	got := c.Stats()
	want := CacheStats{
		Hits: 1, Misses: 3, Inserts: 3, Evictions: 1,
		Entries: 2, Capacity: 2,
		SQLBytes: int64(len(sqlOf(0)) + len(sqlOf(2))),
	}
	if got != want {
		t.Fatalf("stats after scripted sequence:\ngot  %+v\nwant %+v", got, want)
	}
}

// TestCacheStatsHitsSurviveEviction pins the fold-in: an evicted
// entry's hit count stays in the cumulative total.
func TestCacheStatsHitsSurviveEviction(t *testing.T) {
	frags := benchFrags()
	c := NewComposedCache(1)
	c.Stats() // enable hit counting

	c.Get("Q", frags, keyG(0))
	for range 3 {
		c.Get("Q", frags, keyG(0)) // hits(A) = 3, ref set
	}
	// First insert of B: A's ref grants it a second chance and the
	// CLOCK evicts B itself. Second insert: A's ref is now clear, A is
	// evicted and its 3 hits must fold into the cumulative counter.
	c.Get("Q", frags, keyG(1))
	c.Get("Q", frags, keyG(1))

	s := c.Stats()
	if s.Hits != 3 {
		t.Fatalf("cumulative hits after evicting the hot entry: got %d, want 3", s.Hits)
	}
	if s.Entries != 1 || s.Evictions != 2 || s.Inserts != 3 {
		t.Fatalf("unexpected occupancy: %+v", s)
	}
	if want := int64(len(mustCompose(t, frags, keyG(1)))); s.SQLBytes != want {
		t.Fatalf("SQLBytes after eviction: got %d, want %d", s.SQLBytes, want)
	}
}

func mustCompose(t *testing.T, frags []Frag, key ShapeKey) string {
	t.Helper()
	s, _ := ComposeStyle(StyleDollar, frags, key)
	return s
}

// TestObserverComposeEvents pins the hit bit on all three serve paths:
// the lock-free snapshot hit, the locked hit (entry resident but not
// yet republished after an eviction), and the miss.
func TestObserverComposeEvents(t *testing.T) {
	frags := benchFrags()
	c := NewComposedCache(4)
	obs := &recordingObserver{}
	c.SetObserver(obs)

	for g := uint64(0); g < 5; g++ { // 5 misses; the 5th evicts
		c.Get("Q", frags, keyG(g))
	}
	// The eviction defers republishing (publish amortization), so the
	// entry inserted by the 5th miss is resident but absent from the
	// fast snapshot: this hit must come from the locked path.
	c.Get("Q", frags, keyG(4))
	// And an entry that predates the eviction is still in the
	// snapshot: a lock-free hit.
	c.Get("Q", frags, keyG(3))

	hits, misses := obs.hitCounts()
	if hits != 2 || misses != 5 {
		t.Fatalf("compose events: got %d hits / %d misses, want 2 / 5", hits, misses)
	}
	s := c.Stats()
	if s.Hits != 2 || s.Misses != 5 {
		t.Fatalf("stats disagree with observer: %+v", s)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	for _, e := range obs.composes {
		if e.query != "Q" {
			t.Fatalf("compose event query: got %q, want Q", e.query)
		}
		if e.key == "" {
			t.Fatal("compose event carried an empty key encoding")
		}
	}
}

// TestObserverConcurrentSameShape hammers one shape from many
// goroutines: every access must produce exactly one compose event, at
// least one of which is a miss, and raced duplicate compositions may
// exceed insertions but never undercount (hits+misses == accesses).
func TestObserverConcurrentSameShape(t *testing.T) {
	frags := benchFrags()
	c := NewComposedCache(8)
	obs := &countingObserver{}
	c.SetObserver(obs)

	const workers, perWorker = 8, 200
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				c.Get("Q", frags, keyG(2))
			}
		}()
	}
	wg.Wait()

	total := obs.composes.Load()
	hits := obs.hits.Load()
	if total != workers*perWorker {
		t.Fatalf("compose events: got %d, want %d", total, workers*perWorker)
	}
	if hits == total {
		t.Fatal("no miss event: someone composed this shape")
	}
	s := c.Stats()
	if s.Inserts != 1 || s.Entries != 1 {
		t.Fatalf("one shape must yield one entry: %+v", s)
	}
	if s.Hits != hits || s.Hits+s.Misses != total {
		t.Fatalf("stats/observer mismatch: stats %+v, observer %d/%d", s, hits, total)
	}
}

// TestTopShapesDeterministic pins the ranking's total order: hits
// descending, then query and key ascending — equal state must yield
// byte-identical output (the repo-wide determinism rule).
func TestTopShapesDeterministic(t *testing.T) {
	frags := benchFrags()
	c := NewComposedCache(16)
	c.Stats() // enable hit counting

	c.Get("B", frags, keyG(1))
	c.Get("A", frags, keyG(2))
	c.Get("A", frags, keyG(1)) // tied at 0 extra hits with the two above
	c.Get("C", frags, keyG(0))
	c.Get("C", frags, keyG(0)) // 1 hit: must rank first
	c.Get("C", frags, keyG(0)) // 2 hits

	want := []ShapeUse{
		{Query: "C", Key: keyG(0).String(), Hits: 2, SQLBytes: len(mustCompose(t, frags, keyG(0)))},
		{Query: "A", Key: keyG(1).String(), Hits: 0, SQLBytes: len(mustCompose(t, frags, keyG(1)))},
		{Query: "A", Key: keyG(2).String(), Hits: 0, SQLBytes: len(mustCompose(t, frags, keyG(2)))},
		{Query: "B", Key: keyG(1).String(), Hits: 0, SQLBytes: len(mustCompose(t, frags, keyG(1)))},
	}
	for range 10 { // ordering must not depend on map iteration
		got := c.TopShapes(10)
		if len(got) != len(want) {
			t.Fatalf("TopShapes rows: got %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("TopShapes[%d]:\ngot  %+v\nwant %+v", i, got[i], want[i])
			}
		}
	}

	if got := c.TopShapes(2); len(got) != 2 || got[0] != want[0] {
		t.Fatalf("TopShapes(2) truncation: %+v", got)
	}
	if got := c.TopShapes(0); got != nil {
		t.Fatalf("TopShapes(0): got %+v, want nil", got)
	}
}

// TestHitCountingStartsAtFirstObservation pins the documented gate:
// hits before the first SetObserver/Stats/TopShapes call are not
// counted (the unobserved hit path pays nothing), and counting is
// sticky once enabled.
func TestHitCountingStartsAtFirstObservation(t *testing.T) {
	frags := benchFrags()
	c := NewComposedCache(8)

	c.Get("Q", frags, keyG(0))
	c.Get("Q", frags, keyG(0)) // a hit, but nobody has asked for stats yet
	if s := c.Stats(); s.Hits != 0 || s.Misses != 1 {
		t.Fatalf("pre-observation counts: got %+v, want Hits 0 / Misses 1", s)
	}
	c.Get("Q", frags, keyG(0))
	if s := c.Stats(); s.Hits != 1 {
		t.Fatalf("post-observation hit uncounted: %+v", s)
	}
}

// TestHitPathAllocationFree pins that neither the per-entry hit
// counter nor the observer call adds an allocation to the lock-free
// hit path.
func TestHitPathAllocationFree(t *testing.T) {
	frags := benchFrags()
	key := keyG(3)

	for _, withObserver := range []bool{false, true} {
		c := NewComposedCache(8)
		if withObserver {
			c.SetObserver(&countingObserver{})
		}
		c.Get("Q", frags, key) // warm
		allocs := testing.AllocsPerRun(200, func() {
			c.Get("Q", frags, key)
		})
		if allocs != 0 {
			t.Errorf("hit path allocations (observer=%v): got %v, want 0", withObserver, allocs)
		}
	}
}

// TestGetTreeObserved pins that the tree entry points feed the same
// observer and stats, keyed by the tree's structural encoding.
func TestGetTreeObserved(t *testing.T) {
	frags := benchTreeFrags()
	c := NewComposedCache(8)
	obs := &recordingObserver{}
	c.SetObserver(obs)
	tree := benchTree()

	if _, _, err := c.GetTree("T", frags, ShapeKey{}, tree, DefaultTreeCaps); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.GetTree("T", frags, ShapeKey{}, tree, DefaultTreeCaps); err != nil {
		t.Fatal(err)
	}

	hits, misses := obs.hitCounts()
	if hits != 1 || misses != 1 {
		t.Fatalf("tree compose events: got %d hits / %d misses, want 1 / 1", hits, misses)
	}
	obs.mu.Lock()
	enc := obs.composes[0].key
	obs.mu.Unlock()
	if want := (ShapeKey{Trees: []string{tree.Encode()}}).String(); enc != want {
		t.Fatalf("tree compose key: got %q, want %q", enc, want)
	}

	top := c.TopShapes(1)
	if len(top) != 1 || top[0].Query != "T" || top[0].Hits != 1 {
		t.Fatalf("TopShapes for tree entry: %+v", top)
	}

	// A caps rejection reaches the caller before any entry churn and
	// fires no compose event (generated code reports the reject).
	deep := NewLeaf(0, "x")
	for range DefaultTreeCaps.MaxDepth + 1 {
		deep = And(deep, NewLeaf(0, "x"))
	}
	before := c.Stats()
	if _, _, err := c.GetTree("T", frags, ShapeKey{}, deep, DefaultTreeCaps); !errors.Is(err, ErrTreeTooLarge) {
		t.Fatalf("oversized tree: got %v, want ErrTreeTooLarge", err)
	}
	if after := c.Stats(); after != before {
		t.Fatalf("a rejected tree mutated stats: before %+v, after %+v", before, after)
	}
	if h, m := obs.hitCounts(); h != 1 || m != 1 {
		t.Fatalf("rejected tree fired a compose event: %d/%d", h, m)
	}
}

// TestStatsUnderChurn cross-checks the counters against a serial
// reference workload whose shape space exceeds capacity.
func TestStatsUnderChurn(t *testing.T) {
	frags := benchFrags()
	c := NewComposedCache(4)
	c.Stats() // enable hit counting
	const rounds, shapes = 500, 12
	for n := range rounds {
		c.Get("Q", frags, keyG(uint64(n%shapes)))
	}
	s := c.Stats()
	if s.Hits+s.Misses != rounds {
		t.Fatalf("hits+misses = %d, want %d (%+v)", s.Hits+s.Misses, rounds, s)
	}
	if s.Entries > 4 || s.Inserts != s.Misses {
		t.Fatalf("occupancy: %+v", s)
	}
	if s.Evictions != s.Inserts-uint64(s.Entries) {
		t.Fatalf("evictions %d != inserts %d - entries %d", s.Evictions, s.Inserts, s.Entries)
	}
	var wantBytes int64
	for _, u := range c.TopShapes(shapes) {
		wantBytes += int64(u.SQLBytes)
	}
	if s.SQLBytes != wantBytes {
		t.Fatalf("SQLBytes %d != Σ TopShapes %d", s.SQLBytes, wantBytes)
	}
}

// TestStatsConcurrentChurn scrapes Stats and TopShapes while an
// eviction-heavy parallel workload runs — the race detector referees
// the new fields, and the totals must stay coherent up to the
// documented fold skew (a hit racing an eviction's fold-in can drop
// from the cumulative count).
func TestStatsConcurrentChurn(t *testing.T) {
	frags := benchFrags()
	c := NewComposedCache(2)
	obs := &countingObserver{}
	c.SetObserver(obs)

	const workers, perWorker = 8, 1000
	stop := make(chan struct{})
	var scraper sync.WaitGroup
	scraper.Add(1)
	go func() {
		defer scraper.Done()
		for {
			select {
			case <-stop:
				return
			default:
				c.Stats()
				c.TopShapes(3)
			}
		}
	}()
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range perWorker {
				c.Get("Q", frags, keyG(uint64((n*7+w*13)%16)))
			}
		}()
	}
	wg.Wait()
	close(stop)
	scraper.Wait()

	if got := obs.composes.Load(); got != workers*perWorker {
		t.Fatalf("compose events: got %d, want %d", got, workers*perWorker)
	}
	s := c.Stats()
	total := s.Hits + s.Misses
	if total > workers*perWorker {
		t.Fatalf("hits+misses = %d exceeds %d accesses", total, workers*perWorker)
	}
	// Each fold can miss the hits of at most the goroutines racing it;
	// signed arithmetic because an eviction-heavy run can push the
	// theoretical floor below zero.
	if floor := int64(workers*perWorker) - int64(s.Evictions)*workers; floor > 0 && int64(total) < floor {
		t.Fatalf("hits+misses = %d below fold-skew floor %d (%+v)", total, floor, s)
	}
	if s.Entries > 2 {
		t.Fatalf("entries exceed capacity: %+v", s)
	}
}
