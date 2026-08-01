// Package runtime is the small public package imported by
// sqletch-generated code. It composes verified constant fragments into
// SQL deterministically — the byte-for-byte mirror of the compiler's
// verification renderer (premise P2); a shared conformance test in the
// compiler pins the equality.
//
// Nothing here parses SQL or touches user data: composition is
// table-driven selection and concatenation, and user values travel
// exclusively through bind parameters.
package runtime

import (
	"container/list"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

type Kind uint8

const (
	Skel Kind = iota
	Guarded
	Choose
	OrderBy
	FilterTree
	InAny // @in on PostgreSQL: `= ANY($n)`, ParamIdx[0] is the bind
)

type Sep uint8

const (
	SepNone Sep = iota
	SepAnd
	SepComma
)

// Span marks a :name parameter token inside a fragment's text.
type Span struct{ Start, End int32 }

// Case is one selectable @choose body.
type Case struct {
	Text       string
	ParamSpans []Span
	ParamIdx   []int16
}

// Frag is one compile-time-constant fragment. Emitted by sqletch
// generate; never constructed by hand.
type Frag struct {
	Kind       Kind
	Text       string
	ParamSpans []Span  // :name token positions within Text
	ParamIdx   []int16 // flattened params-struct index per span
	GuardMask  uint64  // Guarded: all these bits must be set
	Sep        Sep
	Cases      []Case // Choose: named cases, then the default (if any)
	// OrderBy: the keys
	Default *Case // OrderBy: the @default clause body (may be nil)
}

// ShapeKey identifies one concrete query shape.
type ShapeKey struct {
	Guards  uint64
	Choices []uint8
	// Orders holds one key sequence per @order-by block (elements are
	// key<<1|desc). nil inner = maximal/all keys (verification only);
	// empty = default-or-omit.
	Orders [][]uint8
	// Trees holds the canonical structural encoding of each
	// @filter-tree value (values excluded) — the cache-key component
	// for tree-shaped queries.
	Trees []string
}

// String is the canonical encoding (byte-identical to the compiler's
// shape.Key encoding).
func (k ShapeKey) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "g=%x", k.Guards)
	if len(k.Choices) > 0 {
		b.WriteString(";c=")
		for i, c := range k.Choices {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Itoa(int(c)))
		}
	}
	if len(k.Orders) > 0 {
		b.WriteString(";o=")
		for i, seq := range k.Orders {
			if i > 0 {
				b.WriteByte('|')
			}
			switch {
			case seq == nil:
				b.WriteByte('*')
			case len(seq) == 0:
				b.WriteByte('-')
			default:
				for j, e := range seq {
					if j > 0 {
						b.WriteByte(',')
					}
					b.WriteString(strconv.Itoa(int(e >> 1)))
					if e&1 == 1 {
						b.WriteByte('d')
					} else {
						b.WriteByte('a')
					}
				}
			}
		}
	}
	if len(k.Trees) > 0 {
		b.WriteString(";t=")
		for i, enc := range k.Trees {
			if i > 0 {
				b.WriteByte('|')
			}
			b.WriteString(enc)
		}
	}
	return b.String()
}

// Style is the dialect placeholder emission mode of a generated query,
// fixed at generation time.
type Style uint8

const (
	// StyleDollar: $1, $2, … numbered in first-occurrence order with
	// reuse per bind source (PostgreSQL).
	StyleDollar Style = iota
	// StyleQuestion: one '?' per occurrence; repeated references to a
	// bind source repeat the bind (MySQL, SQLite).
	StyleQuestion
)

// Bind is one entry of a shape's bind plan: the placeholder $(i+1)
// takes vals[Idx] (struct source) or treeArgs[Idx] (tree source).
// Plans contain positions only — never values — so they are cacheable.
type Bind struct {
	FromTree bool
	Idx      int16
}

// ResolveArgs materializes a bind plan into driver arguments.
func ResolveArgs(binds []Bind, vals, treeArgs []any) []any {
	if len(binds) == 0 {
		return nil
	}
	out := make([]any, len(binds))
	for i, b := range binds {
		if b.FromTree {
			out[i] = treeArgs[b.Idx]
		} else {
			out[i] = vals[b.Idx]
		}
	}
	return out
}

