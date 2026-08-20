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

	w := &weaver{
		profile: profile, q: q, maxR: maxR, kind: tree.Kind(),
		rels: tree.Relations(), deep: tree.DeepTables(),
		onInfo: map[int]*joinOnResult{},
	}
	w.overread = overreadDeep(profile, fe, q, w.deep)
	w.where = whereClause(profile, q)

	res := Result{Query: q}
	var whereConjs []string
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
	var ins []insertion
	seq := 0
	add := func(off, prio int, text string) {
		ins = append(ins, insertion{off: off, prio: prio, seq: seq, text: text})
		seq++
	}
	for _, agg := range onAggs {
		joined := strings.Join(agg.conjs, " AND ")
		if agg.res.cs.hasOR {
			add(agg.res.cs.start, 0, "(")
			add(agg.res.cs.end, 1, ") AND "+joined)
		} else {
			add(agg.res.cs.end, 1, " AND "+joined)
		}
	}
	if len(whereConjs) > 0 {
		joined := strings.Join(whereConjs, " AND ")
		switch {
		case q.WhereKwEnd >= 0 && w.where.hasOR:
			add(q.WhereKwEnd, 2, " "+joined+" AND")
			add(w.where.start, 2, "(")
			add(w.where.end, 2, ")")
		case q.WhereKwEnd >= 0:
			add(q.WhereKwEnd, 2, " "+joined+" AND")
		case q.TailStart >= 0:
			add(q.TailStart, 2, "WHERE "+joined+" ")
		case q.StmtEnd >= 0:
			add(q.StmtEnd, 2, " WHERE "+joined)
		}
	}
	if len(ins) == 0 {
		return res
	}

	woven := splice(q, ins)
	registerParams(woven, res.Woven)
	res.Query = woven
	return res
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
	rels    []dialect.RelRef
	deep    []dialect.TableRef

	// overread is the set of lowercased base-table names that some
	// non-maximal verified rendering reads more often than the maximal
	// rendering does — a designated read living only in a non-first
	// @choose alternative or an @order-by @default body, invisible to
	// the maximal rendering the weaver scopes from. Any policy
	// designating such a name is refused (SQLETCH125): it cannot be
	// woven and must not ship unscoped.
	overread map[string]bool

	where  clauseScan
	onInfo map[int]*joinOnResult // keyed by relation template offset
}

// applicability is the shared answer to "would this policy bite this
// query?" — computed identically by the weaver and the enforcement
// pass so they can never disagree.
type applicability struct {
	topOcc []dialect.RelRef // designated top-level occurrences, document order
	hidden bool             // designated occurrences beyond the top level
	active bool             // the policy applies to this query at all
}

func analyzeApplicability(p *Policy, kind dialect.StmtKind, rels []dialect.RelRef, deep []dialect.TableRef) applicability {
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
		// and only when the policy covers reads (design 14 §D6).
		a.active = a.hidden && p.coversSelect()
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
func (w *weaver) apply(p *Policy) (*WovenPolicy, []string, []onNeed, []diagnostics.Diagnostic) {
	a := analyzeApplicability(p, w.kind, w.rels, w.deep)
	over := designatedOverread(p, w.overread, w.kind)
	if !a.active && len(over) == 0 {
		return nil, nil, nil, nil
	}
	// An honored opt-out suppresses weaving and the unweavable
	// diagnostics alike; an opt-out on a query the policy does not
	// touch is SQLETCH126, owned by the enforcement pass.
	if o, ok := optOutFor(w.q, p.Name); ok {
		return &WovenPolicy{Policy: p, OptedOut: true, OptOutReason: o.Reason}, nil, nil, nil
	}
	fail := func(d diagnostics.Diagnostic) (*WovenPolicy, []string, []onNeed, []diagnostics.Diagnostic) {
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
		return fail(w.unweavable(p, w.q.HeaderSpan,
			"a designated table is read inside this INSERT's SELECT body, which sqletch cannot scope"))
	}
	if a.hidden {
		return fail(w.unweavable(p, w.q.HeaderSpan,
			"a designated table appears inside a subquery, CTE, or set-operation branch, which sqletch cannot scope"))
	}

	// Occurrence checks (design 14 §D1/D2/D5, §11.2, §11.3), and the
	// WHERE-vs-ON split: a null-extended outer-join occurrence is
	// scoped in its own join's ON clause (§D2(a)).
	var whereOcc []dialect.RelRef
	var onOcc []struct {
		rel dialect.RelRef
		res *joinOnResult
	}
	for _, r := range a.topOcc {
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
				rel dialect.RelRef
				res *joinOnResult
			}{r, res})
		default:
			whereOcc = append(whereOcc, r)
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
	var whereConjs []string
	var needs []onNeed
	if strings.Contains(p.Predicate, Placeholder) {
		for _, r := range whereOcc {
			c := strings.ReplaceAll(p.Predicate, Placeholder, boundName(r))
			wp.Conjuncts = append(wp.Conjuncts, c)
			if !w.wherePresent(c) {
				whereConjs = append(whereConjs, c)
			}
		}
		for _, o := range onOcc {
			c := strings.ReplaceAll(p.Predicate, Placeholder, boundName(o.rel))
			wp.Conjuncts = append(wp.Conjuncts, c)
			if !onPresent(w.profile, o.res, c) {
				needs = append(needs, onNeed{res: o.res, conj: c})
			}
		}
	} else {
		// No relation reference: one WHERE conjunct scopes every
		// occurrence (it references no joined columns, so it cannot
		// null-filter an outer join).
		wp.Conjuncts = append(wp.Conjuncts, p.Predicate)
		if !w.wherePresent(p.Predicate) {
			whereConjs = append(whereConjs, p.Predicate)
		}
	}
	return wp, whereConjs, needs, nil
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
// unconditional skeleton conjunct of the WHERE clause — the
// idempotence rule: hand-scoped queries are not double-woven. Guarded
// copies deliberately do not count (they vanish in guard-off shapes),
// and a top-level OR poisons matching (the weaver then weaves and
// wraps: doubling is harmless, skipping leaks).
func (w *weaver) wherePresent(conjunct string) bool {
	if !w.where.lexOK || w.where.hasOR {
		return false
	}
	return segsContain(w.profile, w.where.segs, conjunct)
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
