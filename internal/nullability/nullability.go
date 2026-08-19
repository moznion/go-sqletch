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

// strictAggs are aggregates that return non-NULL whenever their
// argument is non-NULL on every input row AND every output row's
// group is non-empty — i.e. under a statement-level GROUP BY without
// grouping sets. Without GROUP BY the empty input yields one NULL
// row; with grouping sets the () set aggregates a possibly-empty
// input. FILTER clauses empty a group's aggregated input and OVER
// changes the semantics entirely — TargetItem.AggArg is nil for both.
var strictAggs = map[string]bool{
	"sum": true,
	"min": true,
	"max": true,
	"avg": true,
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

	// Statement-wide kill-switches no recursion can lift: a top-level
	// set operation (engines may attribute the output to a branch's
	// base table — SQLite: the FIRST branch), top-level grouping sets
	// (super-aggregate rows null grouping columns), and name-keyed
	// attribution defeated by database qualifiers (MySQL/SQLite).
	untrusted := ""
	switch {
	case maxTree.HasSetOperation():
		untrusted = "narrowing disabled: statement is a set operation"
	case maxTree.HasGroupingSets():
		untrusted = "narrowing disabled: ROLLUP/CUBE/GROUPING SETS nulls grouping columns"
	case maxTree.HasUnresolvableProvenance():
		untrusted = "narrowing disabled: database-qualified names defeat name-keyed column attribution"
	}
	trustSrc := untrusted == ""

	// present: source OIDs accounted for by skeleton FROM relations —
	// now RECURSIVELY (design 05 §2b): derived tables and referenced
	// CTE definitions contribute their own relations, with the
	// enclosing side's null-extension compounded in, and hazardous
	// subqueries (set ops, grouping sets, recursive CTEs) POISON every
	// table they mention instead of distrusting the whole statement.
	// Guarded relations are excluded where locations allow: their
	// instance never supplies result columns (R2), and a skeleton
	// instance of the same table must not inherit a guarded instance's
	// properties.
	present := map[uint32]*presence{}
	res := &instanceResolver{}
	if trustSrc && cat != nil {
		collectPresence(maxTree, nil, maxR, cat, false, present, res, true)
	}

	// Skeleton `col IS NOT NULL` conjuncts narrow the filtered column
	// past null-extension and catalog nullability: WHERE runs after
	// the joins, and a skeleton conjunct is present in every shape
	// (guarded-predicate narrowing stays forbidden — F1a). The
	// (SrcRel, SrcAtt) key cannot tell two instances of the same
	// table apart, so narrowing additionally requires the table to
	// have exactly ONE instance ACROSS ALL LEVELS and no poisoned
	// exposure (a hazardous subquery mentioning the table shares its
	// attribution key).
	filtered := map[srcKey]bool{}
	if trustSrc && cat != nil {
		for _, cr := range maxTree.NotNullConjuncts() {
			if !inSkeleton(maxR, cr.Loc) {
				continue
			}
			inst, c, ok := res.resolve(cr.Fields)
			if !ok {
				continue
			}
			if p := present[inst.table.OID]; p == nil || p.count != 1 || p.poisoned {
				continue
			}
			filtered[srcKey{rel: inst.table.OID, att: c.Att}] = true
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

	// Strict aggregates narrow only when every output row's group is
	// non-empty: a statement-level GROUP BY with no grouping sets
	// (trustSrc already excludes the latter).
	groupedAggs := trustSrc && maxTree.HasGroupBy()

	out := make([]Verdict, len(desc.Columns))
	for i, col := range desc.Columns {
		if v, ok := overrides[col.Name]; ok {
			out[i] = Verdict{Nullable: v, Reason: "forced by null_overrides"}
			continue
		}
		// Direct column reference: catalog NOT NULL, provided the
		// source relation is trusted, present, and not null-extended.
		if col.SrcRel != 0 && cat != nil {
			out[i] = srcVerdict(col, cat, trustSrc, untrusted, present, filtered)
			continue
		}
		// Expression column: nullable unless provably total.
		if aligned {
			ti := targets[i]
			switch {
			case funcWhitelist[ti.FuncName]:
				out[i] = Verdict{Nullable: false,
					Reason: "total function " + ti.FuncName + "()"}
				continue
			case ti.Total:
				out[i] = Verdict{Nullable: false,
					Reason: "total expression (literal/EXISTS/null test/total coalesce)"}
				continue
			case groupedAggs && strictAggs[ti.FuncName] && ti.AggArg != nil:
				if inst, c, ok := res.resolve(ti.AggArg); ok &&
					!inst.NullableSide && c.NotNull &&
					!(inst.table.HasChildren && !inst.Only) {
					out[i] = Verdict{Nullable: false,
						Reason: ti.FuncName + "(" + inst.table.Name + "." + c.Name +
							") over a non-null column with GROUP BY"}
					continue
				}
			}
		}
		out[i] = Verdict{Nullable: true,
			Reason: "expression column without a totality proof"}
	}
	return out
}

// srcKey identifies a described column's source: the engine's
// (relation OID, attribute) attribution.
type srcKey struct {
	rel uint32
	att int16
}

// instance is one named, non-guarded skeleton FROM relation resolved
// against the catalog.
type instance struct {
	dialect.RelRef
	table *cache.Table
}

// instanceResolver answers "which TOP-LEVEL relation instance and
// catalog column does this (possibly qualified) column path name?" —
// alias-first for qualified paths, unique-candidate for bare names.
// Only top-level relations enter it: aggregate arguments and WHERE
// conjuncts belong to the outer scope. Cross-level instance COUNTS
// live in the presence map instead.
type instanceResolver struct {
	rels []instance
}

func (r *instanceResolver) add(rel dialect.RelRef, t *cache.Table) {
	r.rels = append(r.rels, instance{RelRef: rel, table: t})
}

func (r *instanceResolver) resolve(fields []string) (instance, *cache.Column, bool) {
	switch len(fields) {
	case 1:
		var found instance
		var col *cache.Column
		n := 0
		for _, inst := range r.rels {
			if c := inst.table.Col(fields[0]); c != nil {
				found, col = inst, c
				n++
			}
		}
		if n == 1 {
			return found, col, true
		}
	case 2:
		for _, inst := range r.rels {
			name := inst.Alias
			if name == "" {
				name = inst.Table
			}
			if name != fields[0] {
				continue
			}
			if c := inst.table.Col(fields[1]); c != nil {
				return inst, c, true
			}
			return instance{}, nil, false
		}
	}
	return instance{}, nil, false
}

// srcVerdict decides a source-attributed column under the provenance
// discipline, spelling out which gate stopped the narrowing.
func srcVerdict(col dialect.ColumnDesc, cat *cache.Catalog,
	trustSrc bool, untrusted string, present map[uint32]*presence,
	filtered map[srcKey]bool) Verdict {

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
	// A skeleton IS NOT NULL conjunct runs after the joins: it beats
	// both null-extension and catalog nullability.
	if filtered[srcKey{rel: col.SrcRel, att: col.SrcAtt}] {
		return Verdict{Nullable: false,
			Reason: qual + " is filtered by an unconditional IS NOT NULL"}
	}
	if p.poisoned {
		return Verdict{Nullable: true,
			Reason: qual + " is exposed through a hazardous subquery (set operation, grouping sets, or a recursive CTE)"}
	}
	if p.inherited {
		return Verdict{Nullable: true,
			Reason: qual + "'s table has inheritance children, which may drop NOT NULL"}
	}
	if p.nullExtended {
		return Verdict{Nullable: true,
			Reason: qual + " is null-extended by an outer join"}
	}
	if !c.NotNull {
		return Verdict{Nullable: true, Reason: qual + " is nullable in the catalog"}
	}
	return Verdict{Nullable: false, Reason: qual + " is NOT NULL in the catalog"}
}

// inSkeleton reports whether a rendered offset lies in skeleton text:
// covered by NO fragment. Fragments carry every conditional emission
// (@if-present, @when, @choose cases, @filter-tree, woven policies) —
// only bare skeleton bytes are outside all of them, and only those
// are present in every shape.
func inSkeleton(maxR ast.Rendering, loc int) bool {
	if loc < 0 {
		return false
	}
	for _, fr := range maxR.Frags {
		if loc >= fr.Start && loc < fr.End {
			return false
		}
	}
	return true
}

// presence is one accounted-for source OID with its aggregated
// hazards across every path that exposes it: null-extension (its own
// side OR any enclosing derived/CTE's side), a non-ONLY scan of a
// plain-inheritance parent, and poisoning by a hazardous subquery.
// count is the number of relation instances across all levels.
type presence struct {
	nullExtended bool
	inherited    bool
	poisoned     bool
	count        int
}

// collectPresence walks one statement level: catalog relations join
// the presence map (hazards merged), CTE references and derived
// tables recurse with the enclosing null-extension compounded, and
// hazardous sub-levels poison instead of contributing (design 05
// §2b). env carries the CTE definitions visible at this level; a
// definition's own name is removed while inside its body (a
// non-recursive CTE cannot legally self-reference, and WITH RECURSIVE
// is poisoned before recursion).
func collectPresence(t dialect.Tree, env map[string]dialect.CTEDef,
	maxR ast.Rendering, cat *cache.Catalog, nullExt bool,
	present map[uint32]*presence, res *instanceResolver, top bool) {

	ctes := t.CTEs()
	if len(ctes) > 0 {
		env2 := make(map[string]dialect.CTEDef, len(env)+len(ctes))
		for k, v := range env {
			env2[k] = v
		}
		for _, def := range ctes {
			env2[def.Name] = def
		}
		env = env2
	}

	for _, rel := range t.Relations() {
		if isGuarded(maxR, rel.Loc) || rel.Table == "" {
			continue
		}
		if def, ok := env[rel.Table]; ok && rel.Schema == "" {
			sub := make(map[string]dialect.CTEDef, len(env))
			for k, v := range env {
				if k != def.Name {
					sub[k] = v
				}
			}
			descend(def.Tree, def.Recursive, sub, maxR, cat,
				nullExt || rel.NullableSide, present, res)
			continue
		}
		tbl := cat.LookupQualified(rel.Schema, rel.Table)
		if tbl == nil {
			continue
		}
		p := present[tbl.OID]
		if p == nil {
			p = &presence{}
			present[tbl.OID] = p
		}
		p.count++
		if nullExt || rel.NullableSide {
			p.nullExtended = true
		}
		// A plain-inheritance parent's NOT NULL is not enforced on
		// its children (PG 16, proven by the adversarial suite) —
		// unless the reference is FROM ONLY, its scan can return NULL
		// where the catalog says otherwise.
		if tbl.HasChildren && !rel.Only {
			p.inherited = true
		}
		if top {
			res.add(rel, tbl)
		}
	}

	for _, d := range t.DerivedRels() {
		descend(d.Tree, false, env, maxR, cat, nullExt || d.NullableSide, present, res)
	}
}

// descend enters one subquery. A nil tree is a data-modifying CTE:
// it exposes only RETURNING rows of its target table, whose
// constraints were enforced at write time — grant nothing, poison
// nothing. A hazardous body (recursive CTE, set operation, grouping
// sets) poisons every table it mentions: its output can carry those
// tables' attribution while breaking their declared constraints
// (SQLite attributes compound output to the FIRST branch's table;
// grouping sets null grouping columns).
func descend(sub dialect.Tree, recursive bool, env map[string]dialect.CTEDef,
	maxR ast.Rendering, cat *cache.Catalog, nullExt bool,
	present map[uint32]*presence, res *instanceResolver) {

	if sub == nil {
		return
	}
	if recursive || sub.HasSetOperation() || sub.HasGroupingSets() {
		for _, tr := range sub.DeepTables() {
			if tbl := cat.LookupQualified(tr.Schema, tr.Name); tbl != nil {
				p := present[tbl.OID]
				if p == nil {
					p = &presence{}
					present[tbl.OID] = p
				}
				p.poisoned = true
			}
		}
		return
	}
	collectPresence(sub, env, maxR, cat, nullExt, present, res, false)
}

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
