package policy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/template"
)

// WovenPolicy records one policy's effect on one query — the input of
// the enforcement pass and the `explain` coverage report.
type WovenPolicy struct {
	Policy *Policy
	// Conjuncts are the scoping conjunct texts (template-space,
	// params as `:name`), one per designated top-level occurrence in
	// document order — whether the weaver inserted them or an
	// identical hand-written conjunct was already present. Empty when
	// the query opted out.
	Conjuncts []string
	// OptedOut records an honored `-- @policy-optout` (the policy
	// would have applied); OptOutReason is its mandatory reason.
	OptedOut     bool
	OptOutReason string
}

// Result is the outcome of weaving one query.
type Result struct {
	// Query is the woven template; it is the input template itself
	// (not a copy) when no policy contributed a conjunct.
	Query *template.QueryTemplate
	Woven []WovenPolicy
	Diags []diagnostics.Diagnostic
}

// Weave applies the policies to one scanned query (design 14 §4):
// it renders the maximal rendering of the unwoven template, parses it
// to learn the statement's relations, and splices unconditional
// scoping conjuncts — into the WHERE clause for ordinary occurrences,
// and into the introducing join's ON clause for occurrences on the
// null-extended side of an outer join (§D2(a): a WHERE conjunct would
// silently turn the outer join into an inner join; the ON conjunct
// preserves the outer row set and scopes only the joined rows). A
// clause whose top level contains OR is wrapped in parentheses first,
// so the appended conjunct always binds above it.
//
// A statement-level set operation (UNION/INTERSECT/EXCEPT) is scoped
// branch by branch (§13): every leaf core is treated like a top-level
// statement — its own WHERE (synthesized when absent), or its own
// join's ON — so a designated read in any branch is scoped where it is
// read. The parser's branch list and the weaver's lexical branch split
// must agree on every designated occurrence, and the woven statement
// must re-parse to the same branch structure; any disagreement is
// SQLETCH125.
//
// A designated table the weaver cannot scope — subquery/CTE position,
// USING/NATURAL join on a null-extended side, guarded join, non-bare
// bound name, conflicting parameter hint — is SQLETCH125: loud and
// incomplete beats silent and incomplete.
//
// A template whose maximal rendering fails to render or parse is
// returned unchanged: the ordinary pipeline diagnostics (SQLETCH100
// and friends) own that failure.
func Weave(profile dialect.LexerProfile, fe dialect.Frontend, pols []Policy, q *template.QueryTemplate) Result {
	if len(pols) == 0 {
		return Result{Query: q}
	}
	maxR, err := ast.Render(profile, q, nil)
	if err != nil {
		return Result{Query: q}
	}
	tree, err := fe.Parse(maxR.SQL)
	if err != nil || tree.StmtCount() != 1 {
		return Result{Query: q}
	}

	rels, nBranches := topRelations(tree)
	w := &weaver{
		profile: profile, q: q, maxR: maxR, kind: tree.Kind(),
		rels: rels, deep: tree.DeepTables(),
		upsertUpdate: tree.HasConflictUpdate(),
		onInfo:       map[int]*joinOnResult{},
		nBranches:    nBranches,
	}
	w.overread = overreadDeep(profile, fe, q, w.deep)
	if nBranches > 0 {
		so := scanSetOp(profile, q)
		w.setop = &so
	} else {
		w.where = whereClause(profile, q)
	}

	res := Result{Query: q}
	var whereConjs []whereNeed
	type occAgg struct {
		res   *joinOnResult
		conjs []string
	}
	var onAggs []*occAgg
	onByStart := map[int]*occAgg{}
	for i := range pols {
		p := &pols[i]
		wp, wcs, needs, diags := w.apply(p)
		res.Diags = append(res.Diags, diags...)
		if wp != nil {
			res.Woven = append(res.Woven, *wp)
		}
		whereConjs = append(whereConjs, wcs...)
		for _, n := range needs {
			agg := onByStart[n.res.cs.start]
			if agg == nil {
				agg = &occAgg{res: n.res}
				onByStart[n.res.cs.start] = agg
				onAggs = append(onAggs, agg)
			}
			agg.conjs = append(agg.conjs, n.conj)
		}
	}

	// Assemble the insertions. prio: wrap parens (0) < ON text (1) <
	// WHERE text (2), so an ON conjunct precedes a WHERE clause
	// synthesized at the same statement-end boundary.
	//
	// Every spliced predicate occurrence is parenthesized (design 14
	// §11.6): the predicate text is trusted config but not
	// precedence-neutral — an unparenthesized `p OR q` would capture the
	// query's own conjuncts under the OR's right arm (leak-shaped), and
	// a predicate carrying a depth-0 AND (its own, or BETWEEN's) would
	// AND-split into several segments the enforcement matcher could
	// never re-assemble. `(p OR q)` and `(a AND b)` are each exactly one
	// depth-0 conjunct with fixed precedence.
	var ins []insertion
	seq := 0
	add := func(off, prio int, text string) {
		ins = append(ins, insertion{off: off, prio: prio, seq: seq, text: text})
		seq++
	}
	for _, agg := range onAggs {
		joined := joinParenthesized(agg.conjs)
		if agg.res.cs.hasOR {
			add(agg.res.cs.start, 0, "(")
			add(agg.res.cs.end, 1, ") AND "+joined)
		} else {
			add(agg.res.cs.end, 1, " AND "+joined)
		}
	}
	// WHERE conjuncts, grouped by branch (-1 = the whole statement) in
	// first-need order; within a branch, policy declaration order.
	var branchOrder []int
	byBranch := map[int][]string{}
	for _, n := range whereConjs {
		if _, seen := byBranch[n.branch]; !seen {
			branchOrder = append(branchOrder, n.branch)
		}
		byBranch[n.branch] = append(byBranch[n.branch], n.conj)
	}
	for _, b := range branchOrder {
		joined := joinParenthesized(byBranch[b])
		// The statement's own WHERE slot, or (inside a set operation) the
		// branch core's — the same four cases either way.
		whereKwEnd, tailStart, end, where := q.WhereKwEnd, q.TailStart, q.StmtEnd, w.where
		if b >= 0 {
			bs := w.setop.branches[b]
			whereKwEnd, tailStart, end, where = bs.whereKwEnd, bs.tailStart, bs.end, bs.where
		}
		switch {
		case whereKwEnd >= 0 && where.hasOR:
			add(whereKwEnd, 2, " "+joined+" AND")
			add(where.start, 2, "(")
			add(where.end, 2, ")")
		case whereKwEnd >= 0:
			add(whereKwEnd, 2, " "+joined+" AND")
		case tailStart >= 0:
			add(tailStart, 2, "WHERE "+joined+" ")
		case end >= 0:
			add(end, 2, " WHERE "+joined)
		}
	}
	if len(ins) == 0 {
		return res
	}

	woven := splice(q, ins)
	registerParams(woven, res.Woven)
	if nBranches > 0 {
		if d, ok := w.verifySetOpWeave(fe, woven, res.Woven); !ok {
			return Result{Query: q, Diags: append(res.Diags, d)}
		}
	}
	res.Query = woven
	return res
}