// Compose walks the fragment table in source order and emits the SQL
// of the shape plus the bind order: argIdx[n] is the params-struct
// value index bound to placeholder $(n+1). Placeholders are numbered
// in first-occurrence order per shape. Queries with a @filter-tree
// use ComposeTree instead.
func Compose(frags []Frag, key ShapeKey) (string, []int16) {
	return ComposeStyle(StyleDollar, frags, key)
}

// ComposeStyle is Compose with an explicit placeholder style.
func ComposeStyle(style Style, frags []Frag, key ShapeKey) (string, []int16) {
	sql, binds, err := ComposeTreeStyle(style, frags, key, nil, DefaultTreeCaps)
	if err != nil {
		// Without a tree the composer has no failure modes; a non-nil
		// error here is a generated-code bug.
		panic(err)
	}
	argIdx := make([]int16, len(binds))
	for i, bd := range binds {
		argIdx[i] = bd.Idx
	}
	return sql, argIdx
}

type bindKey struct {
	fromTree bool
	idx      int16
}

// ComposeTree composes a shape that may include one @filter-tree
// block. tree may be nil (renders TRUE); a nil tree with a required
// block is rejected by the generated code before reaching here.
func ComposeTree(frags []Frag, key ShapeKey, tree *Tree, caps TreeCaps) (string, []Bind, error) {
	return ComposeTreeStyle(StyleDollar, frags, key, tree, caps)
}

// ComposeTreeStyle is ComposeTree with an explicit placeholder style.
func ComposeTreeStyle(style Style, frags []Frag, key ShapeKey, tree *Tree, caps TreeCaps) (string, []Bind, error) {
	var b strings.Builder
	assigned := map[bindKey]int{}
	var binds []Bind

	place := func(k bindKey) {
		if style == StyleQuestion {
			binds = append(binds, Bind{FromTree: k.fromTree, Idx: k.idx})
			b.WriteByte('?')
			return
		}
		n, ok := assigned[k]
		if !ok {
			n = len(binds) + 1
			assigned[k] = n
			binds = append(binds, Bind{FromTree: k.fromTree, Idx: k.idx})
		}
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(n))
	}
	emitFrom := func(text string, spans []Span, idx []int16, fromTree bool, base int16) {
		last := int32(0)
		for i, sp := range spans {
			b.WriteString(text[last:sp.Start])
			place(bindKey{fromTree: fromTree, idx: base + idx[i]})
			last = sp.End
		}
		b.WriteString(text[last:])
	}
	emit := func(text string, spans []Span, idx []int16) {
		emitFrom(text, spans, idx, false, 0)
	}

	// Tree emission: preorder; each leaf instance gets its own slice of
	// the flattened TreeArgs space (repeated predicates bind
	// independently).
	treeArgBase := int16(0)
	var emitTree func(n *Tree, preds []Case)
	emitTree = func(n *Tree, preds []Case) {
		switch {
		case n == nil || n.op == opTrue:
			b.WriteString("TRUE")
		case n.op == opLeaf:
			c := preds[n.pred]
			b.WriteByte('(')
			emitFrom(c.Text, c.ParamSpans, c.ParamIdx, true, treeArgBase)
			b.WriteByte(')')
			treeArgBase += int16(len(n.args))
		default:
			sep := " AND "
			if n.op == opOr {
				sep = " OR "
			}
			b.WriteByte('(')
			for i, k := range n.kids {
				if i > 0 {
					b.WriteString(sep)
				}
				emitTree(k, preds)
			}
			b.WriteByte(')')
		}
	}

	chooseSeen, orderSeen := 0, 0
	for _, f := range frags {
		switch f.Kind {
		case Skel:
			emit(f.Text, f.ParamSpans, f.ParamIdx)
		case OrderBy:
			var seq []uint8 // nil = maximal (all keys)
			if key.Orders != nil && orderSeen < len(key.Orders) {
				seq = key.Orders[orderSeen]
			}
			orderSeen++
			b.WriteByte('\n')
			switch {
			case seq == nil:
				b.WriteString("ORDER BY ")
				for i, c := range f.Cases {
					if i > 0 {
						b.WriteString(", ")
					}
					emit(c.Text, c.ParamSpans, c.ParamIdx)
				}
			case len(seq) == 0:
				if f.Default != nil && f.Default.Text != "" {
					emit(f.Default.Text, f.Default.ParamSpans, f.Default.ParamIdx)
				}
			default:
				b.WriteString("ORDER BY ")
				for i, e := range seq {
					if i > 0 {
						b.WriteString(", ")
					}
					c := f.Cases[e>>1]
					emit(c.Text, c.ParamSpans, c.ParamIdx)
					if e&1 == 1 {
						b.WriteString(" DESC")
					}
				}
			}
		case Guarded:
			if key.Guards&f.GuardMask != f.GuardMask {
				continue
			}
			b.WriteByte('\n')
			switch f.Sep {
			case SepAnd:
				b.WriteString("AND (")
			case SepComma:
				b.WriteString(", ")
			}
			emit(f.Text, f.ParamSpans, f.ParamIdx)
			if f.Sep == SepAnd {
				b.WriteByte(')')
			}
		case Choose:
			ord := 0
			if chooseSeen < len(key.Choices) {
				ord = int(key.Choices[chooseSeen])
			}
			chooseSeen++
			b.WriteByte('\n')
			if ord >= 0 && ord < len(f.Cases) {
				c := f.Cases[ord]
				emit(c.Text, c.ParamSpans, c.ParamIdx)
			}
		case InAny:
			b.WriteString("= ANY(")
			place(bindKey{idx: f.ParamIdx[0]})
			b.WriteByte(')')
		case FilterTree:
			if tree == nil {
				b.WriteString("TRUE")
				continue
			}
			if err := tree.validate(len(f.Cases), caps); err != nil {
				return "", nil, err
			}
			emitTree(tree, f.Cases)
		}
	}
	return b.String(), binds, nil
}

