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
	case touchesView(maxTree, cat):
		untrusted = "narrowing disabled: a view in the statement attributes result columns through to base tables whose NOT NULL its (invisible, possibly null-extending) body need not preserve"
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
		// memo caches each descended sub-level's presence contribution so a
		// CTE body referenced N times is analyzed once, not re-descended per
		// reference (which is exponential for chained CTEs — a DoS). See
		// descend / mergePresence for why the cache is result-preserving.
		memo := map[memoKey]map[uint32]presence{}
		collectPresence(maxTree, nil, maxR, cat, false, present, res, true, memo)
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
	//
	// UPDATE … RETURNING is EXEMPT wholesale. The "WHERE runs after the
	// joins" premise holds for SELECT/DELETE, but an UPDATE's WHERE
	// tests the OLD row while RETURNING yields the NEW one, so
	// `UPDATE t SET c=NULL WHERE c IS NOT NULL RETURNING c` narrows c to
	// non-null yet returns NULL. The target relation's columns are the
	// ones RETURNING can mutate; FROM-joined relations in an UPDATE are
	// read-only and would stay soundly narrowable, but the shared
	// analyzer has no cheap handle on which relation is the target, so
	// the sound conservative choice is to skip filtered narrowing for
	// the whole UPDATE. SQLite shares this path and this exemption.
	filtered := map[srcKey]bool{}
	if trustSrc && cat != nil && maxTree.Kind() != dialect.StmtUpdate {
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

// touchesView reports whether the statement references, at ANY nesting
// level, a relation the catalog knows as a view. DeepTables walks every
// base-relation name position — FROM lists, joins, derived tables, CTE
// bodies, set-operation branches, and subqueries inside expressions — so
// a view is caught wherever it can contribute an attributed column.
//
// The hazard: engines attribute a view's result columns THROUGH to the
// view's base tables (SQLite via sqlite3_column_origin_name; PostgreSQL
// and MySQL resolve view columns to their base relation OID/org_table),
// so a base table appearing directly in FROM can vouch for a column that
// actually flows through the view's invisible, possibly null-extending
// body. All three snapshots now mark views (cache.Table.IsView): the
// SQLite oracle, the PostgreSQL snapshot (relkind v/m), and the MySQL
// snapshot (information_schema.tables.table_type = 'VIEW'). A base table
// present in FROM alongside a view that exposes it is exactly the
// unsound case this kill-switch closes.
func touchesView(t dialect.Tree, cat *cache.Catalog) bool {
	if cat == nil {
		return false
	}
	for _, tr := range t.DeepTables() {
		if tbl := cat.LookupQualified(tr.Schema, tr.Name); tbl != nil && tbl.IsView {
			return true
		}
	}
	return false
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

// cteBinding is a CTE definition paired with the environment visible
// INSIDE its own body, captured at DEFINITION time: the enclosing scope
// plus only the same-level definitions that precede it. Carrying the
// body env with the definition is what lets a reference resolve the
// body correctly no matter how deeply nested the reference site is — a
// definition's body scope is a property of where it was WRITTEN, never
// of where it is USED. Reconstructing the body scope from the
// referencing level (the old fallback) leaked later same-level
// definitions into a body they cannot see, dropping a base relation's
// null-extension hazard when a deeper reference bound a base table to a
// forward CTE name.
type cteBinding struct {
	def     dialect.CTEDef
	bodyEnv map[string]cteBinding
}

// cloneEnv copies a CTE environment, reserving room for extra entries
// the caller will add. A nil source yields a fresh empty map.
func cloneEnv(src map[string]cteBinding, extra int) map[string]cteBinding {
	dst := make(map[string]cteBinding, len(src)+extra)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// collectPresence walks one statement level: catalog relations join
// the presence map (hazards merged), CTE references and derived
// tables recurse with the enclosing null-extension compounded, and
// hazardous sub-levels poison instead of contributing (design 05
// §2b). env carries the CTE definitions visible from enclosing scopes;
// this level's own definitions are added with positional visibility (a
// definition's body sees only the definitions preceding it, matching
// engine name resolution — see the body of this function).
func collectPresence(t dialect.Tree, env map[string]cteBinding,
	maxR ast.Rendering, cat *cache.Catalog, nullExt bool,
	present map[uint32]*presence, res *instanceResolver, top bool,
	memo map[memoKey]map[uint32]presence) {

	// A WITH list is visible to the enclosing SELECT/DML FROM (and to
	// derived tables) in full — every definition, regardless of order —
	// so stmtEnv adds them all. A definition's OWN body sees a narrower
	// scope: the outer environment plus only the definitions that
	// PRECEDE it in the list. PostgreSQL (and MySQL) resolve a name
	// inside a non-recursive CTE body against earlier definitions only,
	// so a forward reference binds to an outer/base relation, never to
	// the later same-named CTE; keying the body scope positionally is
	// what keeps that base relation's null-extension hazard from being
	// dropped. (WITH RECURSIVE is poisoned before recursion, so a
	// definition's self-visibility never matters here.) SQLite makes a
	// whole WITH list visible within each body, but analyzing a forward
	// reference as a base relation only ever ADDS a presence instance or
	// SKIPS a descent — both conservative for narrowing — so positional
	// scoping stays sound there too.
	outer := env
	stmtEnv := env
	if ctes := t.CTEs(); len(ctes) > 0 {
		stmtEnv = cloneEnv(outer, len(ctes))
		preceding := cloneEnv(outer, len(ctes))
		for _, def := range ctes {
			// A definition's body sees the enclosing scope plus only the
			// definitions preceding it — captured here, at definition
			// time, and carried with the binding so a reference from any
			// depth resolves the body against the same scope.
			b := cteBinding{def: def, bodyEnv: cloneEnv(preceding, 0)}
			preceding[def.Name] = b
			stmtEnv[def.Name] = b
		}
	}
	env = stmtEnv

	for _, rel := range t.Relations() {
		if isGuarded(maxR, rel.Loc) || rel.Table == "" {
			continue
		}
		if b, ok := env[rel.Table]; ok && rel.Schema == "" {
			if b.def.Tree == nil {
				// A data-modifying CTE (DELETE/UPDATE/INSERT … RETURNING).
				// The engine attributes its RETURNING columns to the base
				// tables the DML reads, and one of those may be null-
				// extended by a join INSIDE the DML (e.g. RETURNING a RIGHT
				// JOIN's null side). Granting nothing is insufficient: any
				// OTHER clean instance of that table in the outer FROM would
				// vouch for the OID. Poison every table the body mentions so
				// no path narrows it (design 05 §2b).
				poison(b.def.PoisonTables, cat, present)
				continue
			}
			// Always descend with the definition's OWN captured body env,
			// never one reconstructed from this (possibly deeper)
			// reference level. A non-recursive body's scope excludes its
			// own name by construction (bodyEnv is the PRECEDING set), so
			// no self-reference loop is possible; a recursive body is
			// poisoned in descend before any recursion.
			descend(b.def.Tree, b.def.Recursive, b.bodyEnv, maxR, cat,
				nullExt || rel.NullableSide, present, res, memo)
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
		descend(d.Tree, false, env, maxR, cat, nullExt || d.NullableSide, present, res, memo)
	}
}

// memoKey identifies a descended sub-level's presence contribution. The
// contribution is a pure function of (the sub-tree's identity, the
// effective null-extension it is referenced under): the sub-tree pointer
// also fixes its `recursive` flag and its captured body environment (a
// parse node occupies exactly one syntactic position, so it is always
// descended with the same env), leaving nullExt as the only other
// variable. Two references under DIFFERENT null-extension contexts get
// different keys and never share a verdict.
type memoKey struct {
	tree    dialect.Tree
	nullExt bool
}

// mergePresence folds a cached delta into the live presence map. Every
// presence field is accumulated additively (count) or by OR (the
// hazard flags) exactly as collectPresence/poison would when descending
// directly, so a merged delta is byte-for-byte equivalent to
// re-descending — the memoization changes performance, never verdicts.
// Additivity is also why the merge order over the delta map is
// irrelevant to the result (determinism preserved).
func mergePresence(dst map[uint32]*presence, delta map[uint32]presence) {
	for oid, d := range delta {
		p := dst[oid]
		if p == nil {
			p = &presence{}
			dst[oid] = p
		}
		p.count += d.count
		p.nullExtended = p.nullExtended || d.nullExtended
		p.inherited = p.inherited || d.inherited
		p.poisoned = p.poisoned || d.poisoned
	}
}

// descend enters one subquery. A nil tree cannot occur here: a
// data-modifying CTE never reaches descend (its poisoning is handled
// at the reference site in collectPresence, where its PoisonTables are
// in scope). A hazardous body (recursive CTE, set operation, grouping
// sets) poisons every table it mentions: its output can carry those
// tables' attribution while breaking their declared constraints
// (SQLite attributes compound output to the FIRST branch's table;
// grouping sets null grouping columns).
func descend(sub dialect.Tree, recursive bool, env map[string]cteBinding,
	maxR ast.Rendering, cat *cache.Catalog, nullExt bool,
	present map[uint32]*presence, res *instanceResolver,
	memo map[memoKey]map[uint32]presence) {

	if sub == nil {
		return
	}
	// Reuse a previously computed contribution for this (sub-tree, nullExt):
	// N references to one CTE body descend it once instead of N (or, for
	// chained CTEs, 2^N) times. res is never touched below top level, so
	// the cached delta captures the whole effect.
	key := memoKey{tree: sub, nullExt: nullExt}
	if delta, ok := memo[key]; ok {
		mergePresence(present, delta)
		return
	}
	// Compute this sub-level's contribution into a fresh scratch map, then
	// cache and merge. Nested descends recurse through the same memo and
	// accumulate into the scratch, so the cached delta is the sub-level's
	// COMPLETE presence contribution. The reference graph over parse nodes
	// is acyclic (non-recursive bodies never see their own name; recursive
	// bodies poison without descending), so no key is re-entered before it
	// is stored.
	scratch := map[uint32]*presence{}
	if recursive || sub.HasSetOperation() || sub.HasGroupingSets() {
		poison(sub.DeepTables(), cat, scratch)
	} else {
		collectPresence(sub, env, maxR, cat, nullExt, scratch, res, false, memo)
	}
	delta := make(map[uint32]presence, len(scratch))
	for oid, p := range scratch {
		delta[oid] = *p
	}
	memo[key] = delta
	mergePresence(present, delta)
}

// poison marks every catalog-resolvable table in refs as poisoned: a
// poisoned OID never narrows, on ANY exposing path, because a
// hazardous or data-modifying subquery can carry that table's column
// attribution while breaking its declared NOT NULL (a set operation's
// branch attribution, grouping-set nulling, or a DML body's RETURNING
// of a null-extended join side).
func poison(refs []dialect.TableRef, cat *cache.Catalog, present map[uint32]*presence) {
	for _, tr := range refs {
		if tbl := cat.LookupQualified(tr.Schema, tr.Name); tbl != nil {
			p := present[tbl.OID]
			if p == nil {
				p = &presence{}
				present[tbl.OID] = p
			}
			p.poisoned = true
		}
	}
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