// whereNeed is one conjunct destined for a WHERE clause: the
// statement's (branch -1) or one set-operation branch core's.
type whereNeed struct {
	branch int
	conj   string
}

// topRel is one top-level relation occurrence: a FROM/target relation
// of the statement, or of one set-operation branch core (branch >= 0;
// -1 outside a set operation).
type topRel struct {
	dialect.RelRef
	branch int
}

// topRelations returns the statement's top-level relation occurrences
// and its set-operation branch count (0 when it is not a set
// operation). Inside a set operation the occurrences are the branch
// cores' own relations, tagged with their branch — the whole-statement
// Relations() is meaningless there (PostgreSQL/MySQL report none,
// SQLite the first core's only). Shared by Weave and Enforce so both
// read the same occurrences.
func topRelations(tree dialect.Tree) ([]topRel, int) {
	bs := tree.SetOpBranches()
	if bs == nil {
		rels := tree.Relations()
		out := make([]topRel, len(rels))
		for i, r := range rels {
			out[i] = topRel{RelRef: r, branch: -1}
		}
		return out, 0
	}
	var out []topRel
	for i, b := range bs {
		for _, r := range b.Relations() {
			out = append(out, topRel{RelRef: r, branch: i})
		}
	}
	return out, len(bs)
}

// verifySetOpWeave re-parses the woven maximal rendering of a set
// operation and requires the same branch structure: the same number of
// branches, each with the same relations. Every insertion lands inside
// one core's WHERE/ON slot, so this can only fail if the lexical split
// mis-placed one — which must surface as SQLETCH125, never as a woven
// statement of a different shape.
func (w *weaver) verifySetOpWeave(fe dialect.Frontend, woven *template.QueryTemplate, wps []WovenPolicy) (diagnostics.Diagnostic, bool) {
	name := ""
	for _, wp := range wps {
		if !wp.OptedOut && len(wp.Conjuncts) > 0 {
			name = wp.Policy.Name
			break
		}
	}
	bad := func() (diagnostics.Diagnostic, bool) {
		return diagnostics.Errorf(diagnostics.CodePolicyUnweavable, w.q.HeaderSpan,
			"policy %q applies to this query but cannot be woven: the set operation's branches could not be scoped without changing the statement's structure", name).
			WithHint("opt out explicitly with `-- @policy-optout: %s (reason)` or restructure the query", name), false
	}
	r, err := ast.Render(w.profile, woven, nil)
	if err != nil {
		return bad()
	}
	tree, err := fe.Parse(r.SQL)
	if err != nil || tree.StmtCount() != 1 {
		return bad()
	}
	got, n := topRelations(tree)
	if n != w.nBranches || len(got) != len(w.rels) {
		return bad()
	}
	for i := range got {
		if got[i].branch != w.rels[i].branch || !strings.EqualFold(got[i].Table, w.rels[i].Table) || got[i].Alias != w.rels[i].Alias {
			return bad()
		}
	}
	return diagnostics.Diagnostic{}, true
}

