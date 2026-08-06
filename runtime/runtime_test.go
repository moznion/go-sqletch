package runtime

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// A hand-built fragment table mirroring a small template:
//
//	SELECT id FROM t
//	[guard 0] AND (t.a = :a)
//	[guard 1] AND (t.b = :b AND t.c = :a)   -- :a shared across frags
//	[choose]  ORDER BY ... / default
//	LIMIT :limit
func testFrags() []Frag {
	return []Frag{
		{Kind: Skel, Text: "SELECT id FROM t WHERE TRUE"},
		{Kind: Guarded, GuardMask: 1 << 0, Sep: SepAnd,
			Text: "t.a = :a", ParamSpans: []Span{{6, 8}}, ParamIdx: []int16{0}},
		{Kind: Guarded, GuardMask: 1 << 1, Sep: SepAnd,
			Text: "t.b = :b AND t.c = :a", ParamSpans: []Span{{6, 8}, {19, 21}}, ParamIdx: []int16{1, 0}},
		{Kind: Choose, Cases: []Case{
			{Text: "ORDER BY t.x"},
			{Text: "ORDER BY t.y"},
			{Text: "ORDER BY t.id"}, // default
		}},
		{Kind: Skel, Text: "\nLIMIT :limit", ParamSpans: []Span{{7, 13}}, ParamIdx: []int16{2}},
	}
}

func TestCompose_Shapes(t *testing.T) {
	frags := testFrags()

	sql, argIdx := Compose(frags, ShapeKey{Guards: 0, Choices: []uint8{2}})
	if strings.Contains(sql, "t.a") || strings.Contains(sql, "t.b") {
		t.Errorf("inactive fragments leaked:\n%s", sql)
	}
	if !strings.Contains(sql, "ORDER BY t.id") || !strings.Contains(sql, "LIMIT $1") {
		t.Errorf("minimal shape:\n%s", sql)
	}
	if len(argIdx) != 1 || argIdx[0] != 2 {
		t.Errorf("argIdx = %v, want [2]", argIdx)
	}

	sql, argIdx = Compose(frags, ShapeKey{Guards: 0b11, Choices: []uint8{0}})
	for _, want := range []string{"AND (t.a = $1)", "AND (t.b = $2 AND t.c = $1)", "ORDER BY t.x", "LIMIT $3"} {
		if !strings.Contains(sql, want) {
			t.Errorf("maximal shape missing %q:\n%s", want, sql)
		}
	}
	if len(argIdx) != 3 || argIdx[0] != 0 || argIdx[1] != 1 || argIdx[2] != 2 {
		t.Errorf("argIdx = %v, want [0 1 2]", argIdx)
	}

	// Guard 1 only: :b gets $1 (first occurrence), shared :a gets $2.
	sql, argIdx = Compose(frags, ShapeKey{Guards: 0b10, Choices: []uint8{1}})
	if !strings.Contains(sql, "AND (t.b = $1 AND t.c = $2)") || !strings.Contains(sql, "LIMIT $3") {
		t.Errorf("renumbering per shape:\n%s", sql)
	}
	if len(argIdx) != 3 || argIdx[0] != 1 || argIdx[1] != 0 {
		t.Errorf("argIdx = %v, want [1 0 2]", argIdx)
	}
}

