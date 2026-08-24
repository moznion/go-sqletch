package runtime

import (
	"errors"
	"strings"
	"testing"
)

// arityFrags is a three-predicate vocabulary whose middle predicate
// takes two arguments — the shape needed to demonstrate cross-leaf
// desynchronization. Spans/indices follow BuildFrags' emission: a
// predicate's ParamIdx values index the LEAF's own argument list.
func arityFrags() []Frag {
	return []Frag{
		{Kind: Skel, Text: "SELECT id FROM t WHERE TRUE AND "},
		{Kind: FilterTree, Cases: []Case{
			{Text: "a = :one", ParamSpans: []Span{{Start: 4, End: 8}}, ParamIdx: []int16{0}},
			{Text: "b = :p AND c = :q",
				ParamSpans: []Span{{Start: 4, End: 6}, {Start: 15, End: 17}},
				ParamIdx:   []int16{0, 1}},
			{Text: "d = :two", ParamSpans: []Span{{Start: 4, End: 8}}, ParamIdx: []int16{0}},
		}},
	}
}

// TestTreeLeafUnderSupplied_MiddleLeaf is repro A: a middle leaf
// carrying fewer arguments than its predicate consumes used to compose
// SILENTLY — the composer advances the flattened TreeArgs base by the
// SUPPLIED count, so predicate "b/c"'s second parameter and the next
// leaf's parameter collapsed onto the same TreeArgs index:
//
//	((a = $1 AND b = $2) AND (c = $2))   // c receives the NEXT leaf's value
//
// In a tenant-scoped tree that substitutes one scope value for another
// with no error anywhere. It must be rejected before any SQL is built.
func TestTreeLeafUnderSupplied_MiddleLeaf(t *testing.T) {
	frags := arityFrags()
	tree := And(
		NewLeaf(0, int64(1)),
		NewLeaf(1, "only-one-of-two"), // predicate 1 takes 2 args
		NewLeaf(2, int64(3)),
	)

	sql, binds, err := ComposeTree(frags, ShapeKey{}, tree, DefaultTreeCaps)
	if !errors.Is(err, ErrTreeArity) {
		t.Fatalf("under-supplied middle leaf: got err %v, want ErrTreeArity", err)
	}
	if sql != "" || binds != nil {
		t.Fatalf("under-supplied leaf composed SQL anyway: %q / %v", sql, binds)
	}
	// The message must identify the predicate and both counts so the
	// mis-constructed call site is findable.
	for _, want := range []string{"predicate 1", "2 argument", "got 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestTreeLeafUnderSupplied_LastLeaf is repro B: when the LAST leaf is
// under-supplied there is no next leaf to steal from, so the bind plan
// used to reference a TreeArgs index past the flattened slice and
// ResolveArgs panicked with an index-out-of-range at QUERY time. The
// mismatch must instead fail composition loudly.
func TestTreeLeafUnderSupplied_LastLeaf(t *testing.T) {
	frags := arityFrags()
	tree := And(NewLeaf(0, int64(1)), NewLeaf(1, "only-one-of-two"))

	sql, binds, err := ComposeTree(frags, ShapeKey{}, tree, DefaultTreeCaps)
	if !errors.Is(err, ErrTreeArity) {
		t.Fatalf("under-supplied last leaf: got err %v, want ErrTreeArity", err)
	}
	if sql != "" || binds != nil {
		t.Fatalf("under-supplied leaf composed SQL anyway: %q / %v", sql, binds)
	}

	// The panic scenario end to end: had composition succeeded, this
	// ResolveArgs call would have indexed past TreeArgs. With the
	// rejection there is no plan to resolve at all — pinned here so the
	// query-time crash cannot come back without this test noticing.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ResolveArgs panicked: %v", r)
		}
	}()
	_ = ResolveArgs(binds, nil, TreeArgs(tree))
}