// joinParenthesized joins conjunct texts into splice-ready SQL, each
// occurrence wrapped in its own parentheses (see the assembly comment
// in Weave).
func joinParenthesized(conjs []string) string {
	wrapped := make([]string, len(conjs))
	for i, c := range conjs {
		wrapped[i] = "(" + c + ")"
	}
	return strings.Join(wrapped, " AND ")
}

// onNeed is one conjunct destined for one join's ON clause.
type onNeed struct {
	res  *joinOnResult
	conj string
}

type weaver struct {
	profile dialect.LexerProfile
	q       *template.QueryTemplate
	maxR    ast.Rendering
	kind    dialect.StmtKind
	rels    []topRel
	deep    []dialect.TableRef
	// nBranches is the set-operation branch count (0 outside a set
	// operation); setop is the lexical branch split, non-nil exactly
	// when nBranches > 0.
	nBranches int
	setop     *setOpScan
	// upsertUpdate reports an INSERT whose ON CONFLICT DO UPDATE (MySQL:
	// ON DUPLICATE KEY UPDATE) arm modifies rows — refused on a
	// designated target (audit-12 M10).
	upsertUpdate bool

	// overread is the set of lowercased base-table names that some
	// non-maximal verified rendering reads more often than the maximal
	// rendering does — a designated read living only in a non-first
	// @choose alternative or an @order-by @default body, invisible to
	// the maximal rendering the weaver scopes from. Any policy
	// designating such a name is refused (SQLETCH125): it cannot be
	// woven and must not ship unscoped.
	overread map[string]bool

	where  clauseScan            // the statement's WHERE (outside a set operation)
	onInfo map[int]*joinOnResult // keyed by relation template offset
}

// applicability is the shared answer to "would this policy bite this
// query?" — computed identically by the weaver and the enforcement
// pass so they can never disagree.
type applicability struct {
	topOcc []topRel // designated top-level occurrences, document order
	hidden bool     // designated occurrences beyond the top level
	active bool     // the policy applies to this query at all
}

func analyzeApplicability(p *Policy, kind dialect.StmtKind, rels []topRel, deep []dialect.TableRef, upsertUpdate bool) applicability {
	var a applicability
	topCount := map[string]int{}
	for _, r := range rels {
		if r.Table != "" && p.designates(r.Table) {
			a.topOcc = append(a.topOcc, r)
			topCount[strings.ToLower(r.Table)]++
		}
	}
	for _, tr := range deep {
		if p.designates(tr.Name) {
			topCount[strings.ToLower(tr.Name)]--
		}
	}
	for _, n := range topCount {
		if n < 0 {
			a.hidden = true
		}
	}
	if kind == dialect.StmtInsert {
		// The INSERT target is never a weave target (no rows are
		// filtered); only a read inside an INSERT … SELECT body bites,
		// and only when the policy covers reads (design 14 §D6). One
		// exception (owner decision 2026-08-21, audit-12 M10): an
		// INSERT … ON CONFLICT DO UPDATE on a designated target MODIFIES
		// rows like an UPDATE — the policy is active (to be REFUSED in
		// apply/Enforce) when it covers updates and the target is
		// designated.
		a.active = (a.hidden && p.coversSelect()) ||
			(upsertUpdate && len(a.topOcc) > 0 && p.appliesTo(dialect.StmtUpdate))
	} else {
		a.active = p.appliesTo(kind) && (len(a.topOcc) > 0 || a.hidden)
	}
	return a
}