func TestComposeStyle_Question(t *testing.T) {
	frags := testFrags()

	// Question style: one '?' per occurrence; the shared :a binds twice.
	sql, argIdx := ComposeStyle(StyleQuestion, frags, ShapeKey{Guards: 0b11, Choices: []uint8{0}})
	if strings.Contains(sql, "$") {
		t.Errorf("dollar placeholder leaked into question style:\n%s", sql)
	}
	for _, want := range []string{"AND (t.a = ?)", "AND (t.b = ? AND t.c = ?)", "ORDER BY t.x", "LIMIT ?"} {
		if !strings.Contains(sql, want) {
			t.Errorf("question-style shape missing %q:\n%s", want, sql)
		}
	}
	want := []int16{0, 1, 0, 2}
	if len(argIdx) != len(want) {
		t.Fatalf("argIdx = %v, want %v", argIdx, want)
	}
	for i := range want {
		if argIdx[i] != want[i] {
			t.Fatalf("argIdx = %v, want %v", argIdx, want)
		}
	}

	// The cache path composes with the same style.
	c := NewComposedCache(8)
	cSQL, cIdx := c.GetStyle(StyleQuestion, "Q", frags, ShapeKey{Guards: 0b11, Choices: []uint8{0}})
	if cSQL != sql || len(cIdx) != len(argIdx) {
		t.Errorf("cache composed differently:\n%s\nvs\n%s", cSQL, sql)
	}
	// Warm hit returns the identical plan.
	cSQL2, _ := c.GetStyle(StyleQuestion, "Q", frags, ShapeKey{Guards: 0b11, Choices: []uint8{0}})
	if cSQL2 != cSQL {
		t.Error("cache hit differs from miss")
	}
}

func TestComposeStyle_InList(t *testing.T) {
	frags := []Frag{
		{Kind: Skel, Text: "SELECT id FROM t WHERE t.status "},
		{Kind: InList, ParamIdx: []int16{0}},
		{Kind: Skel, Text: "\nLIMIT :limit", ParamSpans: []Span{{7, 13}}, ParamIdx: []int16{1}},
	}

	sql, binds, err := ComposeTreeStyle(StyleQuestion, frags, ShapeKey{Arities: []int32{3}}, Tree{}, DefaultTreeCaps)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "t.status IN (?, ?, ?)") {
		t.Errorf("arity-3 expansion:\n%s", sql)
	}
	want := []Bind{{Idx: 0, Elem: 1}, {Idx: 0, Elem: 2}, {Idx: 0, Elem: 3}, {Idx: 1}}
	if len(binds) != len(want) {
		t.Fatalf("binds = %+v", binds)
	}
	for i := range want {
		if binds[i] != want[i] {
			t.Fatalf("binds = %+v, want %+v", binds, want)
		}
	}
	args := ResolveArgs(binds, []any{[]string{"a", "b", "c"}, int64(10)}, nil)
	if len(args) != 4 || args[0] != "a" || args[2] != "c" || args[3] != int64(10) {
		t.Errorf("args = %v", args)
	}

	// Arity 0: the empty list matches nothing, binds nothing.
	sql, binds, err = ComposeTreeStyle(StyleQuestion, frags, ShapeKey{Arities: []int32{0}}, Tree{}, DefaultTreeCaps)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "t.status IN (SELECT NULL FROM DUAL WHERE FALSE)") {
		t.Errorf("arity-0 rendering:\n%s", sql)
	}
	if len(binds) != 1 || binds[0].Idx != 1 {
		t.Errorf("arity-0 binds = %+v", binds)
	}

	// Distinct arities are distinct cache keys.
	if (ShapeKey{Arities: []int32{3}}).String() == (ShapeKey{Arities: []int32{2}}).String() {
		t.Error("arity must be part of the canonical key encoding")
	}
	if got := (ShapeKey{Guards: 1, Arities: []int32{3}}).String(); got != "g=1;n=3" {
		t.Errorf("canonical encoding = %q", got)
	}
}

func TestCompose_Deterministic(t *testing.T) {
	frags := testFrags()
	k := ShapeKey{Guards: 1, Choices: []uint8{1}}
	a, _ := Compose(frags, k)
	b, _ := Compose(frags, k)
	if a != b {
		t.Fatal("composition must be byte-deterministic")
	}
}

