// Package shape computes and enumerates the reachable query shapes of
// a template: guard bitmask × one ordinal per @choose block. See
// docs/design/03-structural-rules.md §9.
package shape

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/template"
)

// Key identifies one concrete shape.
type Key struct {
	Guards  uint64
	Choices []uint8   // one per @choose block, document order
	Orders  [][]uint8 // one per @order-by block: key<<1|desc sequence
	// (nil inner slice = maximal/all keys; empty = default-or-omit)

	// Ins holds one representative arity per @in construct on
	// expanding dialects: 1 stands for every non-empty list (adding an
	// element to an IN list is parse-invariant), 0 is the distinct
	// empty-list rendering. Empty on PostgreSQL.
	Ins []uint8
}

// String is the canonical, stable encoding used for caches and logs.
// It must stay byte-identical to runtime.ShapeKey.String.
func (k Key) String() string {
	s := EncodeKey(k.Guards, k.Choices, k.Orders)
	if len(k.Ins) > 0 {
		var b strings.Builder
		b.WriteString(s)
		b.WriteString(";n=")
		for i, n := range k.Ins {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "%d", n)
		}
		s = b.String()
	}
	return s
}

// EncodeKey is the shared canonical encoding (also used by the runtime
// mirror): "g=<hex>[;c=<ords>][;o=<seq>|<seq>…][;n=<arities>]" where a
// sequence is "*" (maximal), "-" (default/omit), or elements like
// "1a,0d".
func EncodeKey(guards uint64, choices []uint8, orders [][]uint8) string {
	var b strings.Builder
	fmt.Fprintf(&b, "g=%x", guards)
	if len(choices) > 0 {
		b.WriteString(";c=")
		for i, c := range choices {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "%d", c)
		}
	}
	if len(orders) > 0 {
		b.WriteString(";o=")
		for i, seq := range orders {
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
					dir := "a"
					if e&1 == 1 {
						dir = "d"
					}
					fmt.Fprintf(&b, "%d%s", e>>1, dir)
				}
			}
		}
	}
	return b.String()
}

// Selection converts the key's choices into the renderer's form.
func (k Key) Selection() ast.CaseSelection {
	sel := ast.CaseSelection{}
	for i, c := range k.Choices {
		sel[i] = int(c)
	}
	return sel
}

// OrderSelection converts the key's order sequences into the
// renderer's form.
func (k Key) OrderSelection() ast.OrderSelection {
	return ast.OrderSelection(k.Orders)
}

// InSelection converts the key's @in arities into the renderer's form.
func (k Key) InSelection() ast.InSelection {
	return ast.InSelection(k.Ins)
}

// Arities converts the key's representative arities to the runtime
// key form.
func (k Key) Arities() []int32 {
	if len(k.Ins) == 0 {
		return nil
	}
	out := make([]int32, len(k.Ins))
	for i, n := range k.Ins {
		out[i] = int32(n)
	}
	return out
}

func inBlocks(q *template.QueryTemplate) int {
	n := 0
	for _, it := range q.Items {
		if _, ok := it.(*template.InExpr); ok {
			n++
		}
	}
	return n
}

func chooseBlocks(q *template.QueryTemplate) []*template.Choose {
	var out []*template.Choose
	for _, it := range q.Items {
		if c, ok := it.(*template.Choose); ok {
			out = append(out, c)
		}
	}
	return out
}

func orderBlocks(q *template.QueryTemplate) []*template.OrderBy {
	var out []*template.OrderBy
	for _, it := range q.Items {
		if o, ok := it.(*template.OrderBy); ok {
			out = append(out, o)
		}
	}
	return out
}

// orderOptions enumerates one @order-by block's selections: the empty
// selection (default/omit) plus every ordered subset of keys with
// per-key directions. Emission stops when emit returns false.
func orderOptions(numKeys int, emit func([]uint8) bool) bool {
	if !emit([]uint8{}) {
		return false
	}
	cur := make([]uint8, 0, numKeys)
	var used uint32
	var rec func() bool
	rec = func() bool {
		for k := range numKeys {
			if used&(1<<uint(k)) != 0 {
				continue
			}
			used |= 1 << uint(k)
			for _, d := range []uint8{0, 1} {
				cur = append(cur, uint8(k)<<1|d)
				if !emit(append([]uint8(nil), cur...)) {
					return false
				}
				if !rec() {
					return false
				}
				cur = cur[:len(cur)-1]
			}
			used &^= 1 << uint(k)
		}
		return true
	}
	return rec()
}

func ordinals(c *template.Choose) int {
	n := len(c.Cases)
	if c.Default != nil {
		n++
	}
	if n == 0 {
		n = 1 // defensive; the scanner rejects case-less @choose
	}
	return n
}

