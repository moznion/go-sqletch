package runtime

import (
	"slices"
	"testing"
)

// "Hashes are an index, never identity": the composition cache indexes
// entries by a canonical string encoding of the shape key, but serves
// an entry only after keysEqual proves the FULL key identical. These
// tests pin both halves — that keysEqual distinguishes every key
// dimension, and that an entry found under a caller's map key whose
// full key differs (an encoding collision, which would be a bug in the
// encoder) is dropped and recomposed rather than served.

func identityBaseKey() ShapeKey {
	return ShapeKey{
		Guards:  5,
		Choices: []uint8{1, 2},
		Orders:  [][]uint8{{2, 3}, nil, {}},
		Trees:   []string{"a", "b"},
		Arities: []int32{1, 0},
	}
}

func TestKeysEqual_EveryDimensionDistinguishes(t *testing.T) {
	base := identityBaseKey()
	if !keysEqual(base, identityBaseKey()) {
		t.Fatal("identical keys compare unequal")
	}

	variants := []struct {
		name   string
		mutate func(*ShapeKey)
	}{
		{"guards", func(k *ShapeKey) { k.Guards = 4 }},
		{"choice value", func(k *ShapeKey) { k.Choices[1] = 3 }},
		{"choice count", func(k *ShapeKey) { k.Choices = k.Choices[:1] }},
		{"order element", func(k *ShapeKey) { k.Orders[0][1] = 5 }},
		{"order sequence length", func(k *ShapeKey) { k.Orders[0] = k.Orders[0][:1] }},
		// nil (maximal: verification only) and empty (default-or-omit)
		// are different shapes and must never alias.
		{"order nil vs empty", func(k *ShapeKey) { k.Orders[1] = []uint8{} }},
		{"order empty vs nil", func(k *ShapeKey) { k.Orders[2] = nil }},
		{"order block count", func(k *ShapeKey) { k.Orders = k.Orders[:2] }},
		{"tree encoding", func(k *ShapeKey) { k.Trees[1] = "c" }},
		{"tree count", func(k *ShapeKey) { k.Trees = k.Trees[:1] }},
		{"arity value", func(k *ShapeKey) { k.Arities[1] = 2 }},
		{"arity count", func(k *ShapeKey) { k.Arities = k.Arities[:1] }},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			k := identityBaseKey()
			v.mutate(&k)
			if keysEqual(base, k) || keysEqual(k, base) {
				t.Fatalf("keysEqual cannot tell %+v from %+v", k, base)
			}
		})
	}
}

// cloneKey is what an entry retains; it must preserve the nil/empty
// distinction keysEqual relies on, and must not alias the caller's
// slices (generated code reuses its key buffers).
func TestCloneKey_PreservesShapeAndDoesNotAlias(t *testing.T) {
	src := identityBaseKey()
	c := cloneKey(src)
	if !keysEqual(src, c) {
		t.Fatalf("clone %+v differs from source %+v", c, src)
	}
	if c.Orders[1] != nil || c.Orders[2] == nil {
		t.Fatalf("clone lost the nil/empty order distinction: %#v", c.Orders)
	}
	src.Choices[0] = 9
	src.Orders[0][0] = 9
	src.Trees[0] = "z"
	src.Arities[0] = 9
	if !keysEqual(c, identityBaseKey()) {
		t.Fatalf("clone aliases the source: %+v", c)
	}
}

