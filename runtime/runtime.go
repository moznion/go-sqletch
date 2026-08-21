// Package runtime is the small public package imported by
// sqletch-generated code. It composes verified constant fragments into
// SQL deterministically — the byte-for-byte mirror of the compiler's
// verification renderer (premise P2); a shared conformance test in the
// compiler pins the equality.
//
// Nothing here parses SQL or touches user data: composition is
// table-driven selection and concatenation, and user values travel
// exclusively through bind parameters.
//
// # API contract (v1)
//
// Two audiences share this package:
//
//   - The USER API — what application code is expected to touch:
//     [Tree] and its constructors ([And], [Or], [Unscoped]; the typed
//     per-predicate constructors are generated into your package),
//     [TreeCaps], and the sentinel errors [ErrFilterRequired],
//     [ErrChooseRequired], [ErrOrderKey], [ErrTreeTooLarge],
//     [ErrTreePredicate]. These follow Go API compatibility for all
//     v1 releases.
//
//     The observability surface — [Observer], [CacheStats],
//     [ShapeUse], [ShapeSpaceInfo] and the corresponding
//     [ComposedCache] methods (design doc 18) — is USER API too.
//
//   - The GENERATED-CODE CONTRACT — [Frag], [ShapeKey], [Bind],
//     [Compose] and friends, [ComposedCache], [Expanded]. These are
//     public only because generated code lives outside this module.
//     They also follow Go API compatibility within v1, but their
//     semantics are pinned to the sqletch compiler: after upgrading
//     sqletch, re-run `sqletch generate` so generated code and
//     runtime agree (the conformance tests hold per version pair, not
//     across them). Constructing [Frag] tables by hand is UNSUPPORTED.
package runtime