// TestTreeLeafOverSupplied pins the deliberate STRICT choice: an
// over-supplied leaf keeps the base accounting aligned (the surplus is
// simply never bound), but it cannot come from a generated constructor
// — it means the caller's arguments do not correspond to the
// predicate's parameters (typically a wrong predicate index that
// happened to pass the range check), and the silently dropped values
// may be exactly the scope the caller believes is enforced. Any
// mismatch is rejected.
func TestTreeLeafOverSupplied(t *testing.T) {
	frags := arityFrags()
	tree := NewLeaf(0, int64(1), int64(2)) // predicate 0 takes 1 arg

	_, _, err := ComposeTree(frags, ShapeKey{}, tree, DefaultTreeCaps)
	if !errors.Is(err, ErrTreeArity) {
		t.Fatalf("over-supplied leaf: got err %v, want ErrTreeArity", err)
	}
}

// TestTreeLeafZeroParamPredicate: a predicate with no parameters wants
// exactly zero arguments — none is accepted, any is rejected.
func TestTreeLeafZeroParamPredicate(t *testing.T) {
	frags := []Frag{
		{Kind: Skel, Text: "SELECT id FROM t WHERE TRUE AND "},
		{Kind: FilterTree, Cases: []Case{{Text: "t.deleted_at IS NULL"}}},
	}
	if _, _, err := ComposeTree(frags, ShapeKey{}, NewLeaf(0), DefaultTreeCaps); err != nil {
		t.Fatalf("zero-param leaf with no args must compose: %v", err)
	}
	_, _, err := ComposeTree(frags, ShapeKey{}, NewLeaf(0, int64(7)), DefaultTreeCaps)
	if !errors.Is(err, ErrTreeArity) {
		t.Fatalf("zero-param leaf with an arg: got err %v, want ErrTreeArity", err)
	}
}

// TestTreeLeafArity_CachePath: the cached entry point rejects the same
// way (before any entry churn), and a well-formed tree still composes
// afterwards — the rejection leaves the cache usable.
func TestTreeLeafArity_CachePath(t *testing.T) {
	frags := arityFrags()
	c := NewComposedCache(8)

	bad := And(NewLeaf(0, int64(1)), NewLeaf(1, "only-one-of-two"))
	if _, _, err := c.GetTree("Q", frags, ShapeKey{}, bad, DefaultTreeCaps); !errors.Is(err, ErrTreeArity) {
		t.Fatalf("GetTree with under-supplied leaf: got err %v, want ErrTreeArity", err)
	}
	if s := c.Stats(); s.Entries != 0 {
		t.Fatalf("a rejected tree left %d cache entries", s.Entries)
	}

	good := And(NewLeaf(0, int64(1)), NewLeaf(1, "p", "q"), NewLeaf(2, int64(3)))
	sql, binds, err := c.GetTree("Q", frags, ShapeKey{}, good, DefaultTreeCaps)
	if err != nil {
		t.Fatalf("well-formed tree after a rejection: %v", err)
	}
	if want := "SELECT id FROM t WHERE TRUE AND ((a = $1) AND (b = $2 AND c = $3) AND (d = $4))"; sql != want {
		t.Fatalf("composed SQL:\n got %q\nwant %q", sql, want)
	}
	args := ResolveArgs(binds, nil, TreeArgs(good))
	want := []any{int64(1), "p", "q", int64(3)}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %v, want %v", i, args[i], want[i])
		}
	}
}

