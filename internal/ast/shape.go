package ast

import (
	"strconv"

	"github.com/moznion/go-sqletch/internal/template"
)

// Shape names (docs/design/21-cache-layout.md §3). A rendering's shape
// name is the last path component of its committed cache entry, so it
// has to be stable, readable, and unique within its query — the three
// properties that make a cache diff reviewable.
//
// Names come from the construct that produced the rendering, never
// from its position in the enumeration: an @order-by added above a
// @choose must not rename the @choose's entries.
const (
	shapeMaximal      = "maximal"
	shapeCasePrefix   = "case-"
	shapeOrderPrefix  = "order-default-"
	shapeInPrefix     = "in-empty-"
	shapeTreePrefix   = "tree-empty-"
	shapeDefaultCase  = "default"
	shapeUnnamedBlock = "_"
)

// assignShapes fills Rendering.Shape for a query's whole enumeration.
//
// Two constructs may legitimately carry the same parameter name (only
// @if-present guards are required to be unique), which would give two
// renderings the same name — and the same cache path. Colliding names
// are therefore disambiguated by block index, applied to EVERY member
// of the colliding group so that a name never depends on which of the
// two blocks came first in the file.
func assignShapes(q *template.QueryTemplate, rs []Rendering) {
	var (
		chooses []*template.Choose
		orders  []*template.OrderBy
		ins     []*template.InExpr
		trees   []*template.FilterTree
	)
	for _, it := range q.Items {
		switch v := it.(type) {
		case *template.Choose:
			chooses = append(chooses, v)
		case *template.OrderBy:
			orders = append(orders, v)
		case *template.InExpr:
			ins = append(ins, v)
		case *template.FilterTree:
			trees = append(trees, v)
		}
	}

	names := make([]string, len(rs))
	blocks := make([]int, len(rs))
	for i, r := range rs {
		switch r.Kind {
		case RenderMaximal:
			names[i], blocks[i] = shapeMaximal, -1
		case RenderCase:
			names[i] = shapeCasePrefix + chooseParam(chooses, r.ChooseIdx) + "-" + chooseCaseName(chooses, r.ChooseIdx, r.CaseIdx)
			blocks[i] = r.ChooseIdx
		case RenderOrderDefault:
			names[i] = shapeOrderPrefix + orderParam(orders, r.OrderIdx)
			blocks[i] = r.OrderIdx
		case RenderInEmpty:
			names[i] = shapeInPrefix + inParam(ins, r.InIdx)
			blocks[i] = r.InIdx
		case RenderTreeEmpty:
			names[i] = shapeTreePrefix + treeParam(trees, r.TreeIdx)
			blocks[i] = r.TreeIdx
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

// The param accessors tolerate an out-of-range index rather than
// panicking: a shape name is a file name, and no naming defect is
// worth crashing a compile over. The enumeration never produces one.

func chooseParam(cs []*template.Choose, i int) string {
	if i < 0 || i >= len(cs) || cs[i].Param == "" {
		return shapeUnnamedBlock
	}
	return cs[i].Param
}

func chooseCaseName(cs []*template.Choose, i, ord int) string {
	if i < 0 || i >= len(cs) {
		return strconv.Itoa(ord)
	}
	if ord >= 0 && ord < len(cs[i].Cases) {
		if n := cs[i].Cases[ord].Name; n != "" {
			return n
		}
	}
	return shapeDefaultCase
}

func orderParam(os []*template.OrderBy, i int) string {
	if i < 0 || i >= len(os) || os[i].Param == "" {
		return shapeUnnamedBlock
	}
	return os[i].Param
}

func inParam(is []*template.InExpr, i int) string {
	if i < 0 || i >= len(is) || is[i].Param == "" {
		return shapeUnnamedBlock
	}
	return is[i].Param
}

func treeParam(ts []*template.FilterTree, i int) string {
	if i < 0 || i >= len(ts) || ts[i].Param == "" {
		return shapeUnnamedBlock
	}
	return ts[i].Param
}
