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
	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/cache"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/template"
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
//   - SrcRel provenance is trusted only when the source relation is
//     visibly PRESENT in the statement's own FROM list (schema-aware)
//     and no construct can smuggle provenance past Relations():
//     engines attribute columns THROUGH derived tables, CTEs, and
//     (on some engines) views to base tables, and grouping sets null
//     out grouping columns outright. Every one of those was a proven
//     NULL-into-value counterexample before this check — see
//     TestNullabilitySoundnessAdversarial. An unrecognized or
//     unresolvable source OID therefore fails SAFE to nullable.
func Analyze(maxTree dialect.Tree, maxR ast.Rendering, desc dialect.Desc,
	cat *cache.Catalog, overrides map[string]bool) []bool {

	// trustSrc: with a derived table/CTE/set operation or a grouping
	// set anywhere relevant, no SrcRel narrowing at all — even a
	// directly-present table can be re-attributed through the opaque
	// construct (e.g. the same table both joined directly and wrapped
	// in a null-extended derived table).
	trustSrc := !maxTree.HasOpaqueProvenance() && !maxTree.HasGroupingSets()

	// present: source OIDs accounted for by SKELETON FROM relations,
	// with their aggregated null-extension. Guarded relations are
	// excluded: their instance never supplies result columns (R2), and
	// a skeleton instance of the same table must not inherit a guarded
	// instance's properties.
	type presence struct{ nullExtended bool }
	present := map[uint32]*presence{}
	if cat != nil {
		for _, rel := range maxTree.Relations() {
			if isGuarded(maxR, rel.Loc) || rel.Table == "" {
				continue
			}
			t := cat.LookupQualified(rel.Schema, rel.Table)
			if t == nil {
				continue
			}
			p := present[t.OID]
			if p == nil {
				p = &presence{}
				present[t.OID] = p
			}
			if rel.NullableSide {
				p.nullExtended = true
			}
		}
	}

	// The expression-column whitelist matches desc columns to target
	// items by index, which only holds without star expansion and with
	// equal lengths.
	targets := maxTree.TargetItems()
	aligned := len(targets) == len(desc.Columns)
	for _, ti := range targets {
		if ti.Star {
			aligned = false
		}
	}

	out := make([]bool, len(desc.Columns))
	for i, col := range desc.Columns {
		if v, ok := overrides[col.Name]; ok {
			out[i] = v
			continue
		}
		// Direct column reference: catalog NOT NULL, provided the
		// source relation is trusted, present, and not null-extended.
		if col.SrcRel != 0 && cat != nil {
			nonNull := false
			if p := present[col.SrcRel]; trustSrc && p != nil && !p.nullExtended {
				if t := cat.LookupOID(col.SrcRel); t != nil {
					if c := t.ColByAtt(col.SrcAtt); c != nil {
						nonNull = c.NotNull
					}
				}
			}
			out[i] = !nonNull
			continue
		}
		// Expression column: nullable unless whitelisted total function.
		if aligned && funcWhitelist[targets[i].FuncName] {
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
