package runtime

import (
	"errors"
	"fmt"
	"strconv"
)

// Tree is a runtime-composed boolean combination over a query's closed
// predicate vocabulary. Values are opaque to the composer: they travel
// exclusively as bind parameters. Construct trees only through the
// generated per-query predicate constructors plus And/Or/Unscoped.
//
// Tree is a value type, not a pointer, so that `nil` is not a Tree —
// passing it to a required @filter-tree! argument does not compile:
//
//	cannot use nil as runtime.Tree value in argument to FilterUsers
//
// That matters because `nil` is the shape a forgotten scope takes. The
// only zero Tree that survives compilation is one written out as
// `runtime.Tree{}`, which nobody types by accident; it is still refused
// at runtime with ErrFilterRequired, and Unscoped() remains the way to
// say "no scope" on purpose.
type Tree struct{ n *node }

// node is the internal representation. Keeping it unexported is what
// lets Tree be a value while the structure stays shared and cheap to
// pass around.
type node struct {
	op   uint8 // opLeaf, opAnd, opOr, opTrue
	kids []*node
	pred int16
	args []any
}

const (
	opLeaf uint8 = iota
	opAnd
	opOr
	opTrue
)

// IsZero reports whether no tree was supplied. It distinguishes "the
// caller did not decide" from Unscoped(), which is a decision.
func (t Tree) IsZero() bool { return t.n == nil }

// And combines subtrees conjunctively. And() with no children is TRUE.
func And(ts ...Tree) Tree { return combine(opAnd, ts) }

// Or combines subtrees disjunctively. Or() with no children is TRUE.
func Or(ts ...Tree) Tree { return combine(opOr, ts) }

func combine(op uint8, ts []Tree) Tree {
	kids := make([]*node, 0, len(ts))
	for _, t := range ts {
		// A zero Tree carries no constraint, so it drops out of the
		// combination rather than making the whole thing zero.
		if t.n != nil {
			kids = append(kids, t.n)
		}
	}
	if len(kids) == 0 {
		return Unscoped()
	}
	if len(kids) == 1 {
		return Tree{n: kids[0]}
	}
	return Tree{n: &node{op: op, kids: kids}}
}

// NewLeaf is called by generated predicate constructors; user code
// never calls it directly. args must be exactly the predicate's
// parameters in declaration order — composition rejects any count
// mismatch with [ErrTreeArity] (generated constructors always match).
func NewLeaf(pred int16, args ...any) Tree {
	return Tree{n: &node{op: opLeaf, pred: pred, args: args}}
}

// Unscoped is the explicit, greppable opt-out of a required
// @filter-tree!: it renders as TRUE.
func Unscoped() Tree { return Tree{n: &node{op: opTrue}} }

// TreeCaps bounds adversarially large trees (spec defaults).
type TreeCaps struct {
	MaxNodes int
	MaxDepth int
}

var DefaultTreeCaps = TreeCaps{MaxNodes: 32, MaxDepth: 8}

var (
	// ErrFilterRequired is returned when a @filter-tree! argument is the
	// zero Tree; deliberate unscoped access must use the generated
	// Unscoped constructor.
	ErrFilterRequired = errors.New("sqletch: required @filter-tree argument is the zero Tree (use the generated Unscoped() for deliberate opt-out)")
	ErrTreeTooLarge   = errors.New("sqletch: filter tree exceeds the configured caps")
	ErrTreePredicate  = errors.New("sqletch: filter tree references an unknown predicate")
	// ErrTreeArity is returned when a leaf carries a different number of
	// arguments than its predicate's parameters. The composer flattens
	// leaf arguments into one preorder TreeArgs space and each leaf owns
	// exactly the slice its predicate consumes, so an under-supplied
	// leaf would bind a NEIGHBORING leaf's value (or index out of range
	// at query time) and an over-supplied one would silently drop
	// values; both are construction bugs, rejected before any SQL is
	// built. Generated predicate constructors always match — this guards
	// hand-written NewLeaf calls.
	ErrTreeArity = errors.New("sqletch: filter tree leaf argument count does not match its predicate's parameters")
)

// MaxTreeArgs bounds the predicate arguments one tree may contribute.
// Bind indices are int16 and the composer accumulates one per argument
// across the whole tree, so past this the base wraps negative and
// ResolveArgs indexes out of range. TreeCaps has no upper bound of its
// own — a project may configure MaxNodes freely — so this is checked
// against the value, not the caps.
const MaxTreeArgs = 32767