import (
	"container/list"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type Kind uint8

const (
	Skel Kind = iota
	Guarded
	Choose
	OrderBy
	FilterTree
	InAny  // @in on PostgreSQL: `= ANY($n)`, ParamIdx[0] is the bind
	InList // @in on expanding dialects: `IN (?, …)`, arity from the key
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
	// Arities holds the element count of each @in slice, in template
	// order — a shape dimension on expanding dialects only (empty on
	// PostgreSQL, whose `= ANY` binds the slice whole).
	Arities []int32
}

// keyBufSize is the stack scratch a canonical key encoding is built in
// before it is copied into a string. Keys longer than this simply spill
// to the heap; the constant only decides where the common case builds.
const keyBufSize = 96

// String is the canonical encoding (byte-identical to the compiler's
// shape.Key encoding).
func (k ShapeKey) String() string {
	var buf [keyBufSize]byte
	return string(k.appendTo(buf[:0]))
}

// appendTo writes the canonical encoding to dst. It is the allocation-
// free core of String: the composed cache builds its map key straight
// into a stack buffer with it, so a cache hit never allocates a key.
func (k ShapeKey) appendTo(dst []byte) []byte {
	dst = append(dst, 'g', '=')
	dst = strconv.AppendUint(dst, k.Guards, 16)
	if len(k.Choices) > 0 {
		dst = append(dst, ';', 'c', '=')
		for i, c := range k.Choices {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = strconv.AppendUint(dst, uint64(c), 10)
		}
	}
	if len(k.Orders) > 0 {
		dst = append(dst, ';', 'o', '=')
		for i, seq := range k.Orders {
			if i > 0 {
				dst = append(dst, '|')
			}
			switch {
			case seq == nil:
				dst = append(dst, '*')
			case len(seq) == 0:
				dst = append(dst, '-')
			default:
				for j, e := range seq {
					if j > 0 {
						dst = append(dst, ',')
					}
					dst = strconv.AppendUint(dst, uint64(e>>1), 10)
					if e&1 == 1 {
						dst = append(dst, 'd')
					} else {
						dst = append(dst, 'a')
					}
				}
			}
		}
	}
	if len(k.Trees) > 0 {
		dst = append(dst, ';', 't', '=')
		for i, enc := range k.Trees {
			if i > 0 {
				dst = append(dst, '|')
			}
			dst = append(dst, enc...)
		}
	}
	if len(k.Arities) > 0 {
		dst = append(dst, ';', 'n', '=')
		for i, n := range k.Arities {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = strconv.AppendInt(dst, int64(n), 10)
		}
	}
	return dst
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
	// Elem selects within a slice value: 0 binds the value whole; k>0
	// binds element k-1 (@in arity expansion on Tier 2 dialects).
	Elem int16
}

// ResolveArgs materializes a bind plan into driver arguments.
func ResolveArgs(binds []Bind, vals, treeArgs []any) []any {
	if len(binds) == 0 {
		return nil
	}
	out := make([]any, len(binds))
	for i, b := range binds {
		src := vals
		if b.FromTree {
			src = treeArgs
		}
		v := src[b.Idx]
		if b.Elem > 0 {
			v = sliceElem(v, int(b.Elem)-1)
		}
		out[i] = v
	}
	return out
}

// sliceElem selects element i of a slice held in an interface. The
// type switch covers the element types @in parameters actually take, so
// the reflect path — which costs far more than the indexing it performs
// — is reached only by exotic slices.
func sliceElem(v any, i int) any {
	switch s := v.(type) {
	case []string:
		return s[i]
	case []int64:
		return s[i]
	case []int32:
		return s[i]
	case []int16:
		return s[i]
	case []int:
		return s[i]
	case []uint64:
		return s[i]
	case []float64:
		return s[i]
	case []float32:
		return s[i]
	case []bool:
		return s[i]
	case []time.Time:
		return s[i]
	case []any:
		return s[i]
	}
	return reflect.ValueOf(v).Index(i).Interface()
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
	sql, binds, err := ComposeTreeStyle(style, frags, key, Tree{}, DefaultTreeCaps)
	if err != nil {
		// An empty tree removes the FilterTree failure modes, but an
		// InList fragment can still exceed MaxInArity
		// ([ErrShapeKeyLimit]) even with no tree — so this is reachable,
		// though only by a hand-built fragment table with an InList
		// frag. Generated code never reaches it: dollar-style tables
		// carry no InList frag, and question-style @in routes through
		// the error-returning GetBindsStyle. This value API cannot
		// return the error; a caller composing an InList/tree fragment
		// table must use GetBindsStyle / GetTreeStyle instead.
		panic(fmt.Errorf("runtime.ComposeStyle: %w (use GetBindsStyle/GetTreeStyle for @in or @filter-tree fragment tables)", err))
	}
	argIdx := make([]int16, len(binds))
	for i, bd := range binds {
		argIdx[i] = bd.Idx
	}
	return sql, argIdx
}

// composer holds the emission state of one composition. It replaces a
// nest of closures over local variables: the closures forced every
// composition to heap-allocate their captured state, while a struct
// with methods keeps it in one frame.
type composer struct {
	style Style
	b     []byte
	binds []Bind
	// treeArgBase advances per leaf: each leaf instance owns its own
	// slice of the flattened TreeArgs space (repeated predicates bind
	// independently).
	treeArgBase int16
}

// place emits one placeholder for a bind source. Under StyleDollar a
// source reused across fragments must reuse its number, which is a
// lookup over the binds already placed — a linear scan rather than a
// map, because bind counts are bounded by the template's parameter
// count and stay small enough that the map's hashing costs more than
// the scan (and the map itself allocated on every composition).
func (co *composer) place(bd Bind) {
	if co.style == StyleQuestion {
		// One '?' per occurrence: repeated references repeat the bind,
		// so there is nothing to look up.
		co.binds = append(co.binds, bd)
		co.b = append(co.b, '?')
		return
	}
	n := 0
	for i := range co.binds {
		if co.binds[i] == bd {
			n = i + 1
			break
		}
	}
	if n == 0 {
		co.binds = append(co.binds, bd)
		n = len(co.binds)
	}
	co.b = append(co.b, '$')
	co.b = strconv.AppendInt(co.b, int64(n), 10)
}

func (co *composer) emitFrom(text string, spans []Span, idx []int16, fromTree bool, base int16) {
	last := int32(0)
	for i, sp := range spans {
		co.b = append(co.b, text[last:sp.Start]...)
		co.place(Bind{FromTree: fromTree, Idx: base + idx[i]})
		last = sp.End
	}
	co.b = append(co.b, text[last:]...)
}

func (co *composer) emit(text string, spans []Span, idx []int16) {
	co.emitFrom(text, spans, idx, false, 0)
}

// emitTree renders a filter tree in preorder.
func (co *composer) emitTree(n *node, preds []Case) {
	switch {
	case n == nil || n.op == opTrue:
		co.b = append(co.b, "TRUE"...)
	case n.op == opLeaf:
		c := preds[n.pred]
		co.b = append(co.b, '(')
		co.emitFrom(c.Text, c.ParamSpans, c.ParamIdx, true, co.treeArgBase)
		co.b = append(co.b, ')')
		co.treeArgBase += int16(len(n.args))
	default:
		sep := " AND "
		if n.op == opOr {
			sep = " OR "
		}
		co.b = append(co.b, '(')
		for i, k := range n.kids {
			if i > 0 {
				co.b = append(co.b, sep...)
			}
			co.emitTree(k, preds)
		}
		co.b = append(co.b, ')')
	}
}

// ComposeTree composes a shape that may include one @filter-tree
// block. The zero Tree renders TRUE; a zero tree for a required block
// is rejected by the generated code before reaching here.
func ComposeTree(frags []Frag, key ShapeKey, tree Tree, caps TreeCaps) (string, []Bind, error) {
	return ComposeTreeStyle(StyleDollar, frags, key, tree, caps)
}

// composeBufSize is the initial capacity of a pooled render scratch,
// comfortably above a typical composed statement.
const composeBufSize = 512

// composeBufs recycles render scratch across compositions. A local
// buffer cannot serve: it shares a frame with the bind plan, which is
// returned and therefore heap-allocated, so escape analysis drags the
// scratch to the heap with it. The composed SQL is copied out at exact
// length, so a recycled buffer is never retained by a cache entry.
var composeBufs = sync.Pool{
	New: func() any {
		b := make([]byte, 0, composeBufSize)
		return &b
	},
}

// ComposeTreeStyle is ComposeTree with an explicit placeholder style.
func ComposeTreeStyle(style Style, frags []Frag, key ShapeKey, tree Tree, caps TreeCaps) (string, []Bind, error) {
	// Released explicitly at both exits rather than by defer: a deferred
	// closure would capture the composer and force it onto the heap,
	// which is the allocation this pool exists to remove.
	bp := composeBufs.Get().(*[]byte)
	co := composer{style: style, b: (*bp)[:0]}
	if n := countBinds(frags); n > 0 {
		co.binds = make([]Bind, 0, n)
	}

	chooseSeen, orderSeen, inSeen := 0, 0, 0
	for _, f := range frags {
		switch f.Kind {
		case Skel:
			co.emit(f.Text, f.ParamSpans, f.ParamIdx)
		case OrderBy:
			var seq []uint8 // nil = maximal (all keys)
			if key.Orders != nil && orderSeen < len(key.Orders) {
				seq = key.Orders[orderSeen]
			}
			orderSeen++
			co.b = append(co.b, '\n')
			switch {
			case seq == nil:
				co.b = append(co.b, "ORDER BY "...)
				for i, c := range f.Cases {
					if i > 0 {
						co.b = append(co.b, ", "...)
					}
					co.emit(c.Text, c.ParamSpans, c.ParamIdx)
				}
			case len(seq) == 0:
				if f.Default != nil && f.Default.Text != "" {
					co.emit(f.Default.Text, f.Default.ParamSpans, f.Default.ParamIdx)
				}
			default:
				co.b = append(co.b, "ORDER BY "...)
				for i, e := range seq {
					if i > 0 {
						co.b = append(co.b, ", "...)
					}
					c := f.Cases[e>>1]
					co.emit(c.Text, c.ParamSpans, c.ParamIdx)
					if e&1 == 1 {
						co.b = append(co.b, " DESC"...)
					}
				}
			}
		case Guarded:
			if key.Guards&f.GuardMask != f.GuardMask {
				continue
			}
			co.b = append(co.b, '\n')
			switch f.Sep {
			case SepAnd:
				co.b = append(co.b, "AND ("...)
			case SepComma:
				co.b = append(co.b, ", "...)
			}
			co.emit(f.Text, f.ParamSpans, f.ParamIdx)
			if f.Sep == SepAnd {
				co.b = append(co.b, ')')
			}
		case Choose:
			ord := 0
			if chooseSeen < len(key.Choices) {
				ord = int(key.Choices[chooseSeen])
			}
			chooseSeen++
			co.b = append(co.b, '\n')
			if ord >= 0 && ord < len(f.Cases) {
				c := f.Cases[ord]
				co.emit(c.Text, c.ParamSpans, c.ParamIdx)
			}
		case InAny:
			co.b = append(co.b, "= ANY("...)
			co.place(Bind{Idx: f.ParamIdx[0]})
			co.b = append(co.b, ')')
		case InList:
			n := int32(1)
			if inSeen < len(key.Arities) {
				n = key.Arities[inSeen]
			}
			inSeen++
			if n <= 0 {
				// Arity 0 keeps the spec's semantics: an empty list
				// matches nothing, FALSE even for a NULL operand. The
				// dialect's emission is generated into Frag.Text; the
				// fallback covers fragment tables generated before it
				// existed (MySQL form).
				if f.Text != "" {
					co.b = append(co.b, f.Text...)
				} else {
					co.b = append(co.b, "IN (SELECT NULL FROM DUAL WHERE FALSE)"...)
				}
				continue
			}
			if n > MaxInArity {
				*bp = co.b[:0]
				composeBufs.Put(bp)
				return "", nil, fmt.Errorf("%w: @in list of %d elements exceeds %d",
					ErrShapeKeyLimit, n, MaxInArity)
			}
			co.b = append(co.b, "IN ("...)
			for e := int32(1); e <= n; e++ {
				if e > 1 {
					co.b = append(co.b, ", "...)
				}
				co.place(Bind{Idx: f.ParamIdx[0], Elem: int16(e)})
			}
			co.b = append(co.b, ')')
		case FilterTree:
			if tree.IsZero() {
				co.b = append(co.b, "TRUE"...)
				continue
			}
			if err := tree.validate(len(f.Cases), caps); err != nil {
				*bp = co.b[:0]
				composeBufs.Put(bp)
				return "", nil, err
			}
			co.emitTree(tree.n, f.Cases)
		}
	}
	sql := string(co.b)
	*bp = co.b[:0] // keep any growth for the next composition
	composeBufs.Put(bp)
	return sql, co.binds, nil
}

// countBinds is an upper bound on the binds a composition can place,
// used to size the bind slice in one allocation. Over-counting (guards
// that turn out inactive, @choose cases not selected) only wastes a
// little capacity on the miss path; under-counting would just fall back
// to append's growth, so the estimate need not be tight.
func countBinds(frags []Frag) int {
	n := 0
	for i := range frags {
		f := &frags[i]
		n += len(f.ParamIdx)
		for j := range f.Cases {
			n += len(f.Cases[j].ParamIdx)
		}
		if f.Default != nil {
			n += len(f.Default.ParamIdx)
		}
	}
	return n
}

// ErrChooseRequired is returned before any SQL is sent when a required
// @choose parameter carries its zero value.
var ErrChooseRequired = errors.New("sqletch: required @choose parameter has its zero value")

// Structural limits of the ShapeKey encoding. The compiler refuses
// templates that exceed them (SQLETCH010), so generated code can never
// reach the checks below; they exist because silent truncation here
// composes a DIFFERENT query's SQL — a wrong case, a wrong sort column
// — with no error anywhere. internal/codegen pins these against the
// scanner's copies.
const (
	// MaxOrderKeys: sequence elements pack as key<<1|desc into a uint8,
	// and the duplicate-key mask below is 64 bits wide.
	MaxOrderKeys = 64
	// MaxChooseOrdinals: ShapeKey.Choices holds one uint8 per @choose
	// block, counting the @default body.
	MaxChooseOrdinals = 255
	// MaxInArity bounds an @in list on expanding dialects. Bind.Elem is
	// an int16 holding a 1-based element index, so past this the index
	// wraps: negative reads as "bind the value whole", and far enough
	// round it lands on a different element. Unlike the limits above
	// this one is on CALLER data, not the template, so it is enforced
	// during composition. Engines cap placeholders well below it
	// anyway (SQLite's default is 32766).
	MaxInArity = 32767
)

// ErrShapeKeyLimit reports a construct too large for the shape key's
// encoding. Reaching it means codegen and the scanner disagree.
var ErrShapeKeyLimit = errors.New("sqletch: construct exceeds the shape-key encoding limit")

// ChooseOrdinal maps a generated enum value to the composer's case
// ordinal. With a @default, enum 0 selects the default (ordinal
// numNamed); named cases are 1..numNamed. Without a default, 0 is an
// error.
func ChooseOrdinal(v, numNamed int, hasDefault bool) (uint8, error) {
	ordinals := numNamed
	if hasDefault {
		ordinals++
	}
	if ordinals > MaxChooseOrdinals {
		return 0, fmt.Errorf("%w: %d @choose cases exceeds %d",
			ErrShapeKeyLimit, ordinals, MaxChooseOrdinals)
	}
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
	if numKeys > MaxOrderKeys {
		return nil, fmt.Errorf("%w: %d @order-by keys exceeds %d",
			ErrShapeKeyLimit, numKeys, MaxOrderKeys)
	}
	seq := make([]uint8, 0, len(vals))
	var seen uint64
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
	// Indexing through stack scratch rather than key.String() keeps the
	// encoding out of the string heap entirely — the map index of a
	// string(...) conversion does not copy.
	var buf [keyBufSize]byte
	e, ok := shapes[string(key.appendTo(buf[:0]))]
	if !ok {
		return "", nil, ErrShapeNotExpanded
	}
	return e.SQL, e.ArgIdx, nil
}

// ComposedCache memoizes composed SQL per (query, shape), bounded by a
// capacity with approximate-LRU eviction. Hits compare the full key,
// never just its string form.
//
// Reads take no lock. Composition is deterministic and entries are
// immutable once published, so hits are served from an atomically
// published snapshot of the entry map; only misses — bounded in a
// healthy workload by the number of shapes an application uses, which
// is typically tiny — take the mutex.
//
// Eviction is second-chance (CLOCK) rather than exact LRU: a lock-free
// hit records recency by setting a bit on its own entry instead of
// reordering shared state, and eviction skips once over entries whose
// bit is set. Recency is therefore approximate, which bounds memory
// exactly as strict LRU would while letting hits scale across cores.
type ComposedCache struct {
	// fast is an immutable snapshot of m, read without any lock. It may
	// lag m: an entry evicted from m stays readable here until the next
	// publish, which is harmless — composition is deterministic, so a
	// stale entry is the same SQL — and bounds retained entries at
	// twice the capacity.
	fast atomic.Pointer[map[string]*cacheEntry]

	mu    sync.Mutex
	cap   int
	m     map[string]*cacheEntry
	order *list.List // front = most recent; the CLOCK hand walks from back
	// sinceSnap counts inserts since the last publish, and evictedSince
	// records whether any eviction happened in that window. Publishing
	// copies the map, so a workload whose shape set exceeds the capacity
	// amortizes it instead of paying O(cap) on every miss.
	sinceSnap    int
	evictedSince bool
	// snapshots counts publishNow calls — the O(capacity) map copies.
	// White-box: the churn amortization test pins its growth rate.
	snapshots uint64
	// hitFlushCredit and pendingHits rate-cap the mutex-path hit flush
	// (see entry()): credit grants ONE immediate flush per insert-path
	// publish, and pendingHits counts credit-less deferred hits so a
	// flush still happens within cap of them. Counters only — no time
	// dependence anywhere in this cache.
	hitFlushCredit bool
	pendingHits    int

	// obs receives one ObserveCompose per access. It is an atomic
	// pointer so a SetObserver that races in-flight lock-free reads is
	// safe (a plain interface field is two words: a torn read of one
	// mid-install can crash). The hot path pays a single atomic load;
	// nil until installed. A nil pointer means no observer.
	obs atomic.Pointer[Observer]
	// track gates per-entry hit counting. It flips on (sticky) at the
	// first SetObserver/Stats/TopShapes call: a cache nobody observes
	// pays only this shared read on its hit path, never the counter's
	// exclusive cache-line store — the same trade touch() makes.
	track atomic.Bool
	// Cumulative counters behind Stats, guarded by mu. Hits live on the
	// entries themselves (lock-free path); hitsFolded accumulates the
	// counts of removed entries so cumulative hits survive eviction.
	misses     uint64
	inserts    uint64
	evictions  uint64
	hitsFolded uint64
	sqlBytes   int64
	// maxBytes bounds the approximate retained bytes (composed SQL +
	// bind plans + arg indices) across resident entries; 0 disables the
	// byte bound. totalBytes is the running sum eviction compares to it.
	// The count cap alone leaves memory unbounded when a caller-driven
	// @in arity makes single entries large (up to MaxInArity elements),
	// so the byte bound caps that independently of the entry count.
	maxBytes   int64
	totalBytes int64
}

// defaultCacheMaxBytes is the default byte ceiling: generous enough that
// an ordinary shape set never trips it, finite enough that caller-driven
// @in arities (each entry up to ~MaxInArity elements of SQL + binds)
// cannot pin unbounded memory. Override with SetMaxBytes.
const defaultCacheMaxBytes int64 = 64 << 20

// cacheBindBytes approximates one Bind's retained size (a
// {bool,int16,int16} padded to 8) for the byte accounting; the exact
// value only affects when the byte bound trips, never correctness.
const cacheBindBytes = 8

// entryBytes approximates an entry's retained memory for the byte bound.
func entryBytes(e *cacheEntry) int64 {
	return int64(len(e.sql)) + int64(len(e.mapKey)) +
		int64(len(e.binds))*cacheBindBytes + int64(len(e.argIdx))*2
}

type cacheEntry struct {
	mapKey string
	query  string
	key    ShapeKey
	sql    string
	binds  []Bind
	// argIdx is the Bind.Idx projection the non-tree API returns. It is
	// derived once at insertion rather than rebuilt on every hit;
	// callers only read it (BuildArgs), exactly as they already only
	// read the shared binds.
	argIdx []int16

	// ref is the second-chance bit: a hit sets it without any lock, and
	// eviction clears it to grant one reprieve. Every other field is
	// immutable once the entry is published.
	ref atomic.Bool
	// hits counts accesses served from this entry. Entry-local so cores
	// contend on a shape's own line only when they already share it for
	// ref; Stats sums residents and folds evicted counts (hitsFolded).
	hits atomic.Uint64
	// el is the entry's position in the recency list, guarded by the
	// cache mutex.
	el *list.Element
}

func newCacheEntry(mapKey, query string, key ShapeKey, sql string, binds []Bind) *cacheEntry {
	// The composer sizes its bind slice from an upper bound that counts
	// every @choose case, not just the selected one. That slack is free
	// on the miss path but an entry outlives the call, so it is copied
	// down to exact length here rather than retained for the life of
	// the cache.
	if cap(binds) > len(binds) {
		binds = append(make([]Bind, 0, len(binds)), binds...)
	}
	e := &cacheEntry{mapKey: mapKey, query: query, key: cloneKey(key), sql: sql, binds: binds}
	if len(binds) > 0 {
		e.argIdx = make([]int16, len(binds))
		for i, bd := range binds {
			e.argIdx[i] = bd.Idx
		}
	}
	return e
}

func NewComposedCache(capacity int) *ComposedCache {
	if capacity <= 0 {
		capacity = 256
	}
	return &ComposedCache{
		cap:      capacity,
		maxBytes: defaultCacheMaxBytes,
		m:        map[string]*cacheEntry{},
		order:    list.New(),
	}
}

// SetMaxBytes bounds the cache's approximate retained bytes (Σ composed
// SQL + bind plans + arg indices over resident entries) in addition to
// the entry-count capacity, evicting least-recently-used entries when
// the total would exceed it. A value <= 0 disables the byte bound
// (count cap only). Like SetObserver, call it before the cache serves
// traffic. The default is [defaultCacheMaxBytes]; it exists so a
// caller-controlled @in arity cannot pin unbounded memory behind a
// modest entry count.
func (c *ComposedCache) SetMaxBytes(n int64) {
	c.mu.Lock()
	c.maxBytes = n
	for len(c.m) > 1 && c.maxBytes > 0 && c.totalBytes > c.maxBytes {
		c.evictOne()
	}
	c.publishNow()
	c.mu.Unlock()
}

func (c *ComposedCache) Get(queryName string, frags []Frag, key ShapeKey) (string, []int16) {
	return c.GetStyle(StyleDollar, queryName, frags, key)
}

// GetStyle is Get with an explicit placeholder style.
func (c *ComposedCache) GetStyle(style Style, queryName string, frags []Frag, key ShapeKey) (string, []int16) {
	e, err := c.entry(style, queryName, frags, key, Tree{}, DefaultTreeCaps)
	if err != nil {
		// Reachable only for a hand-built InList fragment table over
		// MaxInArity (see ComposeStyle); generated code routes @in
		// through the error-returning GetBindsStyle and never gets here.
		panic(fmt.Errorf("runtime.GetStyle: %w (use GetBindsStyle/GetTreeStyle for @in or @filter-tree fragment tables)", err))
	}
	return e.sql, e.argIdx
}

// GetBindsStyle is GetStyle returning the full bind plan — needed
// when binds select slice elements (@in arity expansion).
func (c *ComposedCache) GetBindsStyle(style Style, queryName string, frags []Frag, key ShapeKey) (string, []Bind, error) {
	return c.get(style, queryName, frags, key, Tree{}, DefaultTreeCaps)
}

// GetTree is the @filter-tree variant: the tree's structural encoding
// becomes part of the cache key (values never do).
func (c *ComposedCache) GetTree(queryName string, frags []Frag, key ShapeKey, tree Tree, caps TreeCaps) (string, []Bind, error) {
	return c.GetTreeStyle(StyleDollar, queryName, frags, key, tree, caps)
}

// GetTreeStyle is GetTree with an explicit placeholder style.
func (c *ComposedCache) GetTreeStyle(style Style, queryName string, frags []Frag, key ShapeKey, tree Tree, caps TreeCaps) (string, []Bind, error) {
	// Before Encode, which walks the tree with no depth bound of its
	// own: the caps must bound the work, not just the outcome. A tree
	// past them would otherwise be encoded in full — and deep enough,
	// overflow the stack — on its way to being rejected by compose.
	if err := tree.checkCaps(caps); err != nil {
		return "", nil, err
	}
	key.Trees = []string{tree.Encode()}
	return c.get(style, queryName, frags, key, tree, caps)
}

func (c *ComposedCache) get(style Style, queryName string, frags []Frag, key ShapeKey, tree Tree, caps TreeCaps) (string, []Bind, error) {
	e, err := c.entry(style, queryName, frags, key, tree, caps)
	if err != nil {
		return "", nil, err
	}
	return e.sql, e.binds, nil
}

func (c *ComposedCache) entry(style Style, queryName string, frags []Frag, key ShapeKey, tree Tree, caps TreeCaps) (*cacheEntry, error) {
	// The map key is built into stack scratch and indexed as
	// string(buf), which the compiler lowers to a lookup that does not
	// copy: a cache hit allocates nothing at all. Only a miss pays for
	// the string, where it is retained by the entry anyway.
	// The style leads the key because it changes the composed text for
	// an otherwise identical (query, shape): dollar numbers a reused
	// bind once, question repeats it. Generated code fixes the style per
	// package so it cannot mix, but the entry points take it as an
	// argument, and a cache that ignored it would hand a caller the
	// other style's SQL. This key is internal — nothing outside the
	// cache observes its encoding.
	var buf [keyBufSize]byte
	mk := append(buf[:0], '0'+byte(style), '|')
	mk = append(mk, queryName...)
	mk = append(mk, '|')
	mk = key.appendTo(mk)

	// Lock-free hit. Observer calls pass e.key — the entry's retained
	// clone — never the caller's key: an interface call's arguments
	// escape, and threading the caller's key through one would force
	// every generated call site's key slices onto the heap (measured:
	// +1 alloc/op on the hit path). keysEqual has just proven the two
	// keys identical, so the observer cannot tell the difference.
	if snap := c.fast.Load(); snap != nil {
		if e, ok := (*snap)[string(mk)]; ok && keysEqual(e.key, key) {
			touch(e)
			if c.track.Load() {
				e.hits.Add(1)
			}
			if op := c.obs.Load(); op != nil {
				(*op).ObserveCompose(queryName, e.key, true)
			}
			return e, nil
		}
	}

	c.mu.Lock()
	if e, ok := c.m[string(mk)]; ok {
		// Present but not (yet) in the published snapshot.
		if keysEqual(e.key, key) {
			c.order.MoveToFront(e.el)
			// The lock-free snapshot missed this entry: it was created
			// after the last publish and held back by the amortization
			// guard. Flushing here is what keeps M2's promise — a
			// workload that fills past capacity and then stops inserting
			// (steady state = pure hits) must not serve its newest shapes
			// under this mutex forever — but the flush itself is an
			// O(capacity) map copy, so it is RATE-CAPPED rather than
			// unconditional: under permanent churn (shape set never fits
			// the capacity — e.g. varied @in arities on MySQL/SQLite)
			// every new shape's first re-hit lands here, and copying the
			// map each time made churn cost one full copy per new shape.
			//
			// Two counter-based triggers (no time dependence): a CREDIT
			// granted by each insert-path/administrative publish allows
			// the next deferred hit to flush immediately — so once
			// inserts stop, the first such hit still flushes, exactly as
			// before (the pinned M2 test) — and credit-less deferred hits
			// flush after cap of them, bounding how long any resident
			// entry can stay off the lock-free path at cap mutex hits.
			// Under churn both triggers fire O(1) times per cap
			// operations, amortizing the copy; the 2×cap retained-entry
			// bound is untouched (publish frequency changed, snapshot
			// contents did not).
			c.pendingHits++
			if c.hitFlushCredit || c.pendingHits >= c.cap {
				c.publishNow()
				c.hitFlushCredit = false
			}
			c.mu.Unlock()
			touch(e)
			if c.track.Load() {
				e.hits.Add(1)
			}
			if op := c.obs.Load(); op != nil {
				(*op).ObserveCompose(queryName, e.key, true)
			}
			return e, nil
		}
		// Full-key mismatch (canonical-encoding collision would be a
		// bug, but never trust the string form): drop and recompute.
		c.remove(e)
	}
	c.mu.Unlock()

	// Composed outside the lock: composition is pure, so a duplicate
	// under a race costs work but never correctness, and holding the
	// mutex across it would serialize every cold shape.
	sql, binds, err := ComposeTreeStyle(style, frags, key, tree, caps)
	if err != nil {
		return nil, err
	}
	mapKey := string(mk)

	// The observer is called after the unlock at both exits: user code
	// must never run under the cache mutex. Both exits report hit=false
	// — composition ran here even when a raced twin's entry is kept, so
	// compose events can exceed insertions but never undercount work.
	c.mu.Lock()
	c.misses++
	if e, ok := c.m[mapKey]; ok {
		// Raced with another composer of the same shape; keep the entry
		// already published so all callers share one instance. A
		// full-key mismatch still wins over the encoding, as above.
		if keysEqual(e.key, key) {
			c.order.MoveToFront(e.el)
			c.mu.Unlock()
			touch(e)
			if op := c.obs.Load(); op != nil {
				(*op).ObserveCompose(queryName, e.key, false)
			}
			return e, nil
		}
		c.remove(e)
	}
	e := newCacheEntry(mapKey, queryName, key, sql, binds)
	e.el = c.order.PushFront(e)
	c.m[mapKey] = e
	c.inserts++
	c.sqlBytes += int64(len(sql))
	c.totalBytes += entryBytes(e)
	// Evict on either bound: the count cap, or the byte ceiling (never
	// below one entry — the just-composed one must survive so the caller
	// receives it, even if it alone exceeds the ceiling).
	for len(c.m) > c.cap || (c.maxBytes > 0 && c.totalBytes > c.maxBytes && len(c.m) > 1) {
		c.evictOne()
	}
	c.publish()
	c.mu.Unlock()
	if op := c.obs.Load(); op != nil {
		(*op).ObserveCompose(queryName, e.key, false)
	}
	return e, nil
}

// touch records that an entry was used. The load guard matters: the bit
// stays set until eviction considers the entry, so an unconditional
// store would have every core repeatedly claiming the same cache line
// exclusively — the exact contention the lock-free read path exists to
// avoid. Reading a line that is already shared costs nothing.
func touch(e *cacheEntry) {
	if !e.ref.Load() {
		e.ref.Store(true)
	}
}

// remove drops an entry from both the map and the recency list. Callers
// hold c.mu. The entry's hit count folds into hitsFolded so cumulative
// hits survive eviction; hits racing this fold (lock-free path, stale
// snapshot) can drop from the cumulative total — an accepted skew,
// bounded per eviction by the goroutines concurrently hitting that
// entry, on a counter whose consumers read rates and ratios.
func (c *ComposedCache) remove(e *cacheEntry) {
	c.order.Remove(e.el)
	delete(c.m, e.mapKey)
	c.hitsFolded += e.hits.Load()
	c.sqlBytes -= int64(len(e.sql))
	c.totalBytes -= entryBytes(e)
	c.evictedSince = true
}

// evictOne discards one entry by second chance: an entry touched since
// it was last considered gets its bit cleared and moves to the front
// instead of being dropped. Callers hold c.mu.
func (c *ComposedCache) evictOne() {
	// Bounded so that entries being touched concurrently cannot keep
	// the hand spinning: after one full sweep the oldest goes
	// regardless.
	for i := len(c.m); i > 0; i-- {
		el := c.order.Back()
		if el == nil {
			return
		}
		e := el.Value.(*cacheEntry)
		if e.ref.Swap(false) {
			c.order.MoveToFront(el)
			continue
		}
		c.remove(e)
		c.evictions++
		return
	}
	if el := c.order.Back(); el != nil {
		c.remove(el.Value.(*cacheEntry))
		c.evictions++
	}
}

// publish snapshots the entry map for the lock-free read path. Callers
// hold c.mu.
func (c *ComposedCache) publish() {
	c.sinceSnap++
	// While the cache is merely filling up, every insert publishes, so
	// a steady-state workload ends up serving all of its shapes without
	// a lock. Once the shape set outgrows the capacity, publishing is
	// amortized: copying the map on every miss would otherwise make the
	// degenerate case far more expensive than the composition it
	// caches. The deferred snapshot is flushed either by a later insert
	// or, if inserts stop, by a hit that finds an unpublished entry
	// (see entry()'s rate-capped mutex-path flush: immediately while a
	// flush credit is armed, and within cap deferred hits regardless) —
	// so no entry is stranded off the lock-free path indefinitely.
	if c.evictedSince && c.sinceSnap < c.cap {
		return
	}
	c.publishNow()
}

// publishNow snapshots the entry map unconditionally, re-arms the
// amortization window, and grants the hit path one flush credit (a
// fresh snapshot means the next deferred hit is the "inserts have
// stopped" signal worth reacting to immediately; the hit path revokes
// the credit itself after a credited flush). Callers hold c.mu.
func (c *ComposedCache) publishNow() {
	snap := make(map[string]*cacheEntry, len(c.m))
	for k, v := range c.m {
		snap[k] = v
	}
	c.fast.Store(&snap)
	c.sinceSnap = 0
	c.evictedSince = false
	c.snapshots++
	c.pendingHits = 0
	c.hitFlushCredit = true
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
	out.Arities = append([]int32(nil), k.Arities...)
	return out
}

// keysEqual is the full-key comparison — hashes and encodings are an
// index, never identity.
func keysEqual(a, b ShapeKey) bool {
	if a.Guards != b.Guards || !choicesEqual(a.Choices, b.Choices) ||
		len(a.Orders) != len(b.Orders) || len(a.Trees) != len(b.Trees) ||
		len(a.Arities) != len(b.Arities) {
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
	for i := range a.Arities {
		if a.Arities[i] != b.Arities[i] {
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
