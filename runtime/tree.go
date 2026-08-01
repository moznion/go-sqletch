package runtime

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Tree is a runtime-composed boolean combination over a query's closed
// predicate vocabulary. Values are opaque to the composer: they travel
// exclusively as bind parameters. Construct trees only through the
// generated per-query predicate constructors plus And/Or/Unscoped.
type Tree struct {
	op   uint8 // opLeaf, opAnd, opOr, opTrue
	kids []*Tree
	pred int16
	args []any
}

const (
	opLeaf uint8 = iota
	opAnd
	opOr
	opTrue
)

// And combines subtrees conjunctively. And() with no children is TRUE.
func And(ts ...*Tree) *Tree { return combine(opAnd, ts) }

// Or combines subtrees disjunctively. Or() with no children is TRUE.
func Or(ts ...*Tree) *Tree { return combine(opOr, ts) }

func combine(op uint8, ts []*Tree) *Tree {
	kids := make([]*Tree, 0, len(ts))
	for _, t := range ts {
		if t != nil {
			kids = append(kids, t)
		}
	}
	if len(kids) == 0 {
		return Unscoped()
	}
	if len(kids) == 1 {
		return kids[0]
	}
	return &Tree{op: op, kids: kids}
}

// NewLeaf is called by generated predicate constructors; user code
// never calls it directly.
func NewLeaf(pred int16, args ...any) *Tree {
	return &Tree{op: opLeaf, pred: pred, args: args}
}

// Unscoped is the explicit, greppable opt-out of a required
// @filter-tree!: it renders as TRUE.
func Unscoped() *Tree { return &Tree{op: opTrue} }

// TreeCaps bounds adversarially large trees (spec defaults).
type TreeCaps struct {
	MaxNodes int
	MaxDepth int
}

var DefaultTreeCaps = TreeCaps{MaxNodes: 32, MaxDepth: 8}

var (
	// ErrFilterRequired is returned when a @filter-tree! parameter is
	// nil; deliberate unscoped access must use the generated Unscoped
	// constructor.
	ErrFilterRequired = errors.New("sqletch: required @filter-tree parameter is nil (use the generated Unscoped() for deliberate opt-out)")
	ErrTreeTooLarge   = errors.New("sqletch: filter tree exceeds the configured caps")
	ErrTreePredicate  = errors.New("sqletch: filter tree references an unknown predicate")
)

// validate checks caps and predicate ranges.
func (t *Tree) validate(numPreds int, caps TreeCaps) error {
	nodes := 0
	var rec func(n *Tree, depth int) error
	rec = func(n *Tree, depth int) error {
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
	return rec(t, 1)
}

// Encode is the canonical structural encoding (values excluded) used
// in cache keys: leaves "p<idx>", nodes "&(...)"/"|(...)", TRUE "T".
func (t *Tree) Encode() string {
	if t == nil {
		return "T"
	}
	var b strings.Builder
	var rec func(n *Tree)
	rec = func(n *Tree) {
		switch n.op {
		case opTrue:
			b.WriteByte('T')
		case opLeaf:
			b.WriteByte('p')
			b.WriteString(strconv.Itoa(int(n.pred)))
		default:
			if n.op == opAnd {
				b.WriteByte('&')
			} else {
				b.WriteByte('|')
			}
			b.WriteByte('(')
			for i, k := range n.kids {
				if i > 0 {
					b.WriteByte(',')
				}
				rec(k)
			}
			b.WriteByte(')')
		}
	}
	rec(t)
	return b.String()
}

// TreeArgs flattens leaf argument values in preorder — the order the
// composer's tree-bind indices reference.
func TreeArgs(t *Tree) []any {
	var out []any
	var rec func(n *Tree)
	rec = func(n *Tree) {
		if n == nil {
			return
		}
		if n.op == opLeaf {
			out = append(out, n.args...)
			return
		}
		for _, k := range n.kids {
			rec(k)
		}
	}
	rec(t)
	return out
}
