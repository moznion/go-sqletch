// Package rules implements the structural rule checks R1–R9.
// This file is R1 (slot legality + node completeness), which runs in
// phase P2's pipeline position; catalog-free and catalog-dependent
// rule passes (R2–R9) arrive in P3. See docs/design/02-rendering.md §4
// and 03-structural-rules.md.
package rules

import (
	"errors"
	"fmt"
	"strings"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/template"
)

// CheckR1 validates that every fragment is one complete AST node in
// its slot and that every rendering parses as a single DML statement.
// rs must come from ast.Renderings (maximal first).
//
// Order matters for diagnostics quality: fragment-local probes run
// first (they pinpoint the offending fragment), then whole-rendering
// parses, then AST-membership consistency checks on the maximal tree.
func CheckR1(profile dialect.LexerProfile, fe dialect.Frontend,
	q *template.QueryTemplate, rs []ast.Rendering) []diagnostics.Diagnostic {

	var diags []diagnostics.Diagnostic

	// 1. Fragment-local probes (design 02 §4). Probe inputs have
	// :name params rewritten to $n — the dialect grammar knows nothing
	// about named params.
	for _, it := range q.Items {
		switch v := it.(type) {
		case *template.IfPresent:
			switch v.Slot {
			case template.SlotWhereConjunct, template.SlotHavingConjunct:
				diags = append(diags, probeConjunct(profile, fe, v)...)
			case template.SlotJoinItem:
				diags = append(diags, probeJoin(profile, fe, v)...)
			case template.SlotSetItem:
				diags = append(diags, probeSetItem(profile, fe, v)...)
			case template.SlotInsertValue:
				diags = append(diags, probeInsertValue(profile, fe, v)...)
				// SlotInsertColumn needs no probe: the scanner already
				// requires a single identifier.
			}
		case *template.Choose:
			diags = append(diags, probeChooseCases(profile, fe, v)...)
		case *template.OrderBy:
			diags = append(diags, probeOrderKeys(profile, fe, v)...)
		case *template.FilterTree:
			for _, pr := range v.Predicates {
				if pr.Body == "" {
					continue
				}
				if !balanced(profile, pr.Body) {
					diags = append(diags, diagnostics.Errorf(diagnostics.CodeNodeIncomplete, pr.Span,
						"@predicate body has unbalanced parentheses (R1)"))
					continue
				}
				if err := fe.ProbeExpr(rewriteParams(profile, pr.Body)); err != nil {
					diags = append(diags, diagnostics.Errorf(diagnostics.CodeNodeIncomplete, pr.Span,
						"@predicate body must be a single boolean expression (R1): %s", probeMsg(err)))
				}
			}
		}
	}

	// 2. Every rendering parses as exactly one DML statement.
	trees := make([]dialect.Tree, len(rs))
	for i, r := range rs {
		tree, err := fe.Parse(r.SQL)
		if err != nil {
			span := q.HeaderSpan
			msg := err.Error()
			if pe, ok := errors.AsType[*dialect.ParseError](err); ok {
				tOff, _ := r.Map.ToTemplate(pe.Pos)
				span = diagnostics.Span{File: q.HeaderSpan.File, Start: tOff, End: tOff + 1}
				msg = pe.Msg
			}
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeRenderingParse, span,
				"generated SQL does not parse: %s", msg))
			continue
		}
		if tree.StmtCount() != 1 || tree.Kind() == dialect.StmtOther {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeNotSingleDML, q.HeaderSpan,
				"a query must be exactly one SELECT/UPDATE/INSERT/DELETE statement"))
			continue
		}
		trees[i] = tree
	}
	if trees[0] == nil {
		return diags // no maximal tree; membership checks would be noise
	}
	maxTree, maxR := trees[0], rs[0]

	// 3. AST-membership consistency on the parsed maximal rendering.
	for _, fr := range maxR.Frags {
		switch v := fr.Item.(type) {
		case *template.IfPresent:
			switch v.Slot {
			case template.SlotWhereConjunct:
				diags = append(diags, checkConjunctMembership(maxTree, v, fr)...)
			case template.SlotJoinItem:
				diags = append(diags, checkJoinMembership(maxTree, v, fr)...)
			}
		}
	}
	for i, r := range rs {
		if trees[i] == nil {
			continue
		}
		diags = append(diags, checkOrderByContainment(trees[i], r)...)
	}

	// @filter-tree conjunct membership runs on the empty-tree rendering,
	// not the maximal: the maximal conjunction AND-flattens through its
	// parentheses into several top-level conjuncts, but the empty form
	// is the single constant TRUE — it must be exactly one top-level
	// conjunct, or the runtime's TRUE fallback would not substitute the
	// whole construct (e.g. under OR precedence).
	for i, r := range rs {
		if r.Kind != ast.RenderTreeEmpty || trees[i] == nil {
			continue
		}
		treeIdx := 0
		for _, fr := range r.Frags {
			ft, ok := fr.Item.(*template.FilterTree)
			if !ok {
				continue
			}
			if treeIdx == r.TreeIdx {
				n := 0
				for _, loc := range trees[i].TopConjunctLocs() {
					if loc >= fr.Start && loc < fr.End {
						n++
					}
				}
				if n != 1 {
					diags = append(diags, diagnostics.Errorf(diagnostics.CodeNodeIncomplete, ft.Span,
						"@filter-tree does not occupy one whole WHERE conjunct: its empty rendering TRUE maps to %d top-level conjuncts, want exactly 1 (R1)", n).
						WithHint("give the construct its own conjunct: `WHERE TRUE` then `AND @filter-tree(...)`"))
				}
			}
			treeIdx++
		}
	}

	// @order-by clause-coupling restrictions (review fixes F2/F1a of
	// the spec cycle): DISTINCT ON is prefix-order-sensitive, and
	// WITH TIES makes ORDER BY mandatory.
	for _, it := range q.Items {
		o, ok := it.(*template.OrderBy)
		if !ok {
			continue
		}
		if maxTree.HasDistinctOn() {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeOrderByDistinct, o.Span,
				"@order-by cannot be combined with DISTINCT ON: its ORDER BY validity depends on key order and prefix, which breaks the subset/permutation argument").
				WithHint("use @choose with whole ORDER BY clauses instead"))
		}
		if maxTree.HasFetchWithTies() && o.Default == nil {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeOrderByNeedsDflt, o.Span,
				"FETCH FIRST … WITH TIES makes ORDER BY mandatory; declare an @order-by @default so the clause can never vanish"))
		}
	}
	return diags
}