// ErrChooseRequired is returned before any SQL is sent when a required
// @choose parameter carries its zero value.
var ErrChooseRequired = errors.New("sqletch: required @choose parameter has its zero value")

// ChooseOrdinal maps a generated enum value to the composer's case
// ordinal. With a @default, enum 0 selects the default (ordinal
// numNamed); named cases are 1..numNamed. Without a default, 0 is an
// error.
func ChooseOrdinal(v, numNamed int, hasDefault bool) (uint8, error) {
	switch {
	case v == 0 && hasDefault:
		return uint8(numNamed), nil
	case v >= 1 && v <= numNamed:
		return uint8(v - 1), nil
	case v == 0:
		return 0, ErrChooseRequired
	default:
		return 0, fmt.Errorf("sqletch: @choose enum value %d out of range", v)
	}
}

// ErrOrderKey is returned when an @order-by selection references a key
// out of range or repeats a key.
var ErrOrderKey = errors.New("sqletch: invalid @order-by key selection")

// OrderSeq converts a generated sort-key slice into the composer's
// sequence, validating range and rejecting duplicate keys (the same
// key in both directions makes no sense either).
func OrderSeq[T ~int](vals []T, numKeys int) ([]uint8, error) {
	seq := make([]uint8, 0, len(vals))
	var seen uint32
	for _, v := range vals {
		e := int(v)
		k := e >> 1
		if e < 0 || k >= numKeys {
			return nil, fmt.Errorf("%w: value %d out of range", ErrOrderKey, e)
		}
		if seen&(1<<uint(k)) != 0 {
			return nil, fmt.Errorf("%w: key %d selected twice", ErrOrderKey, k)
		}
		seen |= 1 << uint(k)
		seq = append(seq, uint8(e))
	}
	return seq, nil
}

// BuildArgs selects the bind values for a shape from the flattened
// params-struct values. Pointer values pass through as-is (active
// guards guarantee non-nil; the driver encodes pointers natively).
func BuildArgs(argIdx []int16, vals []any) []any {
	if len(argIdx) == 0 {
		return nil
	}
	out := make([]any, len(argIdx))
	for i, idx := range argIdx {
		out[i] = vals[idx]
	}
	return out
}

// Expanded is one statically expanded shape: SQL and bind order were
// precomputed at generate time (via Compose, so byte-identical to what
// runtime composition would produce).
type Expanded struct {
	SQL    string
	ArgIdx []int16
}

// ErrShapeNotExpanded is returned when a statically expanded query is
// asked for a shape key absent from its table — impossible unless the
// generated code is stale.
var ErrShapeNotExpanded = errors.New("sqletch: shape missing from the static expansion table")

// Lookup fetches a precomposed shape.
func Lookup(shapes map[string]Expanded, key ShapeKey) (string, []int16, error) {
	e, ok := shapes[key.String()]
	if !ok {
		return "", nil, ErrShapeNotExpanded
	}
	return e.SQL, e.ArgIdx, nil
}