// Count returns the exact number of reachable shapes (PostgreSQL
// view: @in contributes no dimension).
func Count(q *template.QueryTemplate) *big.Int {
	return CountExpand(q, false)
}

// CountExpand counts shapes; with expandIn each @in contributes the
// two representative arities (non-empty, empty). The true runtime
// arity space is unbounded — verification quotients it to these two
// classes because IN-list growth is parse-invariant.
func CountExpand(q *template.QueryTemplate, expandIn bool) *big.Int {
	n := new(big.Int).Lsh(big.NewInt(1), uint(len(q.GuardAtoms)))
	for _, c := range chooseBlocks(q) {
		n.Mul(n, big.NewInt(int64(ordinals(c))))
	}
	for _, o := range orderBlocks(q) {
		n.Mul(n, orderCount(len(o.Keys)))
	}
	if expandIn {
		n.Lsh(n, uint(inBlocks(q)))
	}
	return n
}

// orderCount = 1 (empty) + Σ_{k=1..n} P(n,k)·2^k — ordered subsets of
// keys with per-key directions.
func orderCount(n int) *big.Int {
	total := big.NewInt(1)
	perm := big.NewInt(1)
	pow2 := big.NewInt(1)
	for k := 1; k <= n; k++ {
		perm.Mul(perm, big.NewInt(int64(n-k+1)))
		pow2.Lsh(pow2, 1)
		term := new(big.Int).Mul(perm, pow2)
		total.Add(total, term)
	}
	return total
}

// Enumerate yields every shape key in a deterministic order, stopping
// after cap keys. truncated reports whether the cap cut enumeration
// short. cap <= 0 means no cap. @order-by blocks contribute their full
// selection space (empty, every ordered subset, every direction mix).
// @in contributes no dimension (the PostgreSQL view); expanding
// dialects use EnumerateExpand.
func Enumerate(q *template.QueryTemplate, capN int) (keys []Key, truncated bool) {
	return EnumerateExpand(q, capN, false)
}

// EnumerateExpand is Enumerate with an optional @in dimension: with
// expandIn each @in contributes its two representative arities
// (1 = any non-empty list, 0 = the empty-list rendering).
func EnumerateExpand(q *template.QueryTemplate, capN int, expandIn bool) (keys []Key, truncated bool) {
	blocks := chooseBlocks(q)
	oBlocks := orderBlocks(q)
	ords := make([]int, len(blocks))
	for i, c := range blocks {
		ords[i] = ordinals(c)
	}
	nGuards := len(q.GuardAtoms)
	nIns := 0
	if expandIn {
		nIns = inBlocks(q)
	}

	choices := make([]uint8, len(blocks))
	orders := make([][]uint8, len(oBlocks))
	ins := make([]uint8, nIns)
	emit := func(guards uint64) bool {
		if capN > 0 && len(keys) >= capN {
			truncated = true
			return false
		}
		k := Key{Guards: guards, Choices: append([]uint8(nil), choices...)}
		if len(orders) > 0 {
			k.Orders = make([][]uint8, len(orders))
			copy(k.Orders, orders)
		}
		if nIns > 0 {
			k.Ins = append([]uint8(nil), ins...)
		}
		keys = append(keys, k)
		return true
	}
	var walkIns func(block int, guards uint64) bool
	walkIns = func(block int, guards uint64) bool {
		if block == nIns {
			return emit(guards)
		}
		for _, n := range []uint8{1, 0} {
			ins[block] = n
			if !walkIns(block+1, guards) {
				return false
			}
		}
		return true
	}
	var walkOrders func(block int, guards uint64) bool
	walkOrders = func(block int, guards uint64) bool {
		if block == len(oBlocks) {
			return walkIns(0, guards)
		}
		return orderOptions(len(oBlocks[block].Keys), func(seq []uint8) bool {
			orders[block] = seq
			return walkOrders(block+1, guards)
		})
	}
	var walk func(block int, guards uint64) bool // false = stop
	walk = func(block int, guards uint64) bool {
		if block == len(blocks) {
			return walkOrders(0, guards)
		}
		for o := 0; o < ords[block]; o++ {
			choices[block] = uint8(o)
			if !walk(block+1, guards) {
				return false
			}
		}
		return true
	}
	// Guard masks in ascending numeric order; for each, all case combos.
	total := uint64(1) << uint(nGuards)
	for g := uint64(0); ; g++ {
		if nGuards < 64 && g >= total {
			break
		}
		if !walk(0, g) {
			return keys, truncated
		}
		if nGuards >= 64 && g == ^uint64(0) {
			break
		}
	}
	return keys, truncated
}
