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
	Choices []uint8 // one per @choose block, document order
}

// String is the canonical, stable encoding used for caches and logs.
func (k Key) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "g=%x", k.Guards)
	if len(k.Choices) > 0 {
		b.WriteString(";c=")
		for i, c := range k.Choices {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "%d", c)
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

func chooseBlocks(q *template.QueryTemplate) []*template.Choose {
	var out []*template.Choose
	for _, it := range q.Items {
		if c, ok := it.(*template.Choose); ok {
			out = append(out, c)
		}
	}
	return out
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

// Count returns the exact number of reachable shapes.
func Count(q *template.QueryTemplate) *big.Int {
	n := new(big.Int).Lsh(big.NewInt(1), uint(len(q.GuardAtoms)))
	for _, c := range chooseBlocks(q) {
		n.Mul(n, big.NewInt(int64(ordinals(c))))
	}
	return n
}

// Enumerate yields every shape key in a deterministic order, stopping
// after cap keys. truncated reports whether the cap cut enumeration
// short. cap <= 0 means no cap.
func Enumerate(q *template.QueryTemplate, capN int) (keys []Key, truncated bool) {
	blocks := chooseBlocks(q)
	ords := make([]int, len(blocks))
	for i, c := range blocks {
		ords[i] = ordinals(c)
	}
	nGuards := len(q.GuardAtoms)

	choices := make([]uint8, len(blocks))
	var walk func(block int, guards uint64) bool // false = stop
	emit := func(guards uint64) bool {
		if capN > 0 && len(keys) >= capN {
			truncated = true
			return false
		}
		k := Key{Guards: guards, Choices: append([]uint8(nil), choices...)}
		keys = append(keys, k)
		return true
	}
	walk = func(block int, guards uint64) bool {
		if block == len(blocks) {
			return emit(guards)
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
