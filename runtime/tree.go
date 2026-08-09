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
// never calls it directly.
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
)

// validate checks caps and predicate ranges.
func (t Tree) validate(numPreds int, caps TreeCaps) error {
	if t.n == nil {
		return nil
	}
	nodes := 0
	var rec func(n *node, depth int) error
	rec = func(n *node, depth int) error {
		nodes++
		if nodes > caps.MaxNodes || depth > caps.MaxDepth {
			return fmt.Errorf("%w (max %d nodes, depth %d)", ErrTreeTooLarge, caps.MaxNodes, caps.MaxDepth)
		}
		if n.op == opLeaf && (n.pred < 0 || int(n.pred) >= numPreds) {
			return fmt.Errorf("%w: %d", ErrTreePredicate, n.pred)
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

// Encode is the canonical structural encoding (values excluded) used
// in cache keys: leaves "p<idx>", nodes "&(...)"/"|(...)", TRUE "T".
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