// checkCaps enforces the size caps alone: node count, depth, and the
// argument budget. It is separate from validate because it must run
// BEFORE anything else walks the tree — the encoder that builds the
// cache key recurses without a depth bound of its own, so an
// unvalidated tree would overflow the stack rather than be rejected.
//
// Every traversal here is bounded: the counters trip within
// caps.MaxNodes nodes and caps.MaxDepth frames.
func (t Tree) checkCaps(caps TreeCaps) error {
	if t.n == nil {
		return nil
	}
	nodes, args := 0, 0
	var rec func(n *node, depth int) error
	rec = func(n *node, depth int) error {
		nodes++
		if nodes > caps.MaxNodes || depth > caps.MaxDepth {
			return fmt.Errorf("%w (max %d nodes, depth %d)", ErrTreeTooLarge, caps.MaxNodes, caps.MaxDepth)
		}
		args += len(n.args)
		if args > MaxTreeArgs {
			return fmt.Errorf("%w (max %d predicate arguments)", ErrTreeTooLarge, MaxTreeArgs)
		}
		for _, k := range n.kids {
			if err := rec(k, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return rec(t.n, 1)
}

// validate checks caps, predicate ranges, and leaf arity against the
// query's predicate table.
func (t Tree) validate(preds []Case, caps TreeCaps) error {
	if err := t.checkCaps(caps); err != nil {
		return err
	}
	if t.n == nil {
		return nil
	}
	var rec func(n *node) error
	rec = func(n *node) error {
		if n.op == opLeaf {
			if n.pred < 0 || int(n.pred) >= len(preds) {
				return fmt.Errorf("%w: %d", ErrTreePredicate, n.pred)
			}
			// The composer offsets a predicate's ParamIdx values into
			// the leaf's own slice of the flattened TreeArgs space and
			// then advances the base by the SUPPLIED count, so any
			// mismatch desynchronizes every leaf after this one:
			// under-supply makes a bind reference a NEIGHBOR's argument
			// (silent wrong-value binding — in a tenant-scoped tree,
			// one scope value substituted for another) or, on the last
			// leaf, an index past TreeArgs (a query-time panic in
			// ResolveArgs). Over-supply keeps the accounting aligned,
			// but the surplus values are silently never bound — the
			// caller's arguments do not correspond to the predicate's
			// parameters, which is the same construction bug — so ANY
			// mismatch is rejected. Generated constructors always pass
			// exactly the predicate's parameters; this guards
			// hand-written NewLeaf calls.
			if want := leafArity(preds[n.pred]); len(n.args) != want {
				return fmt.Errorf("%w: predicate %d takes %d argument(s), got %d",
					ErrTreeArity, n.pred, want, len(n.args))
			}
		}
		for _, k := range n.kids {
			if err := rec(k); err != nil {
				return err
			}
		}
		return nil
	}
	return rec(t.n)
}

// leafArity is the argument count a leaf of this predicate must carry.
// A predicate Case's ParamIdx values index the leaf's argument list
// (its distinct :params in first-occurrence order, per BuildFrags), so
// the leaf owns exactly enough arguments to cover the largest index —
// and codegen derives the parameter list from the body, so every index
// up to the maximum occurs.
func leafArity(c Case) int {
	want := 0
	for _, idx := range c.ParamIdx {
		if int(idx)+1 > want {
			want = int(idx) + 1
		}
	}
	return want
}

// Encode is the canonical structural encoding (values excluded) used
// in cache keys: leaves "p<idx>", nodes "&(...)"/"|(...)", TRUE "T".
//
// The walk is unbounded recursion, so callers must have passed the tree
// through checkCaps first — the cache entry point does, and generated
// code only reaches Encode after a successful compose. Encode cannot
// enforce that itself: it does not receive the caps, and rejecting on
// a built-in bound would collapse the encoding of legitimate trees in
// projects that raised filter_tree_caps, which is a cache-key
// collision (distinct trees sharing composed SQL).
func (t Tree) Encode() string {
	if t.n == nil {
		return "T"
	}
	// Trees are cap-bounded (32 nodes by default), so the encoding fits
	// stack scratch in every non-adversarial case and the only
	// allocation is the returned string.
	var buf [128]byte
	return string(t.n.appendEncoding(buf[:0]))
}

func (n *node) appendEncoding(dst []byte) []byte {
	switch n.op {
	case opTrue:
		return append(dst, 'T')
	case opLeaf:
		dst = append(dst, 'p')
		return strconv.AppendInt(dst, int64(n.pred), 10)
	default:
		if n.op == opAnd {
			dst = append(dst, '&')
		} else {
			dst = append(dst, '|')
		}
		dst = append(dst, '(')
		for i, k := range n.kids {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = k.appendEncoding(dst)
		}
		return append(dst, ')')
	}
}

// TreeArgs flattens leaf argument values in preorder — the order the
// composer's tree-bind indices reference.
func TreeArgs(t Tree) []any {
	n := t.n.countArgs()
	if n == 0 {
		return nil
	}
	// Sized up front: the arg count is one cheap walk, and growing the
	// slice instead costs an allocation per doubling on a path that runs
	// per query call.
	out := make([]any, 0, n)
	return t.n.appendArgs(out)
}

func (n *node) countArgs() int {
	if n == nil {
		return 0
	}
	if n.op == opLeaf {
		return len(n.args)
	}
	c := 0
	for _, k := range n.kids {
		c += k.countArgs()
	}
	return c
}

func (n *node) appendArgs(dst []any) []any {
	if n == nil {
		return dst
	}
	if n.op == opLeaf {
		return append(dst, n.args...)
	}
	for _, k := range n.kids {
		dst = k.appendArgs(dst)
	}
	return dst
}