// plantCollision files an entry composed for `other` under the map key
// `victim` encodes — the state an encoder collision would produce.
// White-box: it mirrors entry()'s key construction and the insert
// path's accounting so remove() keeps the counters consistent.
func plantCollision(c *ComposedCache, style Style, query string, frags []Frag, victim, other ShapeKey) {
	var buf [keyBufSize]byte
	mk := append(buf[:0], '0'+byte(style), '|')
	mk = append(mk, query...)
	mk = append(mk, '|')
	mk = victim.appendTo(mk)

	sql, binds, err := ComposeTreeStyle(style, frags, other, Tree{}, DefaultTreeCaps)
	if err != nil {
		panic(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := newCacheEntry(string(mk), query, other, sql, binds)
	e.el = c.order.PushFront(e)
	c.m[string(mk)] = e
	c.sqlBytes += int64(len(sql))
	c.totalBytes += entryBytes(e)
}

func TestCache_CollidingEntryIsNeverServed(t *testing.T) {
	frags := benchFrags()
	victim := ShapeKey{Guards: 0x1, Choices: []uint8{0}}
	other := ShapeKey{Guards: 0x2, Choices: []uint8{0}}
	wantSQL, wantIdx := ComposeStyle(StyleDollar, frags, victim)
	foreignSQL, _ := ComposeStyle(StyleDollar, frags, other)
	if wantSQL == foreignSQL {
		t.Fatal("fixture: the two shapes must compose differently")
	}

	// Both read paths consult keysEqual: the lock-free snapshot (the
	// collision is published) and the mutex path (it is not).
	for _, published := range []bool{true, false} {
		name := "mutex path"
		if published {
			name = "lock-free snapshot"
		}
		t.Run(name, func(t *testing.T) {
			c := NewComposedCache(16)
			c.mu.Lock()
			c.publishNow() // an empty snapshot, so the unpublished plant is off the fast path
			c.mu.Unlock()
			plantCollision(c, StyleDollar, "Q", frags, victim, other)
			if published {
				c.mu.Lock()
				c.publishNow()
				c.mu.Unlock()
			}

			gotSQL, gotIdx := c.Get("Q", frags, victim)
			if gotSQL != wantSQL || !slices.Equal(gotIdx, wantIdx) {
				t.Fatalf("served the colliding entry:\n got %q %v\nwant %q %v", gotSQL, gotIdx, wantSQL, wantIdx)
			}
			// The foreign entry is gone and its slot now holds the
			// victim's own composition, with the counters consistent.
			c.mu.Lock()
			defer c.mu.Unlock()
			if len(c.m) != 1 {
				t.Fatalf("cache holds %d entries, want 1 (the recomposed victim)", len(c.m))
			}
			for _, e := range c.m {
				if !keysEqual(e.key, victim) || e.sql != wantSQL {
					t.Fatalf("resident entry is %+v / %q, want the victim's", e.key, e.sql)
				}
				if c.sqlBytes != int64(len(e.sql)) || c.totalBytes != entryBytes(e) {
					t.Fatalf("byte accounting drifted: sql=%d total=%d, want %d/%d",
						c.sqlBytes, c.totalBytes, len(e.sql), entryBytes(e))
				}
			}
		})
	}
}

// Regression: cloneKey used to flatten an EMPTY order sequence (the
// default-or-omit shape generated code passes when the caller picks no
// sort keys — OrderSeq returns a non-nil empty slice) into nil (the
// maximal shape). The retained key then never matched its own caller's,
// so every call of a default-order shape missed: mutex, recompose,
// re-insert, and the observer saw the maximal key instead.
func TestCache_DefaultOrderShapeHits(t *testing.T) {
	frags := []Frag{
		{Kind: Skel, Text: "SELECT 1 FROM t\n"},
		{Kind: OrderBy, Cases: []Case{{Text: "t.a"}}, Default: &Case{Text: "ORDER BY t.id"}},
	}
	seq, err := OrderSeq([]int{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	c := NewComposedCache(16)
	for range 5 {
		c.Get("Q", frags, ShapeKey{Orders: [][]uint8{seq}})
	}
	if c.misses != 1 || c.inserts != 1 {
		t.Fatalf("default-order shape: misses=%d inserts=%d over 5 calls, want 1/1", c.misses, c.inserts)
	}
	if !inFast(c, StyleDollar, "Q", ShapeKey{Orders: [][]uint8{seq}}) {
		t.Fatal("default-order shape is not served from the lock-free snapshot")
	}
}
