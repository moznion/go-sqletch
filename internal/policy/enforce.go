package policy

import (
	"strings"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/template"
)

// Enforce proves the codebase-level invariant on the WOVEN template
// (spec §"Cross-Query Policies"): for every top-level relation whose
// table a policy designates (and whose statement kind the policy
// covers), a conjunct token-identical to the policy's bound predicate
// is unconditional skeleton text — of the WHERE clause, or of the
// relation's own join's ON clause for a null-extended outer-join
// occurrence (§D2(a)) — hence present in every reachable shape. It
// re-derives presence from the template itself, independently of what
// the weaver decided, so a weaver regression surfaces as SQLETCH124
// instead of a silent leak.
//
// It also owns SQLETCH126: an opt-out naming an unknown policy, or
// one that does not apply to the query — renaming a policy must never
// silently disarm its opt-outs.
//
// tree and maxR are the parsed maximal rendering of the woven
// template and that rendering itself (the caller has both in hand;
// design 14 §6.1: this pass lives in cli.resolvedChecks). Unweavable
// positions are not re-checked here: they are SQLETCH125 at weave
// time, which already stops the pipeline before this pass.
func Enforce(profile dialect.LexerProfile, fe dialect.Frontend, pols []Policy, q *template.QueryTemplate,
	tree dialect.Tree, maxR ast.Rendering) []diagnostics.Diagnostic {


	var diags []diagnostics.Diagnostic

	kind := tree.Kind()
	rels := tree.Relations()
	deep := tree.DeepTables()
	// Independently re-derive the non-maximal overread set (a designated
	// read living only in a non-first @choose alternative or @order-by
	// @default). A correct weaver refuses these at weave time (SQLETCH125,
	// which stops the pipeline before this pass), so this fires only if
	// the weaver regressed and let one through — the same
	// catch-the-weaver role the conjunct re-derivation below plays.
	over := overreadDeep(profile, fe, q, deep)
	applies := func(p *Policy) bool {
		return analyzeApplicability(p, kind, rels, deep).active || len(designatedOverread(p, over, kind)) > 0
	}

	byName := map[string]*Policy{}
	for i := range pols {
		byName[pols[i].Name] = &pols[i]
	}

	// Opt-out sanity first, in declaration order.
	for _, o := range q.PolicyOptOuts {
		p, ok := byName[o.Policy]
		if !ok {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodePolicyBadOptOut, o.Span,
				"@policy-optout names unknown policy %q", o.Policy).
				WithHint("declared policies come from sqletch.yaml `policies:`; remove the opt-out or fix the name"))
			continue
		}
		if !applies(p) {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodePolicyBadOptOut, o.Span,
				"@policy-optout: policy %q does not apply to this query", o.Policy).
				WithHint("the query touches no table designated by %q (in a kind it covers); remove the opt-out", o.Policy))
		}
	}

	where := whereClause(profile, q)
	whereOK := where.lexOK && !where.hasOR
	onScans := map[int]*joinOnResult{}
	for i := range pols {
		p := &pols[i]
		a := analyzeApplicability(p, kind, rels, deep)
		if _, ok := optOutFor(q, p.Name); ok {
			continue
		}
		// Weaver-regression backstop: a designated read confined to a
		// non-maximal rendering should have been refused at weave time.
		// If it reached here, the woven output ships it unscoped — report
		// it rather than let it leak.
		if names := designatedOverread(p, over, kind); len(names) > 0 {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodePolicyUnscoped, q.HeaderSpan,
				"query reads policy-designated table %q inside a non-maximal rendering (a @choose alternative or @order-by @default) that policy %q cannot scope",
				names[0], p.Name).
				WithHint("that rendering is invisible to the weaver; restructure so the designated read appears in every shape, or opt out with `-- @policy-optout: %s (reason)`", p.Name))
			continue
		}
		if !a.active || a.hidden || kind == dialect.StmtInsert {
			continue
		}
		for _, r := range a.topOcc {
			conjunct := strings.ReplaceAll(p.Predicate, Placeholder, boundName(r))
			present := false
			if r.NullableSide {
				if res := onScanFor(profile, q, maxR, r, onScans); res != nil {
					present = res.found && res.cs.lexOK && !res.cs.hasOR &&
						segsContain(profile, res.cs.segs, conjunct)
				}
			} else {
				present = whereOK && segsContain(profile, where.segs, conjunct)
			}
			if !present {
				clause := "WHERE clause"
				if r.NullableSide {
					clause = "join's ON clause (the table is on a null-extended outer-join side)"
				}
				diags = append(diags, diagnostics.Errorf(diagnostics.CodePolicyUnscoped, q.HeaderSpan,
					"query touches policy-designated table %q without policy %q's scoping conjunct in every shape",
					r.Table, p.Name).
					WithHint("expected an unconditional conjunct `%s` in the %s; a copy inside @if-present does not count — it vanishes in guard-off shapes", conjunct, clause))
				break
			}
		}
	}
	return diags
}

// onScanFor maps the relation back to the template and memoizes the
// ON-clause scan; nil when the relation cannot be located (the
// conservative answer — the caller then reports the conjunct absent).
func onScanFor(profile dialect.LexerProfile, q *template.QueryTemplate, maxR ast.Rendering,
	r dialect.RelRef, memo map[int]*joinOnResult) *joinOnResult {

	if r.Loc < 0 {
		return nil
	}
	tOff, synth := maxR.Map.ToTemplate(r.Loc)
	if synth {
		return nil
	}
	if res, ok := memo[tOff]; ok {
		return res
	}
	res := joinOn(profile, q, tOff)
	memo[tOff] = &res
	return &res
}
