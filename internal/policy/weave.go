package policy

import (
	"fmt"
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
	where   clauseScan
	onInfo  map[int]*joinOnResult // keyed by relation template offset
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
func (w *weaver) joinOnFor(relOff int) *joinOnResult {
	if res, ok := w.onInfo[relOff]; ok {
		return res
	}
	res := joinOn(w.profile, w.q, relOff)
	w.onInfo[relOff] = &res
	return &res
}

// apply runs one policy against the query. It returns the coverage
// record (nil when the policy does not touch the query), the WHERE
// conjuncts to insert, the ON-clause needs, and diagnostics.
func (w *weaver) apply(p *Policy) (*WovenPolicy, []string, []onNeed, []diagnostics.Diagnostic) {
	a := analyzeApplicability(p, w.kind, w.rels, w.deep)
	if !a.active {
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
			res := w.joinOnFor(relOff)
			if !res.found || !res.cs.lexOK || res.cs.start < 0 {
				return fail(w.unweavable(p, w.relSpan(r),
					fmt.Sprintf("table %q sits on the null-extended side of a join with no ON expression to extend (USING/NATURAL/comma join); rewrite it with an explicit ON", r.Table)))
			}
			onOcc = append(onOcc, struct {
				rel dialect.RelRef
				res *joinOnResult
			}{r, res})
		default:
			whereOcc = append(whereOcc, r)
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
