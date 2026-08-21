package runtime

import "testing"

// TestCachePublishRateCappedUnderChurn pins the amortization of the
// snapshot copy under permanent churn — a workload whose shape set
// never fits the capacity (realistic on MySQL/SQLite, where @in arity
// is a shape-key dimension, so varied IN-list sizes insert forever).
//
// Each publish copies the whole entry map (O(capacity)). The insert
// path already amortizes its own publishes to one per cap inserts, but
// the mutex-path hit flush used to run unconditionally: every newly
// inserted shape's FIRST re-hit paid a full O(cap) copy, making churn
// cost ~one map copy per new shape. The flush must instead be
// rate-capped so publishes stay O(total operations / cap).
func TestCachePublishRateCappedUnderChurn(t *testing.T) {
	frags := benchFrags()
	const capacity = 32
	c := NewComposedCache(capacity)

	// n distinct shapes, each inserted then immediately re-hit: the
	// re-hit of every post-fill shape lands on the mutex path (its entry
	// was inserted after the last publish).
	const n = 2048
	for i := range n {
		k := ShapeKey{Guards: uint64(i), Choices: []uint8{0}}
		c.Get("Q", frags, k) // miss: insert
		c.Get("Q", frags, k) // first re-hit
	}

	c.mu.Lock()
	snaps := c.snapshots
	c.mu.Unlock()

	// Fill phase publishes once per insert (capacity publishes); after
	// that, the insert path publishes once per capacity inserts and the
	// hit path is allowed at most the same order (one credited flush per
	// insert-path publish, plus one per capacity deferred hits). Give
	// the bound a small constant slack; the unfixed behavior publishes
	// on nearly every re-hit (~n times) and must fail this by an order
	// of magnitude.
	limit := uint64(capacity + 4*(n/capacity) + 8)
	if snaps > limit {
		t.Fatalf("churn workload published %d snapshots for %d insert+re-hit pairs (cap %d); want <= %d — the O(cap) map copy is not amortized",
			snaps, n, capacity, limit)
	}
}

// TestCacheChurnStopsConvergesToFastPath pins the other side of the
// trade: rate-capping the flush must not strand entries behind the
// mutex once the workload stabilizes. Whatever the deferred-publish
// state left by churn, a shape that stays resident reaches the
// lock-free snapshot within a bounded number of hits (at most the
// capacity — the deferred-hit threshold), and stays there.
func TestCacheChurnStopsConvergesToFastPath(t *testing.T) {
	frags := benchFrags()
	const capacity = 16
	c := NewComposedCache(capacity)

	// Churn well past capacity, leaving the publish state mid-window.
	for i := range 10*capacity + 3 {
		k := ShapeKey{Guards: uint64(i), Choices: []uint8{0}}
		c.Get("Q", frags, k)
		c.Get("Q", frags, k)
	}

	// Inserts stop; a stable working set of one shape remains. It is
	// resident (just inserted) but possibly unpublished.
	last := ShapeKey{Guards: uint64(10*capacity + 2), Choices: []uint8{0}}
	for i := 0; i <= capacity; i++ {
		if inFast(c, StyleDollar, "Q", last) {
			return // converged
		}
		c.Get("Q", frags, last)
	}
	if !inFast(c, StyleDollar, "Q", last) {
		t.Fatalf("after churn stopped, %d hits did not bring the shape onto the lock-free path (cap %d)",
			capacity+1, capacity)
	}
}
