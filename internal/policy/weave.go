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
// scoping conjuncts into the WHERE clause. A designated table the
// weaver cannot scope — subquery/CTE position, null-extended
// outer-join side, guarded join, non-bare bound name, conflicting
// parameter hint — is SQLETCH125: loud and incomplete beats silent
// and incomplete.
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

	w := &weaver{profile: profile, q: q, maxR: maxR, tree: tree, kind: tree.Kind()}
	w.rels = tree.Relations()
	w.deep = tree.DeepTables()
	w.segments, w.segmentable = skeletonConjuncts(profile, q)

	res := Result{Query: q}
	var inserts []string
	for i := range pols {
		p := &pols[i]
		wp, conjs, diags := w.apply(p)
		res.Diags = append(res.Diags, diags...)
		if wp != nil {
			res.Woven = append(res.Woven, *wp)
		}
		inserts = append(inserts, conjs...)
	}
	if len(inserts) == 0 {
		return res
	}

	woven := splice(q, inserts)
	registerParams(woven, res.Woven)
	res.Query = woven
	return res
}

type weaver struct {
	profile dialect.LexerProfile
	q       *template.QueryTemplate
	maxR    ast.Rendering
	tree    dialect.Tree
	kind    dialect.StmtKind
	rels    []dialect.RelRef
	deep    []dialect.TableRef

	// segments are the query's unconditional skeleton WHERE conjuncts
	// as normalized token sequences (the idempotence input);
	// segmentable is false when the WHERE clause has a top-level OR,
	// in which case nothing matches and the weaver errs toward weaving
	// (a doubled predicate is harmless; a skipped one is a leak).
	segments    [][]string
	segmentable bool
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

// apply runs one policy against the query. It returns the coverage
// record (nil when the policy does not touch the query), the conjunct
// texts to insert, and diagnostics.
func (w *weaver) apply(p *Policy) (*WovenPolicy, []string, []diagnostics.Diagnostic) {
	a := analyzeApplicability(p, w.kind, w.rels, w.deep)
	if !a.active {
		return nil, nil, nil
	}
	// An honored opt-out suppresses weaving and the unweavable
	// diagnostics alike; an opt-out on a query the policy does not
	// touch is SQLETCH126, owned by the enforcement pass.
	if o, ok := optOutFor(w.q, p.Name); ok {
		return &WovenPolicy{Policy: p, OptedOut: true, OptOutReason: o.Reason}, nil, nil
	}
	if w.kind == dialect.StmtInsert {
		return nil, nil, []diagnostics.Diagnostic{w.unweavable(p, w.q.HeaderSpan,
			"a designated table is read inside this INSERT's SELECT body, which sqletch cannot scope")}
	}
	if a.hidden {
		return nil, nil, []diagnostics.Diagnostic{w.unweavable(p, w.q.HeaderSpan,
			"a designated table appears inside a subquery, CTE, or set-operation branch, which sqletch cannot scope")}
	}
	topOcc := a.topOcc

	// Occurrence checks (design 14 §D1/D2/D5, §11.2, §11.3).
	for _, r := range topOcc {
		switch {
		case r.NullableSide:
			return nil, nil, []diagnostics.Diagnostic{w.unweavable(p, w.relSpan(r),
				fmt.Sprintf("table %q sits on the null-extended side of an outer join; a WHERE conjunct would silently turn it into an inner join", r.Table))}
		case w.guardedAt(r.Loc):
			return nil, nil, []diagnostics.Diagnostic{w.unweavable(p, w.relSpan(r),
				fmt.Sprintf("table %q is introduced by a guarded (@if-present) join and cannot be unconditionally scoped", r.Table))}
		case !bareIdentRe.MatchString(boundName(r)):
			return nil, nil, []diagnostics.Diagnostic{w.unweavable(p, w.relSpan(r),
				fmt.Sprintf("the name bound to the predicate placeholder (%q) is not a bare identifier", boundName(r)))}
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
				return nil, nil, []diagnostics.Diagnostic{w.unweavable(p, paramDeclSpan(w.q, p.ParamName), why)}
			}
		}
	}

	// Parameter-hint agreement (design 14 §11.4).
	if p.ParamName != "" && p.ParamType != "" {
		if h, ok := w.q.TypeHints[p.ParamName]; ok && !strings.EqualFold(strings.TrimSpace(h.SQLType), p.ParamType) {
			return nil, nil, []diagnostics.Diagnostic{w.unweavable(p, h.Span,
				fmt.Sprintf("the query hints parameter %q as %q, but the policy declares %q", p.ParamName, h.SQLType, p.ParamType))}
		}
	}

	wp := &WovenPolicy{Policy: p}
	var inserts []string
	if strings.Contains(p.Predicate, Placeholder) {
		for _, r := range topOcc {
			c := strings.ReplaceAll(p.Predicate, Placeholder, boundName(r))
			wp.Conjuncts = append(wp.Conjuncts, c)
			if !w.present(c) {
				inserts = append(inserts, c)
			}
		}
	} else {
		// No relation reference: one conjunct scopes every occurrence.
		wp.Conjuncts = append(wp.Conjuncts, p.Predicate)
		if !w.present(p.Predicate) {
			inserts = append(inserts, p.Predicate)
		}
	}
	return wp, inserts, nil
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

// present reports whether an identical conjunct is already an
// unconditional skeleton conjunct of the WHERE clause — the
// idempotence rule: hand-scoped queries are not double-woven. Guarded
// copies deliberately do not count (they vanish in guard-off shapes).
func (w *weaver) present(conjunct string) bool {
	if !w.segmentable {
		return false
	}
	want := normalizedTokens(w.profile, conjunct)
	if want == nil {
		return false
	}
	for _, seg := range w.segments {
		if tokensEqual(seg, want) {
			return true
		}
	}
	return false
}

func boundName(r dialect.RelRef) string {
	if r.Alias != "" {
		return r.Alias
	}
	return r.Table
}