// overreadDeep returns the set of lowercased base-table names that some
// non-maximal verified rendering reads more often than the maximal
// rendering does. Only @choose alternatives and @order-by @default
// bodies can introduce such a read: an @in arity-0 rendering emits a
// table-free placeholder and an empty @filter-tree emits TRUE, so
// neither adds a base-table reference the maximal rendering lacks. The
// maximal rendering itself provides the baseline (maxDeep).
//
// Candidate renderings are materialised and discarded one at a time, so
// peak memory is a single rendering regardless of the (linear) rendering
// count — the SQLETCH302 shape cap guards the simultaneous Renderings
// set, which this scan never builds. A rendering that fails to render or
// parse is skipped: the ordinary pipeline diagnostics own that failure,
// and an unparseable alternative cannot ship valid unscoped SQL.
func overreadDeep(profile dialect.LexerProfile, fe dialect.Frontend, q *template.QueryTemplate, maxDeep []dialect.TableRef) map[string]bool {
	base := map[string]int{}
	for _, tr := range maxDeep {
		base[strings.ToLower(tr.Name)]++
	}
	over := map[string]bool{}
	note := func(r ast.Rendering, err error) {
		if err != nil {
			return
		}
		t, perr := fe.Parse(r.SQL)
		if perr != nil || t.StmtCount() != 1 {
			return
		}
		cur := map[string]int{}
		for _, tr := range t.DeepTables() {
			cur[strings.ToLower(tr.Name)]++
		}
		for name, cnt := range cur {
			if cnt > base[name] {
				over[name] = true
			}
		}
	}

	chooseIdx, orderCount := 0, 0
	for _, it := range q.Items {
		switch c := it.(type) {
		case *template.Choose:
			n := len(c.Cases)
			if c.Default != nil {
				n++
			}
			for ord := 1; ord < n; ord++ {
				r, err := ast.Render(profile, q, ast.CaseSelection{chooseIdx: ord})
				note(r, err)
			}
			chooseIdx++
		case *template.OrderBy:
			orderCount++
		}
	}
	if orderCount > 0 {
		orderIdx := 0
		for _, it := range q.Items {
			o, ok := it.(*template.OrderBy)
			if !ok {
				continue
			}
			if o.Default != nil {
				orders := make(ast.OrderSelection, orderCount)
				orders[orderIdx] = []uint8{} // empty non-nil = @default body
				r, err := ast.RenderShape(profile, q, ^uint64(0), nil, orders, nil)
				note(r, err)
			}
			orderIdx++
		}
	}
	if len(over) == 0 {
		return nil
	}
	return over
}