func probeOrderKeys(profile dialect.LexerProfile, fe dialect.Frontend,
	o *template.OrderBy) []diagnostics.Diagnostic {

	var diags []diagnostics.Diagnostic
	for _, k := range o.Keys {
		if k.Body == "" {
			continue // scanner already reported
		}
		if !balanced(profile, k.Body) {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeNodeIncomplete, k.Span,
				"@key body has unbalanced parentheses (R1)"))
			continue
		}
		if err := fe.ProbeOrderByKey(rewriteParams(profile, k.Body)); err != nil {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeNodeIncomplete, k.Span,
				"@key body must be exactly one sort key (R1): %s", probeMsg(err)))
		}
	}
	if o.Default != nil && o.Default.Body != "" {
		if err := fe.ProbeOrderBy(rewriteParams(profile, o.Default.Body)); err != nil {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeNodeIncomplete, o.Default.Span,
				"the @order-by @default body must be exactly one ORDER BY clause (R1): %s", probeMsg(err)))
		}
	}
	return diags
}

func probeConjunct(profile dialect.LexerProfile, fe dialect.Frontend,
	v *template.IfPresent) []diagnostics.Diagnostic {

	if !balanced(profile, v.Body) {
		return []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodeNodeIncomplete, v.BodySpan,
			"fragment has unbalanced parentheses; it must form one complete predicate (R1)")}
	}
	if err := fe.ProbeExpr(rewriteParams(profile, v.Body)); err != nil {
		return []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodeNodeIncomplete, v.BodySpan,
			"fragment must be a single predicate expression (R1): %s", probeMsg(err))}
	}
	return nil
}

func probeJoin(profile dialect.LexerProfile, fe dialect.Frontend,
	v *template.IfPresent) []diagnostics.Diagnostic {

	if !balanced(profile, v.Body) {
		return []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodeNodeIncomplete, v.BodySpan,
			"fragment has unbalanced parentheses; it must form one complete join item (R1)")}
	}
	if err := fe.ProbeJoinItem(rewriteParams(profile, v.Body)); err != nil {
		return []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodeNodeIncomplete, v.BodySpan,
			"fragment must be a single join item (R1): %s", probeMsg(err))}
	}
	return nil
}

func probeSetItem(profile dialect.LexerProfile, fe dialect.Frontend,
	v *template.IfPresent) []diagnostics.Diagnostic {

	if !balanced(profile, v.Body) {
		return []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodeNodeIncomplete, v.BodySpan,
			"fragment has unbalanced parentheses; it must form one complete SET assignment (R1)")}
	}
	if err := fe.ProbeSetItem(rewriteParams(profile, v.Body)); err != nil {
		return []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodeNodeIncomplete, v.BodySpan,
			"fragment must be a single SET assignment (R1): %s", probeMsg(err))}
	}
	return nil
}

func probeInsertValue(profile dialect.LexerProfile, fe dialect.Frontend,
	v *template.IfPresent) []diagnostics.Diagnostic {

	if !balanced(profile, v.Body) {
		return []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodeNodeIncomplete, v.BodySpan,
			"fragment has unbalanced parentheses; it must form one complete VALUES item (R1)")}
	}
	if err := fe.ProbeInsertValue(rewriteParams(profile, v.Body)); err != nil {
		return []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodeNodeIncomplete, v.BodySpan,
			"fragment must be a single VALUES item (R1): %s", probeMsg(err))}
	}
	return nil
}

