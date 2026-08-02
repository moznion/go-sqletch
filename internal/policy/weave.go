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
	// identical hand-written conjunct was already present.
	Conjuncts []string
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

// apply runs one policy against the query. It returns the coverage
// record (nil when the policy does not touch the query), the conjunct
// texts to insert, and diagnostics.
func (w *weaver) apply(p *Policy) (*WovenPolicy, []string, []diagnostics.Diagnostic) {
	var topOcc []dialect.RelRef
	topCount := map[string]int{}
	for _, r := range w.rels {
		if r.Table != "" && p.designates(r.Table) {
			topOcc = append(topOcc, r)
			topCount[strings.ToLower(r.Table)]++
		}
	}
	for _, tr := range w.deep {
		if p.designates(tr.Name) {
			topCount[strings.ToLower(tr.Name)]--
		}
	}
	hidden := false
	for _, n := range topCount {
		if n < 0 {
			hidden = true
		}
	}

	// INSERT: the target relation is never a weave target (no rows are
	// filtered), but a designated table read by an INSERT … SELECT
	// body is a position the weaver cannot scope (design 14 §D6) —
	// checked only when the policy covers reads at all.
	if w.kind == dialect.StmtInsert {
		if hidden && p.coversSelect() {
			return nil, nil, []diagnostics.Diagnostic{w.unweavable(p, w.q.HeaderSpan,
				"a designated table is read inside this INSERT's SELECT body, which sqletch cannot scope")}
		}
		return nil, nil, nil
	}
	if !p.appliesTo(w.kind) {
		return nil, nil, nil
	}
	if hidden {
		return nil, nil, []diagnostics.Diagnostic{w.unweavable(p, w.q.HeaderSpan,
			"a designated table appears inside a subquery, CTE, or set-operation branch, which sqletch cannot scope")}
	}
	if len(topOcc) == 0 {
		return nil, nil, nil
	}

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
