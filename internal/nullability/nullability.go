// Package nullability decides, per result column, whether the
// generated Go field must be a pointer — under the spec's
// per-shape-sound discipline: narrowing uses only the skeleton;
// guarded fragments NEVER narrow. See docs/design/05-nullability.md.
//
// Correctness invariant: if Analyze reports a column non-nullable, no
// reachable shape can return NULL there. False positives (nullable
// verdicts for always-present values) cost a pointer, never a panic.
package nullability

import (
	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/cache"
	"github.com/moznion/sqletch/internal/dialect"
	"github.com/moznion/sqletch/internal/template"
)

// funcWhitelist lists functions whose bare top-level call is total
// (never NULL) regardless of arguments. Additions require a per-shape
// soundness argument, not just "usually not null":
//   - count: always returns a bigint, 0 on empty input
//   - now:   always returns the transaction timestamp
//
// Deliberately absent: coalesce (null when ALL args are null —
// needs argument analysis), sum/min/max (NULL on empty input).
var funcWhitelist = map[string]bool{
	"count": true,
	"now":   true,
}

// Analyze returns nullable-ness per result column of desc.Columns.
//
// The discipline, mechanically:
//   - Guarded joins are ignored entirely. They are INNER/LEFT and
//     contribute no result columns (R2), so they can filter rows but
//     never null-extend a skeleton column. Reasoning "guarded INNER
//     join implies its FK is non-null" would be per-shape UNSOUND
//     (review counterexample F1b) — do not add it.
//   - No WHERE-based narrowing at all in v0.1, not even from skeleton
//     predicates (kept conservative; guarded-predicate narrowing is
//     never sound, F1a).
//   - Skeleton outer joins null-extend their side (RelRef.NullableSide,
//     computed by the frontend for LEFT/RIGHT/FULL).
func Analyze(maxTree dialect.Tree, maxR ast.Rendering, desc dialect.Desc,
	cat *cache.Catalog, overrides map[string]bool) []bool {

	// Table OIDs that appear on a null-extended side of a SKELETON
	// outer join. Guarded relations are excluded: their instance never
	// supplies result columns (R2), and a skeleton instance of the
	// same table must not inherit a guarded instance's properties.
	nullableSideOIDs := map[uint32]bool{}
	for _, rel := range maxTree.Relations() {
		if isGuarded(maxR, rel.Loc) {
			continue
		}
		if rel.NullableSide && cat != nil {
			if t := cat.Lookup(rel.Table); t != nil {
				nullableSideOIDs[t.OID] = true
			}
		}
	}

	targets := maxTree.TargetItems()
	out := make([]bool, len(desc.Columns))
	for i, col := range desc.Columns {
		if v, ok := overrides[col.Name]; ok {
			out[i] = v
			continue
		}
		// Direct column reference: catalog NOT NULL minus outer-join
		// null extension.
		if col.SrcRel != 0 && cat != nil {
			nonNull := false
			if t := cat.LookupOID(col.SrcRel); t != nil {
				if c := t.ColByAtt(col.SrcAtt); c != nil {
					nonNull = c.NotNull && !nullableSideOIDs[col.SrcRel]
				}
			}
			out[i] = !nonNull
			continue
		}
		// Expression column: nullable unless whitelisted total function.
		if i < len(targets) && funcWhitelist[targets[i].FuncName] {
			out[i] = false
			continue
		}
		out[i] = true
	}
	return out
}

// AnalyzeAll runs Analyze over every verified rendering and unions the
// results per column — the spec's nullable-most rule for @choose
// projection cases (a case may be nullable where another is not;
// review counterexample F1c). Renderings must already have passed the
// column-agreement check (equal lengths).
func AnalyzeAll(fe dialect.Frontend, rs []ast.Rendering, descs []dialect.Desc,
	cat *cache.Catalog, overrides map[string]bool) ([]bool, error) {

	var out []bool
	for i, r := range rs {
		if i >= len(descs) {
			break
		}
		tree, err := fe.Parse(r.SQL)
		if err != nil {
			return nil, err
		}
		n := Analyze(tree, r, descs[i], cat, overrides)
		if out == nil {
			out = n
			continue
		}
		for c := range out {
			if c < len(n) {
				out[c] = out[c] || n[c]
			}
		}
	}
	return out, nil
}

// isGuarded reports whether the rendered offset falls inside an
// @if-present fragment's emission.
func isGuarded(maxR ast.Rendering, loc int) bool {
	if loc < 0 {
		return false
	}
	for _, fr := range maxR.Frags {
		if loc >= fr.Start && loc < fr.End {
			_, ok := fr.Item.(*template.IfPresent)
			return ok
		}
	}
	return false
}