// TestTreeLeafArity_CachePath_GoodThenBad is the regression for audit
// finding M1: per-leaf arity used to be checked ONLY on the cache-miss
// compose path, while the cache key EXCLUDES per-leaf argument counts.
// So once a correct-arity tree of a given STRUCTURE is warm, a
// same-structure wrong-arity tree hit the cache and was served the
// cached bind plan without validate ever running — a query-time panic
// (under-supply) or a silent cross-leaf mis-bind (compensating
// over/under-supply). Both variants below MUST be rejected with
// ErrTreeArity, never a cached plan and never a panic.
func TestTreeLeafArity_CachePath_GoodThenBad(t *testing.T) {
	frags := arityFrags()

	// Warm the cache with a well-formed tree so a structurally identical
	// malformed one would find its entry.
	warm := func(c *ComposedCache) {
		good := And(NewLeaf(0, int64(1)), NewLeaf(1, "p", "q"), NewLeaf(2, int64(3)))
		if _, _, err := c.GetTree("Q", frags, ShapeKey{}, good, DefaultTreeCaps); err != nil {
			t.Fatalf("warming with a well-formed tree: %v", err)
		}
		if s := c.Stats(); s.Entries != 1 {
			t.Fatalf("warm-up left %d cache entries, want 1", s.Entries)
		}
	}

	t.Run("under-supply", func(t *testing.T) {
		c := NewComposedCache(8)
		warm(c)
		// Same structure (And of three leaves 0,1,2) but predicate 1 is
		// under-supplied: one argument instead of two.
		bad := And(NewLeaf(0, int64(9)), NewLeaf(1, "only-one"), NewLeaf(2, int64(11)))
		sql, binds, err := c.GetTree("Q", frags, ShapeKey{}, bad, DefaultTreeCaps)
		if !errors.Is(err, ErrTreeArity) {
			t.Fatalf("good-then-bad under-supply on cache path: got err %v, want ErrTreeArity", err)
		}
		if sql != "" || binds != nil {
			t.Fatalf("rejected tree still returned a cached plan: %q / %v", sql, binds)
		}
	})

	t.Run("compensating-over-under-supply", func(t *testing.T) {
		c := NewComposedCache(8)
		warm(c)
		// Total argument count stays 4 (1 + 3 + 0) as in the good tree,
		// so a mere total-count check would NOT catch it — predicate 1 is
		// over-supplied by one and predicate 2 is under-supplied by one.
		bad := And(
			NewLeaf(0, int64(9)),
			NewLeaf(1, "p", "q", "SURPLUS"),
			NewLeaf(2), // takes 1, given 0
		)
		sql, binds, err := c.GetTree("Q", frags, ShapeKey{}, bad, DefaultTreeCaps)
		if !errors.Is(err, ErrTreeArity) {
			t.Fatalf("good-then-bad compensating supply on cache path: got err %v, want ErrTreeArity", err)
		}
		if sql != "" || binds != nil {
			t.Fatalf("rejected tree still returned a cached plan: %q / %v", sql, binds)
		}
	})
}

// TestTreeLeafArity_CachePath_MisBind is the concrete tenant mis-bind
// from finding M1: predicate0 takes 1 arg, predicate1 takes 2. Warming
// with And(leaf0("X"), leaf1("Y","Z")) then serving a same-structure
// And(leaf0(), leaf1("TENANT_B","C","SURPLUS")) — total count 3 either
// way — previously returned SQL plus a mis-ordered bind plan that bound
// a neighboring leaf's value to the wrong predicate. It must be
// rejected before any SQL is served.
func TestTreeLeafArity_CachePath_MisBind(t *testing.T) {
	frags := []Frag{
		{Kind: Skel, Text: "SELECT id FROM t WHERE TRUE AND "},
		{Kind: FilterTree, Cases: []Case{
			{Text: "a = :one", ParamSpans: []Span{{Start: 4, End: 8}}, ParamIdx: []int16{0}},
			{Text: "b = :p AND c = :q",
				ParamSpans: []Span{{Start: 4, End: 6}, {Start: 15, End: 17}},
				ParamIdx:   []int16{0, 1}},
		}},
	}
	c := NewComposedCache(8)

	good := And(NewLeaf(0, "X"), NewLeaf(1, "Y", "Z"))
	if _, _, err := c.GetTree("Q", frags, ShapeKey{}, good, DefaultTreeCaps); err != nil {
		t.Fatalf("warming with a well-formed tree: %v", err)
	}

	// leaf0 under-supplied (0 args), leaf1 over-supplied (3 args): same
	// structure, same total count (3), previously a silent tenant leak.
	bad := And(NewLeaf(0), NewLeaf(1, "TENANT_B", "C", "SURPLUS"))
	sql, binds, err := c.GetTree("Q", frags, ShapeKey{}, bad, DefaultTreeCaps)
	if !errors.Is(err, ErrTreeArity) {
		t.Fatalf("mis-bind tree on cache path: got err %v, want ErrTreeArity", err)
	}
	if sql != "" || binds != nil {
		t.Fatalf("mis-bind tree still returned SQL/binds: %q / %v", sql, binds)
	}
}