func probeChooseCases(profile dialect.LexerProfile, fe dialect.Frontend,
	c *template.Choose) []diagnostics.Diagnostic {

	var probe func(string) error
	var what string
	switch c.Slot {
	case template.SlotOrderBy:
		probe, what = fe.ProbeOrderBy, "one ORDER BY clause"
	case template.SlotGroupBy:
		probe, what = fe.ProbeGroupBy, "one GROUP BY clause"
	case template.SlotProjExpr:
		probe, what = fe.ProbeExpr, "one expression"
	default:
		return nil // scanner already rejected the block
	}

	var diags []diagnostics.Diagnostic
	check := func(body string, span diagnostics.Span) {
		if body == "" {
			return // scanner enforces non-empty where required
		}
		if !balanced(profile, body) {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeNodeIncomplete, span,
				"@case body has unbalanced parentheses (R1)"))
			return
		}
		if err := probe(rewriteParams(profile, body)); err != nil {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeNodeIncomplete, span,
				"@case body must be exactly %s (R1): %s", what, probeMsg(err)))
		}
	}
	for _, cs := range c.Cases {
		check(cs.Body, cs.Span)
	}
	if c.Default != nil {
		check(c.Default.Body, c.Default.Span)
	}
	return diags
}

func checkConjunctMembership(maxTree dialect.Tree, v *template.IfPresent,
	fr ast.FragRange) []diagnostics.Diagnostic {

	n := 0
	for _, loc := range maxTree.TopConjunctLocs() {
		if loc >= fr.Start && loc < fr.End {
			n++
		}
	}
	if n != 1 {
		return []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodeNodeIncomplete, v.BodySpan,
			"fragment maps to %d WHERE conjuncts, want exactly 1 (R1)", n)}
	}
	return nil
}

func checkJoinMembership(maxTree dialect.Tree, v *template.IfPresent,
	fr ast.FragRange) []diagnostics.Diagnostic {

	var diags []diagnostics.Diagnostic
	found := 0
	for _, rel := range maxTree.Relations() {
		if rel.Loc < fr.Start || rel.Loc >= fr.End {
			continue
		}
		found++
		if rel.Join != dialect.JoinInner && rel.Join != dialect.JoinLeft {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeJoinTypeForbidden, v.BodySpan,
				"optional joins must be INNER or LEFT; %s null-extends or multiplies skeleton rows, "+
					"which would change result nullability per shape (R2)", rel.Join))
		}
	}
	if found == 0 {
		diags = append(diags, diagnostics.Errorf(diagnostics.CodeNodeIncomplete, v.BodySpan,
			"fragment does not introduce a relation in the FROM clause (R1)"))
	}
	return diags
}

func checkOrderByContainment(tree dialect.Tree, r ast.Rendering) []diagnostics.Diagnostic {
	var ownerFr *ast.FragRange
	var ownerSpan diagnostics.Span
	for i := range r.Frags {
		switch v := r.Frags[i].Item.(type) {
		case *template.Choose:
			if v.Slot == template.SlotOrderBy {
				ownerFr, ownerSpan = &r.Frags[i], v.Span
			}
		case *template.OrderBy:
			ownerFr, ownerSpan = &r.Frags[i], v.Span
		}
		if ownerFr != nil {
			break
		}
	}
	if ownerFr == nil {
		return nil
	}
	for _, loc := range tree.OrderByLocs() {
		if loc < ownerFr.Start || loc >= ownerFr.End {
			return []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodeNodeIncomplete, ownerSpan,
				"a sort key outside the construct coexists with its ORDER BY (R1); "+
					"the statement may have only one ORDER BY owner")}
		}
	}
	return nil
}

func probeMsg(err error) string {
	if pe, ok := errors.AsType[*dialect.ParseError](err); ok {
		return pe.Msg
	}
	return err.Error()
}

// rewriteParams replaces :name refs with the dialect's placeholders
// ($n or ?) so probe inputs are valid dialect SQL. Numbering is
// per-occurrence; probe checks are purely syntactic, so stable
// numbering is not required.
func rewriteParams(profile dialect.LexerProfile, body string) string {
	question := dialect.StyleOf(profile) == dialect.PlaceholderQuestion
	src := []byte(body)
	var b strings.Builder
	pos, n := 0, 0
	for {
		tok, err := profile.NextToken(src, pos)
		if err != nil || tok.Kind == dialect.KindEOF {
			b.Write(src[pos:])
			return b.String()
		}
		if tok.Kind == dialect.KindParamRef {
			b.Write(src[pos:tok.Start])
			if question {
				b.WriteByte('?')
			} else {
				n++
				fmt.Fprintf(&b, "$%d", n)
			}
			pos = tok.End
			continue
		}
		b.Write(src[pos:tok.End])
		pos = tok.End
	}
}

// balanced reports whether the fragment's parentheses are balanced and
// never negative — the lexer-level precondition that makes the
// wrap-in-parens probe sound (design 02 §4).
func balanced(profile dialect.LexerProfile, body string) bool {
	src := []byte(body)
	pos, depth := 0, 0
	for {
		tok, err := profile.NextToken(src, pos)
		if err != nil || tok.Kind == dialect.KindEOF {
			return err == nil && depth == 0
		}
		switch tok.Kind {
		case dialect.KindLParen:
			depth++
		case dialect.KindRParen:
			depth--
			if depth < 0 {
				return false
			}
		}
		pos = tok.End
	}
}
