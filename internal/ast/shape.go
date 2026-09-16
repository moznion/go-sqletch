package ast

import (
	"strconv"

	"github.com/moznion/go-sqletch/internal/template"
)

// assignShapes fills Rendering.Shape for a query's whole enumeration
// (docs/design/21-cache-layout.md §3). A rendering's shape name is the
// last path component of its committed cache entry, so it has to be
// stable, readable, and unique within its query — the three properties
// that make a cache diff reviewable.
//
// Names come from the construct that produced the rendering, never
// from its position in the enumeration: an @order-by added above a
// @choose must not rename the @choose's entries.
//
// Two constructs may legitimately carry the same parameter name (only
// @if-present guards are required to be unique), which would give two
// renderings the same name — and the same cache path. Colliding names
// are therefore disambiguated by block index, applied to EVERY member
// of the colliding group so that a name never depends on which of the
// two blocks came first in the file.
func assignShapes(q *template.QueryTemplate, rs []Rendering) {
	// Parameters by block, in template order — the same order the
	// enumeration's *Idx fields count in.
	var chooseParams, orderParams, inParams, treeParams []string
	var chooseCases [][]string // per @choose block, its case names
	for _, it := range q.Items {
		switch v := it.(type) {
		case *template.Choose:
			names := make([]string, len(v.Cases))
			for i, c := range v.Cases {
				names[i] = c.Name
			}
			chooseParams, chooseCases = append(chooseParams, v.Param), append(chooseCases, names)
		case *template.OrderBy:
			orderParams = append(orderParams, v.Param)
		case *template.InExpr:
			inParams = append(inParams, v.Param)
		case *template.FilterTree:
			treeParams = append(treeParams, v.Param)
		}
	}

	names := make([]string, len(rs))
	blocks := make([]int, len(rs))
	for i, r := range rs {
		switch r.Kind {
		case RenderMaximal:
			names[i], blocks[i] = "maximal", -1
		case RenderCase:
			names[i] = "case-" + paramAt(chooseParams, r.ChooseIdx) + "-" + caseNameAt(chooseCases, r.ChooseIdx, r.CaseIdx)
			blocks[i] = r.ChooseIdx
		case RenderOrderDefault:
			names[i], blocks[i] = "order-default-"+paramAt(orderParams, r.OrderIdx), r.OrderIdx
		case RenderInEmpty:
			names[i], blocks[i] = "in-empty-"+paramAt(inParams, r.InIdx), r.InIdx
		case RenderTreeEmpty:
			names[i], blocks[i] = "tree-empty-"+paramAt(treeParams, r.TreeIdx), r.TreeIdx
		}
	}

	count := map[string]int{}
	for _, n := range names {
		count[n]++
	}
	for i := range rs {
		if count[names[i]] > 1 {
			names[i] += "-" + strconv.Itoa(blocks[i])
		}
		rs[i].Shape = names[i]
	}
}

// paramAt and caseNameAt tolerate an out-of-range index rather than
// panicking: a shape name is a file name, and no naming defect is
// worth crashing a compile over. The enumeration never produces one.

func paramAt(params []string, i int) string {
	if i < 0 || i >= len(params) || params[i] == "" {
		return "_"
	}
	return params[i]
}

// caseNameAt names one @choose ordinal. Ordinals past the declared
// cases are the block's @default, which has no name of its own.
func caseNameAt(cases [][]string, block, ord int) string {
	if block < 0 || block >= len(cases) {
		return strconv.Itoa(ord)
	}
	if ord >= 0 && ord < len(cases[block]) && cases[block][ord] != "" {
		return cases[block][ord]
	}
	return "default"
}