// designatedOverread returns, in sorted order, the overread table names
// a policy designates — the designated reads that live only in a
// non-maximal rendering. It is empty unless the policy covers the
// statement's kind (an INSERT's overread is a read in an INSERT … SELECT
// case body, gated on coversSelect like every other read).
func designatedOverread(p *Policy, over map[string]bool, kind dialect.StmtKind) []string {
	if len(over) == 0 {
		return nil
	}
	applies := p.appliesTo(kind)
	if kind == dialect.StmtInsert {
		applies = p.coversSelect()
	}
	if !applies {
		return nil
	}
	var out []string
	for name := range over {
		if p.designates(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// optOutFor returns the query's first opt-out naming the policy.
func optOutFor(q *template.QueryTemplate, name string) (template.PolicyOptOut, bool) {
	for _, o := range q.PolicyOptOuts {
		if o.Policy == name {
			return o, true
		}
	}
	return template.PolicyOptOut{}, false
}

// applyFor returns the query's first `-- @policy-apply` naming the
// policy. It is the affirmative mirror of optOutFor; unlike an
// opt-out it changes nothing about weaving (design 14 §12.2).
func applyFor(q *template.QueryTemplate, name string) (template.PolicyApply, bool) {
	for _, a := range q.PolicyApplies {
		if a.Policy == name {
			return a, true
		}
	}
	return template.PolicyApply{}, false
}

// relTemplateOff maps a relation's rendered location to its template
// offset; ok is false when the dialect exposes no offset or the
// location maps into synthesized text.
func (w *weaver) relTemplateOff(r dialect.RelRef) (int, bool) {
	if r.Loc < 0 {
		return 0, false
	}
	tOff, synth := w.maxR.Map.ToTemplate(r.Loc)
	if synth {
		return 0, false
	}
	return tOff, true
}

// joinOnFor memoizes the ON-clause scan per relation occurrence.
func (w *weaver) joinOnFor(relOff int, ownJoin dialect.JoinType) *joinOnResult {
	if res, ok := w.onInfo[relOff]; ok {
		return res
	}
	res := joinOn(w.profile, w.q, relOff, ownJoin)
	w.onInfo[relOff] = &res
	return &res
}

// apply runs one policy against the query. It returns the coverage
// record (nil when the policy does not touch the query), the WHERE
// conjuncts to insert, the ON-clause needs, and diagnostics.
//
// The policy's `require_annotation` obligation (design 14 §12) is
// checked here, around the weave rather than inside it: an
// unannotated query is still woven, and SQLETCH127 is reported
// alongside whatever the weave itself produced. Emitting the
// diagnostic INSTEAD of the conjunct would make a query that fails
// the requirement fail UNSCOPED, putting the safe direction at the
// mercy of every caller honoring the error.
func (w *weaver) apply(p *Policy) (*WovenPolicy, []whereNeed, []onNeed, []diagnostics.Diagnostic) {
	a := analyzeApplicability(p, w.kind, w.rels, w.deep, w.upsertUpdate)
	over := designatedOverread(p, w.overread, w.kind)
	if !a.active && len(over) == 0 {
		return nil, nil, nil, nil
	}
	wp, conjuncts, needs, diags := w.applyActive(p, a, over)
	// Appended last so the structural refusal (SQLETCH125) reads
	// first: restructuring or opting out is the author's next move,
	// and an opt-out discharges this obligation as a side effect.
	if d, ok := w.annotationObligation(p); ok {
		diags = append(diags, d)
	}
	return wp, conjuncts, needs, diags
}

// annotationObligation reports the policy's unmet `require_annotation`
// demand for this query. The caller has already established that the
// policy applies — §12.3: the obligation keys on exactly the
// applicability predicate SQLETCH126 uses, so a query can never be
// obliged to annotate and simultaneously forbidden from doing so.
func (w *weaver) annotationObligation(p *Policy) (diagnostics.Diagnostic, bool) {
	if !p.RequireAnnotation {
		return diagnostics.Diagnostic{}, false
	}
	if _, ok := applyFor(w.q, p.Name); ok {
		return diagnostics.Diagnostic{}, false
	}
	if _, ok := optOutFor(w.q, p.Name); ok {
		return diagnostics.Diagnostic{}, false
	}
	return diagnostics.Errorf(diagnostics.CodePolicyUnannotated, w.q.HeaderSpan,
		"policy %q applies to this query and is declared `require_annotation: true`, so the query must say so: it is scoped either way, but the template must record which",
		p.Name).
		WithHint("add `-- @policy-apply: %s` to acknowledge the scoping, or `-- @policy-optout: %s (reason)` to exempt this query", p.Name, p.Name), true
}

// applyActive is apply's body for a policy that does touch the query.
func (w *weaver) applyActive(p *Policy, a applicability, over []string) (*WovenPolicy, []whereNeed, []onNeed, []diagnostics.Diagnostic) {
	// An honored opt-out suppresses weaving and the unweavable
	// diagnostics alike; an opt-out on a query the policy does not
	// touch is SQLETCH126, owned by the enforcement pass.
	if o, ok := optOutFor(w.q, p.Name); ok {
		return &WovenPolicy{Policy: p, OptedOut: true, OptOutReason: o.Reason}, nil, nil, nil
	}
	fail := func(d diagnostics.Diagnostic) (*WovenPolicy, []whereNeed, []onNeed, []diagnostics.Diagnostic) {
		return nil, nil, nil, []diagnostics.Diagnostic{d}
	}
	// A designated table read only in a non-maximal rendering — a
	// non-first @choose alternative or an @order-by @default body — is
	// invisible to the maximal rendering the weaver scopes from, so no
	// spliced WHERE conjunct reaches it. Refuse rather than ship it
	// completely unscoped (a silent tenant-scoping leak); this holds
	// whether or not the policy also bites the maximal rendering.
	if len(over) > 0 {
		return fail(w.unweavable(p, w.q.HeaderSpan,
			fmt.Sprintf("designated table %q is read only inside a non-first @choose alternative or an @order-by @default body, a rendering the weaver cannot see or scope (it works from the maximal rendering)", over[0])))
	}
	if w.kind == dialect.StmtInsert {
		if w.upsertUpdate && len(a.topOcc) > 0 {
			// An INSERT … ON CONFLICT DO UPDATE (MySQL: ON DUPLICATE KEY
			// UPDATE) on a designated target modifies rows on a conflict,
			// but the DO UPDATE arm cannot carry a woven WHERE that scopes
			// the conflict — a cross-tenant unique-key collision could
			// overwrite another tenant's row. Refuse (owner decision
			// 2026-08-21, audit-12 M10).
			return fail(w.unweavable(p, w.relSpan(a.topOcc[0].RelRef),
				fmt.Sprintf("table %q is the target of an INSERT … ON CONFLICT DO UPDATE (upsert); its DO UPDATE arm modifies rows but cannot carry a scoping conjunct, so the upsert cannot be woven", a.topOcc[0].Table)))
		}
		return fail(w.unweavable(p, w.q.HeaderSpan,
			"a designated table is read inside this INSERT's SELECT body, which sqletch cannot scope"))
	}
	if a.hidden {
		return fail(w.unweavable(p, w.q.HeaderSpan,
			"a designated table appears inside a subquery or CTE body, which sqletch cannot scope"))
	}
	if w.setop != nil {
		if d, ok := w.checkSetOpOccurrences(p, a); !ok {
			return fail(d)
		}
	}

	// Occurrence checks (design 14 §D1/D2/D5, §11.2, §11.3), and the
	// WHERE-vs-ON split: a null-extended outer-join occurrence is
	// scoped in its own join's ON clause (§D2(a)).
	var whereOcc []topRel
	var onOcc []struct {
		rel topRel
		res *joinOnResult
	}
	for _, occ := range a.topOcc {
		r := occ.RelRef
		switch {
		case w.guardedAt(r.Loc):
			return fail(w.unweavable(p, w.relSpan(r),
				fmt.Sprintf("table %q is introduced by a guarded (@if-present) join and cannot be unconditionally scoped", r.Table)))
		case !bareIdentRe.MatchString(boundName(r)):
			return fail(w.unweavable(p, w.relSpan(r),
				fmt.Sprintf("the name bound to the predicate placeholder (%q) is not a bare identifier", boundName(r))))
		case r.NullableSide:
			relOff, ok := w.relTemplateOff(r)
			if !ok {
				return fail(w.unweavable(p, w.relSpan(r),
					fmt.Sprintf("table %q sits on the null-extended side of an outer join, and its reference cannot be located in the template", r.Table)))
			}
			res := w.joinOnFor(relOff, r.Join)
			if !res.found || !res.cs.lexOK || res.cs.start < 0 {
				return fail(w.unweavable(p, w.relSpan(r),
					fmt.Sprintf("table %q sits on the null-extended side of a join with no ON expression to extend (USING/NATURAL/comma join); rewrite it with an explicit ON", r.Table)))
			}
			if res.wrongJoin {
				// The located ON does not belong to the join that
				// null-extends this occurrence (a FULL join preserves both
				// sides, or the table is on the preserved side of its own
				// outer join and null-extended farther out). Weaving there
				// would silently leak the designated table's own rows;
				// refuse rather than ship a leak the SQLETCH124 pass would
				// (correctly) then also reject.
				return fail(w.unweavable(p, w.relSpan(r),
					fmt.Sprintf("table %q is null-extended by an outer join whose ON clause cannot scope its own rows (a FULL join preserves both sides, or the table is on the preserved side of its own join and null-extended by an enclosing join); a WHERE conjunct would turn the join inner and an ON conjunct on the wrong join would leak", r.Table)))
			}
			onOcc = append(onOcc, struct {
				rel topRel
				res *joinOnResult
			}{occ, res})
		default:
			whereOcc = append(whereOcc, occ)
		}
	}

	// Parameter-kind agreement (design 14 §11.4). A policy binds its
	// value parameter UNCONDITIONALLY (D3a); reusing a name the query
	// author already declared is sound only when that parameter is a
	// plain, always-required value parameter. Sharing the name with an
	// optional (@if-present) parameter would send NULL in every shape
	// the caller leaves it None — silently emptying the result, the
	// exact failure D3 makes the woven parameter a required argument to
	// prevent; sharing with a control parameter (@when value, presence
	// guard, or @filter-tree @predicate argument) binds a value where R9
	// forbids one. Reject loudly instead of weaving a copy the
	// enforcement pass (SQLETCH124) would then wrongly accept.
	if p.ParamName != "" {
		if existing, ok := w.q.Params[p.ParamName]; ok {
			if why := policyParamKindCollision(existing); why != "" {
				return fail(w.unweavable(p, paramDeclSpan(w.q, p.ParamName), why))
			}
		}
	}

	// Parameter-hint agreement (design 14 §11.4).
	if p.ParamName != "" && p.ParamType != "" {
		if h, ok := w.q.TypeHints[p.ParamName]; ok && !strings.EqualFold(strings.TrimSpace(h.SQLType), p.ParamType) {
			return fail(w.unweavable(p, h.Span,
				fmt.Sprintf("the query hints parameter %q as %q, but the policy declares %q", p.ParamName, h.SQLType, p.ParamType)))
		}
	}

	wp := &WovenPolicy{Policy: p}
	var whereConjs []whereNeed
	var needs []onNeed
	if strings.Contains(p.Predicate, Placeholder) {
		for _, r := range whereOcc {
			c := strings.ReplaceAll(p.Predicate, Placeholder, boundName(r.RelRef))
			wp.Conjuncts = append(wp.Conjuncts, c)
			if !w.wherePresent(r.branch, c) {
				whereConjs = append(whereConjs, whereNeed{branch: r.branch, conj: c})
			}
		}
		for _, o := range onOcc {
			c := strings.ReplaceAll(p.Predicate, Placeholder, boundName(o.rel.RelRef))
			wp.Conjuncts = append(wp.Conjuncts, c)
			if !onPresent(w.profile, o.res, c) {
				needs = append(needs, onNeed{res: o.res, conj: c})
			}
		}
	} else {
		// No relation reference: one WHERE conjunct scopes every
		// occurrence of the statement — or of one set-operation branch
		// (it references no joined columns, so it cannot null-filter an
		// outer join).
		seen := map[int]bool{}
		for _, occ := range a.topOcc {
			if seen[occ.branch] {
				continue
			}
			seen[occ.branch] = true
			wp.Conjuncts = append(wp.Conjuncts, p.Predicate)
			if !w.wherePresent(occ.branch, p.Predicate) {
				whereConjs = append(whereConjs, whereNeed{branch: occ.branch, conj: p.Predicate})
			}
		}
	}
	return wp, whereConjs, needs, nil
}

// checkSetOpOccurrences cross-checks every designated occurrence of a
// set operation against the lexical branch split (§13.2): the split
// must have found exactly the parser's branches, the occurrence's name
// token must lie in the lexical core of the SAME index the parser
// assigned it, and that core must be a SELECT (a TABLE or VALUES
// operand has no WHERE slot). A conjunct is only ever woven into the
// branch both sides agree on.
func (w *weaver) checkSetOpOccurrences(p *Policy, a applicability) (diagnostics.Diagnostic, bool) {
	if !w.setop.ok || len(w.setop.branches) != w.nBranches {
		return w.unweavable(p, w.q.HeaderSpan,
			"the branches of this set operation could not be located in the template (a set operator inside a construct body, or an operand shape sqletch does not model)"), false
	}
	for _, occ := range a.topOcc {
		if w.guardedAt(occ.Loc) {
			// Inside a construct body (not lexed by the split); refused
			// with the guarded-join diagnostic by the occurrence loop.
			continue
		}
		relOff, ok := w.relTemplateOff(occ.RelRef)
		bi, found := w.setop.branchOf[relOff]
		if !ok || !found || bi != occ.branch {
			return w.unweavable(p, w.relSpan(occ.RelRef),
				fmt.Sprintf("table %q in a set-operation branch cannot be located in the template", occ.Table)), false
		}
		if kw := w.setop.branches[bi].leadKw; kw != "SELECT" {
			return w.unweavable(p, w.relSpan(occ.RelRef),
				fmt.Sprintf("table %q is read by a %s operand of a set operation, which has no WHERE clause to scope it; write that operand as SELECT … FROM", occ.Table, kw)), false
		}
	}
	return diagnostics.Diagnostic{}, true
}

// policyParamKindCollision reports why a policy cannot re-bind an
// existing query parameter as its unconditional scoping value, or ""
// when sharing is sound. The unsafe kinds are recognised from
// scanner-populated fields alone (GuardBit and the per-occurrence
// InFilterTree flag), so the answer does not depend on the R9
// classification (Optional) having run: an optional @if-present
// parameter is always a presence guard, so GuardBit >= 0 already
// covers it, and Optional is honoured only as belt-and-braces.
func policyParamKindCollision(existing *template.Param) string {
	for _, occ := range existing.Occurrences {
		if occ.InFilterTree {
			return fmt.Sprintf("the query already binds parameter %q inside a @filter-tree @predicate (a constructor argument); it cannot also be bound as a policy scoping value (R9)", existing.Name)
		}
	}
	if existing.GuardBit >= 0 || existing.Optional {
		return fmt.Sprintf("the query already uses parameter %q as a control parameter (an @if-present guard or @when value); an optional guard sends NULL in every shape the caller omits it, and a control parameter cannot be bound as a policy scoping value (R9)", existing.Name)
	}
	return ""
}

// paramDeclSpan points at the author's declaration of name (its first
// bind occurrence), falling back to the query header.
func paramDeclSpan(q *template.QueryTemplate, name string) diagnostics.Span {
	if p, ok := q.Params[name]; ok && len(p.Occurrences) > 0 {
		return p.Occurrences[0].Span
	}
	return q.HeaderSpan
}

func (w *weaver) unweavable(p *Policy, span diagnostics.Span, why string) diagnostics.Diagnostic {
	return diagnostics.Errorf(diagnostics.CodePolicyUnweavable, span,
		"policy %q applies to this query but cannot be woven: %s", p.Name, why).
		WithHint("opt out explicitly with `-- @policy-optout: %s (reason)` or restructure the query", p.Name)
}

// relSpan maps a relation's rendered location back to a template span
// (the query header when the dialect exposes no offset).
func (w *weaver) relSpan(r dialect.RelRef) diagnostics.Span {
	if r.Loc < 0 {
		return w.q.HeaderSpan
	}
	tOff, synth := w.maxR.Map.ToTemplate(r.Loc)
	if synth {
		return w.q.HeaderSpan
	}
	n := len(r.Table)
	return diagnostics.Span{File: w.q.HeaderSpan.File, Start: tOff, End: tOff + n}
}

// guardedAt reports whether a rendered offset lies inside an
// @if-present fragment's emission (the D5 rejection input; mirrors
// rules.resolver.fragAt).
func (w *weaver) guardedAt(loc int) bool {
	if loc < 0 {
		return false
	}
	for _, fr := range w.maxR.Frags {
		if loc >= fr.Start && loc < fr.End {
			ip, ok := fr.Item.(*template.IfPresent)
			return ok && len(ip.Guards) > 0
		}
	}
	return false
}

// wherePresent reports whether an identical conjunct is already an
// unconditional skeleton conjunct of the WHERE clause (of the
// statement, or of one set-operation branch core) — the
// idempotence rule: hand-scoped queries are not double-woven. Guarded
// copies deliberately do not count (they vanish in guard-off shapes),
// and a top-level OR poisons matching (the weaver then weaves and
// wraps: doubling is harmless, skipping leaks).
func (w *weaver) wherePresent(branch int, conjunct string) bool {
	where := w.where
	if branch >= 0 {
		where = w.setop.branches[branch].where
	}
	if !where.lexOK || where.hasOR {
		return false
	}
	return segsContain(w.profile, where.segs, conjunct)
}

// onPresent is wherePresent for one join's ON clause.
func onPresent(profile dialect.LexerProfile, res *joinOnResult, conjunct string) bool {
	if !res.found || !res.cs.lexOK || res.cs.hasOR {
		return false
	}
	return segsContain(profile, res.cs.segs, conjunct)
}

func boundName(r dialect.RelRef) string {
	if r.Alias != "" {
		return r.Alias
	}
	return r.Table
}
