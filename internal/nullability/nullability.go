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

	vs := AnalyzeVerdicts(maxTree, maxR, desc, cat, overrides)
	out := make([]bool, len(vs))
	for i, v := range vs {
		out[i] = v.Nullable
	}
	return out
}

// Verdict couples the per-column decision with the reason it was
// reached — surfaced by `explain` so a conservative verdict is
// auditable (and a null_overrides entry can be written with
// confidence) instead of opaque.
type Verdict struct {
	Nullable bool
	Reason   string
}

// AnalyzeVerdicts is Analyze with reasons.
func AnalyzeVerdicts(maxTree dialect.Tree, maxR ast.Rendering, desc dialect.Desc,
	cat *cache.Catalog, overrides map[string]bool) []Verdict {

	// Statement-wide kill-switches: with a derived table/CTE/set
	// operation or a grouping set anywhere relevant, no SrcRel
	// narrowing at all — even a directly-present table can be
	// re-attributed through the opaque construct (e.g. the same table
	// both joined directly and wrapped in a null-extended derived
	// table).
	untrusted := ""
	switch {
	case maxTree.HasOpaqueProvenance():
		untrusted = "narrowing disabled: statement contains a derived table, CTE, or set operation"
	case maxTree.HasGroupingSets():
		untrusted = "narrowing disabled: ROLLUP/CUBE/GROUPING SETS nulls grouping columns"
	}
	trustSrc := untrusted == ""

	// present: source OIDs accounted for by SKELETON FROM relations,
	// with their aggregated null-extension. Guarded relations are
	// excluded: their instance never supplies result columns (R2), and
	// a skeleton instance of the same table must not inherit a guarded
	// instance's properties.
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

	out := make([]Verdict, len(desc.Columns))
	for i, col := range desc.Columns {
		if v, ok := overrides[col.Name]; ok {
			out[i] = Verdict{Nullable: v, Reason: "forced by null_overrides"}
			continue
		}
		// Direct column reference: catalog NOT NULL, provided the
		// source relation is trusted, present, and not null-extended.
		if col.SrcRel != 0 && cat != nil {
			out[i] = srcVerdict(col, cat, trustSrc, untrusted, present)
			continue
		}
		// Expression column: nullable unless whitelisted total function.
		if aligned && funcWhitelist[targets[i].FuncName] {
			out[i] = Verdict{Nullable: false,
				Reason: "total function " + targets[i].FuncName + "()"}
			continue
		}
		out[i] = Verdict{Nullable: true,
			Reason: "expression column without a totality proof"}
	}
	return out
}

// srcVerdict decides a source-attributed column under the provenance
// discipline, spelling out which gate stopped the narrowing.
func srcVerdict(col dialect.ColumnDesc, cat *cache.Catalog,
	trustSrc bool, untrusted string, present map[uint32]*presence) Verdict {

	if !trustSrc {
		return Verdict{Nullable: true, Reason: untrusted}
	}
	p := present[col.SrcRel]
	if p == nil {
		return Verdict{Nullable: true,
			Reason: "source relation is not present in FROM (untrusted provenance)"}
	}
	t := cat.LookupOID(col.SrcRel)
	if t == nil {
		return Verdict{Nullable: true, Reason: "source relation unknown to the catalog"}
	}
	c := t.ColByAtt(col.SrcAtt)
	if c == nil {
		return Verdict{Nullable: true, Reason: "source column unknown to the catalog"}
	}
	qual := t.Name + "." + c.Name
	if p.nullExtended {
		return Verdict{Nullable: true,
			Reason: qual + " is null-extended by an outer join"}
	}
	if !c.NotNull {
		return Verdict{Nullable: true, Reason: qual + " is nullable in the catalog"}
	}
	return Verdict{Nullable: false, Reason: qual + " is NOT NULL in the catalog"}
}

// presence is one accounted-for source OID: seen in the skeleton FROM
// list, with its aggregated null-extension.
type presence struct{ nullExtended bool }

// AnalyzeAll runs Analyze over every verified rendering and unions the
// results per column — the spec's nullable-most rule for @choose
// projection cases (a case may be nullable where another is not;
// review counterexample F1c). Renderings must already have passed the
// column-agreement check (equal lengths).
func AnalyzeAll(fe dialect.Frontend, rs []ast.Rendering, descs []dialect.Desc,
	cat *cache.Catalog, overrides map[string]bool) ([]bool, error) {

	vs, err := AnalyzeAllVerdicts(fe, rs, descs, cat, overrides)
	if err != nil {
		return nil, err
	}
	out := make([]bool, len(vs))
	for i, v := range vs {
		out[i] = v.Nullable
	}
	return out, nil
}

// AnalyzeAllVerdicts is AnalyzeAll with reasons: the union keeps the
// first rendering's reason while a column stays non-nullable and
// adopts the flipping rendering's reason when one turns it nullable.
func AnalyzeAllVerdicts(fe dialect.Frontend, rs []ast.Rendering, descs []dialect.Desc,
	cat *cache.Catalog, overrides map[string]bool) ([]Verdict, error) {

	var out []Verdict
	for i, r := range rs {
		if i >= len(descs) {
			break
		}
		tree, err := fe.Parse(r.SQL)
		if err != nil {
			return nil, err
		}
		n := AnalyzeVerdicts(tree, r, descs[i], cat, overrides)
		if out == nil {
			out = n
			continue
		}
		for c := range out {
			if c < len(n) && !out[c].Nullable && n[c].Nullable {
				out[c] = n[c]
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
