package runtime

import (
	"errors"
	"runtime/debug"
	"testing"
)

func chain(depth int) Tree {
	t := NewLeaf(0, int64(1))
	for range depth {
		t = And(t, NewLeaf(0, int64(1)))
	}
	return t
}

var treeFrags = []Frag{{Kind: FilterTree, Cases: []Case{{Text: "t.a = :p", ParamIdx: []int16{0}}}}}

// The caps must bound the WORK, not merely the outcome. GetTree used to
// encode the tree for its cache key before anything validated it, so a
// tree far past MaxDepth was walked in full — and the encoder's
// recursion is unbounded, which is a fatal stack overflow rather than a
// recoverable error. @filter-tree values are built by the caller from
// request data, so their depth is not the program's to assume.
func TestTreeCapsBoundEncodingWork(t *testing.T) {
	// A stack small enough that an unbounded walk cannot survive it, but
	// large enough for ordinary composition.
	defer debug.SetMaxStack(debug.SetMaxStack(1 << 20))

	c := NewComposedCache(8)
	_, _, err := c.GetTree("Q", treeFrags, ShapeKey{}, chain(200000), DefaultTreeCaps)
	if !errors.Is(err, ErrTreeTooLarge) {
		t.Fatalf("want ErrTreeTooLarge, got %v", err)
	}

	// The same through the uncached entry point.
	if _, _, err := ComposeTree(treeFrags, ShapeKey{}, chain(200000), DefaultTreeCaps); !errors.Is(err, ErrTreeTooLarge) {
		t.Fatalf("ComposeTree: want ErrTreeTooLarge, got %v", err)
	}

	// A tree within the caps still composes, and still keys the cache by
	// its structure: two different trees must not share an entry.
	small := And(NewLeaf(0, int64(1)), NewLeaf(0, int64(2)))
	sqlSmall, _, err := c.GetTree("Q", treeFrags, ShapeKey{}, small, DefaultTreeCaps)
	if err != nil {
		t.Fatalf("small tree: %v", err)
	}
	sqlOne, _, err := c.GetTree("Q", treeFrags, ShapeKey{}, NewLeaf(0, int64(1)), DefaultTreeCaps)
	if err != nil {
		t.Fatalf("single leaf: %v", err)
	}
	if sqlSmall == sqlOne {
		t.Errorf("distinct trees composed identically:\n%s", sqlSmall)
	}
}

// Tree bind indices are int16, and treeArgBase accumulates one per
// predicate argument across the whole tree. Caps are configurable with
// no upper bound of their own, so the accumulation is what must be
// checked: past 32767 args the base wraps negative and ResolveArgs
// indexes out of range.
func TestTreeArgBudget(t *testing.T) {
	// Two args per leaf so the budget is reached well before MaxNodes.
	frags := []Frag{{Kind: FilterTree, Cases: []Case{
		{Text: "t.a = :p AND t.b = :q", ParamIdx: []int16{0, 1}},
	}}}
	leaves := MaxTreeArgs/2 + 1
	wide := NewLeaf(0, int64(1), int64(2))
	for range leaves - 1 {
		wide = And(wide, NewLeaf(0, int64(1), int64(2)))
	}
	caps := TreeCaps{MaxNodes: 1 << 20, MaxDepth: 1 << 20}
	if _, _, err := ComposeTree(frags, ShapeKey{}, wide, caps); !errors.Is(err, ErrTreeTooLarge) {
		t.Fatalf("a tree over the arg budget must be ErrTreeTooLarge, got %v", err)
	}
}