func TestChooseOrdinal(t *testing.T) {
	tests := []struct {
		v, numNamed int
		hasDefault  bool
		want        uint8
		err         bool
	}{
		{0, 3, true, 3, false},  // zero selects the default (last ordinal)
		{1, 3, true, 0, false},  // first named case
		{3, 3, true, 2, false},  // last named case
		{4, 3, true, 0, true},   // out of range
		{0, 3, false, 0, true},  // required: zero value is an error
		{2, 3, false, 1, false}, // required: valid
	}
	for _, tt := range tests {
		got, err := ChooseOrdinal(tt.v, tt.numNamed, tt.hasDefault)
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("ChooseOrdinal(%d,%d,%v) = (%d,%v), want (%d,err=%v)",
				tt.v, tt.numNamed, tt.hasDefault, got, err, tt.want, tt.err)
		}
	}
	if _, err := ChooseOrdinal(0, 2, false); !errors.Is(err, ErrChooseRequired) {
		t.Errorf("zero without default must be ErrChooseRequired, got %v", err)
	}
}

func TestBuildArgs(t *testing.T) {
	v := "x"
	vals := []any{&v, int64(7), nil}
	args := BuildArgs([]int16{1, 0}, vals)
	if len(args) != 2 || args[0] != int64(7) || args[1] != &v {
		t.Errorf("args = %+v", args)
	}
	if BuildArgs(nil, vals) != nil {
		t.Error("empty argIdx must yield nil args")
	}
}

func TestComposedCache_HitAndEvict(t *testing.T) {
	frags := testFrags()
	c := NewComposedCache(2)

	k1 := ShapeKey{Guards: 1, Choices: []uint8{0}}
	s1, _ := c.Get("Q", frags, k1)
	s1b, _ := c.Get("Q", frags, k1)
	if s1 != s1b {
		t.Fatal("cache hit must return identical SQL")
	}

	// Fill beyond capacity; the oldest entry is evicted but results
	// stay correct.
	c.Get("Q", frags, ShapeKey{Guards: 2, Choices: []uint8{0}})
	c.Get("Q", frags, ShapeKey{Guards: 3, Choices: []uint8{0}})
	s1c, _ := c.Get("Q", frags, k1) // recomputed after eviction
	if s1c != s1 {
		t.Fatal("recomputed SQL must equal the original")
	}
}

// TestComposedCache_BoundedUnderChurn pins the capacity bound that
// survives the move from exact LRU to second-chance eviction: recency
// is approximate, the bound is not.
func TestComposedCache_BoundedUnderChurn(t *testing.T) {
	frags := testFrags()
	const capacity = 4
	c := NewComposedCache(capacity)

	for g := uint64(0); g < 64; g++ {
		key := ShapeKey{Guards: g, Choices: []uint8{0}}
		got, _ := c.Get("Q", frags, key)
		want, _ := Compose(frags, key)
		if got != want {
			t.Fatalf("guards=%d: cached SQL diverged from a fresh composition:\ngot  %q\nwant %q", g, got, want)
		}
		c.mu.Lock()
		n, listLen := len(c.m), c.order.Len()
		c.mu.Unlock()
		if n > capacity {
			t.Fatalf("guards=%d: cache holds %d entries, capacity %d", g, n, capacity)
		}
		if n != listLen {
			t.Fatalf("guards=%d: map has %d entries but recency list has %d", g, n, listLen)
		}
	}
}

// TestComposedCache_Concurrent exercises the lock-free read path: hits
// are served from an atomically published snapshot while other
// goroutines insert and evict. Run under -race, this is what guards the
// unsynchronized reads.
func TestComposedCache_Concurrent(t *testing.T) {
	frags := testFrags()
	c := NewComposedCache(8)

	// Precompute expectations serially so the goroutines compare against
	// a fixed reference rather than against each other.
	const shapes = 32
	want := make([]string, shapes)
	for g := range shapes {
		want[g], _ = Compose(frags, ShapeKey{Guards: uint64(g), Choices: []uint8{0}})
	}

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				// Workers walk the shape space out of phase, so hits,
				// misses and evictions interleave.
				g := (i + w*7) % shapes
				sql, argIdx := c.Get("Q", frags, ShapeKey{Guards: uint64(g), Choices: []uint8{0}})
				if sql != want[g] {
					t.Errorf("guards=%d: got %q, want %q", g, sql, want[g])
					return
				}
				if argIdx == nil {
					t.Errorf("guards=%d: nil argIdx", g)
					return
				}
			}
		}()
	}
	wg.Wait()

	c.mu.Lock()
	n := len(c.m)
	c.mu.Unlock()
	if n > 8 {
		t.Fatalf("cache holds %d entries after concurrent churn, capacity 8", n)
	}
}