// ComposedCache memoizes composed SQL per (query, shape), LRU-bounded.
// Hits compare the full key, never just its string form.
type ComposedCache struct {
	mu    sync.Mutex
	cap   int
	m     map[string]*list.Element
	order *list.List // front = most recent
}

type cacheEntry struct {
	mapKey string
	key    ShapeKey
	sql    string
	binds  []Bind
}

func NewComposedCache(capacity int) *ComposedCache {
	if capacity <= 0 {
		capacity = 256
	}
	return &ComposedCache{cap: capacity, m: map[string]*list.Element{}, order: list.New()}
}

func (c *ComposedCache) Get(queryName string, frags []Frag, key ShapeKey) (string, []int16) {
	return c.GetStyle(StyleDollar, queryName, frags, key)
}

// GetStyle is Get with an explicit placeholder style.
func (c *ComposedCache) GetStyle(style Style, queryName string, frags []Frag, key ShapeKey) (string, []int16) {
	sql, binds, err := c.get(style, queryName, frags, key, nil, DefaultTreeCaps)
	if err != nil {
		panic(err) // no failure modes without a tree
	}
	argIdx := make([]int16, len(binds))
	for i, bd := range binds {
		argIdx[i] = bd.Idx
	}
	return sql, argIdx
}

// GetTree is the @filter-tree variant: the tree's structural encoding
// becomes part of the cache key (values never do).
func (c *ComposedCache) GetTree(queryName string, frags []Frag, key ShapeKey, tree *Tree, caps TreeCaps) (string, []Bind, error) {
	return c.GetTreeStyle(StyleDollar, queryName, frags, key, tree, caps)
}

// GetTreeStyle is GetTree with an explicit placeholder style.
func (c *ComposedCache) GetTreeStyle(style Style, queryName string, frags []Frag, key ShapeKey, tree *Tree, caps TreeCaps) (string, []Bind, error) {
	key.Trees = []string{tree.Encode()}
	return c.get(style, queryName, frags, key, tree, caps)
}

func (c *ComposedCache) get(style Style, queryName string, frags []Frag, key ShapeKey, tree *Tree, caps TreeCaps) (string, []Bind, error) {
	mapKey := queryName + "|" + key.String()
	c.mu.Lock()
	if el, ok := c.m[mapKey]; ok {
		e := el.Value.(*cacheEntry)
		if keysEqual(e.key, key) {
			c.order.MoveToFront(el)
			sql, binds := e.sql, e.binds
			c.mu.Unlock()
			return sql, binds, nil
		}
		// Full-key mismatch (canonical-encoding collision would be a
		// bug, but never trust the string form): drop and recompute.
		c.order.Remove(el)
		delete(c.m, mapKey)
	}
	c.mu.Unlock()

	sql, binds, err := ComposeTreeStyle(style, frags, key, tree, caps)
	if err != nil {
		return "", nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[mapKey]; !ok {
		e := &cacheEntry{mapKey: mapKey, key: cloneKey(key), sql: sql, binds: binds}
		c.m[mapKey] = c.order.PushFront(e)
		for len(c.m) > c.cap {
			oldest := c.order.Back()
			c.order.Remove(oldest)
			delete(c.m, oldest.Value.(*cacheEntry).mapKey)
		}
	}
	return sql, binds, nil
}

func cloneKey(k ShapeKey) ShapeKey {
	out := ShapeKey{Guards: k.Guards, Choices: append([]uint8(nil), k.Choices...)}
	for _, seq := range k.Orders {
		if seq == nil {
			out.Orders = append(out.Orders, nil)
		} else {
			out.Orders = append(out.Orders, append([]uint8(nil), seq...))
		}
	}
	out.Trees = append([]string(nil), k.Trees...)
	return out
}

// keysEqual is the full-key comparison — hashes and encodings are an
// index, never identity.
func keysEqual(a, b ShapeKey) bool {
	if a.Guards != b.Guards || !choicesEqual(a.Choices, b.Choices) ||
		len(a.Orders) != len(b.Orders) || len(a.Trees) != len(b.Trees) {
		return false
	}
	for i := range a.Orders {
		if (a.Orders[i] == nil) != (b.Orders[i] == nil) || !choicesEqual(a.Orders[i], b.Orders[i]) {
			return false
		}
	}
	for i := range a.Trees {
		if a.Trees[i] != b.Trees[i] {
			return false
		}
	}
	return true
}

func choicesEqual(a, b []uint8) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
