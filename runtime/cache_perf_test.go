package runtime

import (
	"strings"
	"testing"
)

// inFast reports whether (query, key) is resolvable from the lock-free
// published snapshot — i.e. a hit on it takes no lock. White-box: it
// rebuilds the internal map key exactly as entry() does.
func inFast(c *ComposedCache, style Style, query string, key ShapeKey) bool {
	var buf [keyBufSize]byte
	mk := append(buf[:0], '0'+byte(style), '|')
	mk = append(mk, query...)
	mk = append(mk, '|')
	mk = key.appendTo(mk)
	snap := c.fast.Load()
	if snap == nil {
		return false
	}
	e, ok := (*snap)[string(mk)]
	return ok && keysEqual(e.key, key)
}

// TestCacheDeferredPublishFlushedByHit pins M2: an entry inserted after
// the amortization guard began deferring publishes must not stay off the
// lock-free path forever. Once inserts stop, the first hit on such an
// entry flushes the snapshot so it — and every co-resident shape — is
// served without the mutex again.
func TestCacheDeferredPublishFlushedByHit(t *testing.T) {
	frags := benchFrags()
	c := NewComposedCache(4)

	keys := make([]ShapeKey, 5)
	for i := range keys {
		keys[i] = ShapeKey{Guards: uint64(i), Choices: []uint8{0}}
	}
	for _, k := range keys {
		c.Get("Q", frags, k)
	}

	// The 5th insert evicted one entry (evictedSince), so its own publish
	// was deferred: it is resident but not in the lock-free snapshot.
	last := keys[4]
	if inFast(c, StyleDollar, "Q", last) {
		t.Fatal("precondition failed: the deferred insert should be absent from the lock-free snapshot")
	}

	// A hit on the unpublished entry must flush it into the snapshot.
	c.Get("Q", frags, last)
	if !inFast(c, StyleDollar, "Q", last) {
		t.Fatal("M2: a hit on an unpublished entry did not flush it into the lock-free snapshot; " +
			"it would stay behind the global mutex indefinitely")
	}
}

// TestCacheByteBoundEvicts pins M3: the cache must bound retained bytes,
// not just entry count, so a caller-controlled large shape (here a large
// composed SQL, standing in for a big @in arity) cannot pin unbounded
// memory behind a modest entry count.
func TestCacheByteBoundEvicts(t *testing.T) {
	const perEntry = 4096
	frags := []Frag{{Kind: Skel, Text: strings.Repeat("x", perEntry)}}

	c := NewComposedCache(1000) // count cap far above what bytes will allow
	const ceiling = 20 * 1024
	c.SetMaxBytes(ceiling)

	for i := 0; i < 50; i++ {
		c.Get("Q", frags, ShapeKey{Guards: uint64(i)})
	}

	c.mu.Lock()
	entries := len(c.m)
	total := c.totalBytes
	c.mu.Unlock()

	if entries >= 50 {
		t.Fatalf("M3: byte bound did not evict — %d entries resident with a %d-byte ceiling", entries, ceiling)
	}
	if total > ceiling {
		t.Fatalf("M3: totalBytes %d exceeds the %d-byte ceiling", total, ceiling)
	}
	if entries == 0 {
		t.Fatal("M3: cache evicted everything; at least the last-composed entry must survive")
	}
}

// TestCacheByteBoundKeepsOversizeEntry pins the one-entry floor: a single
// entry larger than the whole ceiling must still be served (the caller
// just composed it), never evicted into an empty cache.
func TestCacheByteBoundKeepsOversizeEntry(t *testing.T) {
	frags := []Frag{{Kind: Skel, Text: strings.Repeat("y", 8192)}}
	c := NewComposedCache(1000)
	c.SetMaxBytes(1024) // smaller than one entry

	c.Get("Q", frags, ShapeKey{Guards: 1})

	c.mu.Lock()
	entries := len(c.m)
	c.mu.Unlock()
	if entries != 1 {
		t.Fatalf("oversize single entry: want 1 resident, got %d", entries)
	}
}