// TestGetTree_DerivesTreeSegment pins the contract generated code
// relies on: the cache derives the `;t=` key segment itself, so a call
// site that passes a key without Trees lands on the same entry as one
// that fills it in. That is what lets generated code stop encoding the
// tree a second time for the OnQuery hook — and the hook's own
// derivation must still spell out the identical key.
func TestGetTree_DerivesTreeSegment(t *testing.T) {
	frags := []Frag{
		{Kind: Skel, Text: "SELECT id FROM t WHERE TRUE AND "},
		{Kind: FilterTree, Cases: []Case{
			{Text: "t.tenant_id = :tenant", ParamSpans: []Span{{14, 21}}, ParamIdx: []int16{0}},
			{Text: "t.status = :status", ParamSpans: []Span{{11, 18}}, ParamIdx: []int16{0}},
		}},
	}
	tree := And(NewLeaf(0, int64(7)), NewLeaf(1, "active"))
	c := NewComposedCache(8)

	// What generated code now passes: no Trees on the key.
	sqlDerived, _, err := c.GetTree("Q", frags, ShapeKey{}, tree, DefaultTreeCaps)
	if err != nil {
		t.Fatal(err)
	}
	// What a caller filling the segment in itself would pass.
	sqlExplicit, _, err := c.GetTree("Q", frags, ShapeKey{Trees: []string{tree.Encode()}}, tree, DefaultTreeCaps)
	if err != nil {
		t.Fatal(err)
	}
	if sqlDerived != sqlExplicit {
		t.Fatalf("derived and explicit tree keys composed differently:\n%q\nvs\n%q", sqlDerived, sqlExplicit)
	}

	c.mu.Lock()
	n := len(c.m)
	var stored ShapeKey
	for _, e := range c.m {
		stored = e.key
	}
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("the two forms must key to one entry, got %d", n)
	}

	// The key the hook spells out (hookTree's derivation) must equal the
	// one the cache stored, `;t=` segment included.
	hookKey := ShapeKey{Trees: []string{tree.Encode()}}
	if got := hookKey.String(); got != stored.String() {
		t.Errorf("hook key %q != cached key %q", got, stored.String())
	}
	if !strings.Contains(hookKey.String(), ";t=") {
		t.Errorf("hook key lost the tree segment: %q", hookKey.String())
	}
}

// TestComposedCache_FullKeyOnHit pins that entries are matched on the
// full key, never on its string encoding alone (the encoding is an
// index). A forged entry under a colliding map key must be rejected and
// recomputed rather than served.
func TestComposedCache_FullKeyOnHit(t *testing.T) {
	frags := testFrags()
	c := NewComposedCache(8)

	key := ShapeKey{Guards: 1, Choices: []uint8{0}}
	want, _ := Compose(frags, key)

	// Publish an entry whose stored key disagrees with the one callers
	// will ask for, under the map key the caller's request derives.
	mapKey := "Q|" + key.String()
	bogus := newCacheEntry(mapKey, ShapeKey{Guards: 999, Choices: []uint8{0}}, "SELECT 'wrong'", nil)
	c.mu.Lock()
	bogus.el = c.order.PushFront(bogus)
	c.m[mapKey] = bogus
	c.publish()
	c.mu.Unlock()

	if got, _ := c.Get("Q", frags, key); got != want {
		t.Fatalf("full-key mismatch served from cache: got %q, want %q", got, want)
	}
}

func TestShapeKeyString_Canonical(t *testing.T) {
	k := ShapeKey{Guards: 0x2a, Choices: []uint8{1, 0}}
	if got := k.String(); got != "g=2a;c=1,0" {
		t.Errorf("String() = %q", got)
	}
	if got := (ShapeKey{Guards: 5}).String(); got != "g=5" {
		t.Errorf("String() = %q", got)
	}
}
