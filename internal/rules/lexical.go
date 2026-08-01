package rules

import (
	"slices"
	"strings"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/template"
)

// CheckLexical runs the catalog-free rule pass: R6 (anchored clauses)
// and R9 (parameter discipline). As a side effect it classifies each
// parameter's optionality (consumed by codegen). See
// docs/design/03-structural-rules.md §3–4.
func CheckLexical(profile dialect.LexerProfile, q *template.QueryTemplate) []diagnostics.Diagnostic {
	var diags []diagnostics.Diagnostic
	diags = append(diags, checkAnchors(profile, q)...)
	diags = append(diags, checkInsertPairing(q)...)
	diags = append(diags, checkParamDiscipline(q)...)
	return diags
}

// checkInsertPairing implements R7: guarded INSERT column items and
// each VALUES row's guarded value items must carry identical guard
// sets in identical order. Combined with the scan-time tail rule and
// the maximal Describe, this guarantees column/value alignment in
// every shape.
func checkInsertPairing(q *template.QueryTemplate) []diagnostics.Diagnostic {
	cols := q.InsertColGuards
	rows := q.InsertValGuards
	hasRowGuards := false
	for _, r := range rows {
		if len(r) > 0 {
			hasRowGuards = true
		}
	}
	if len(cols) == 0 && !hasRowGuards {
		return nil
	}
	var diags []diagnostics.Diagnostic
	if len(cols) > 0 && len(rows) == 0 {
		return []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodePairedGuards, cols[0].Span,
			"optional INSERT columns require a VALUES clause with matching optional items (R7)")}
	}
	for r, row := range rows {
		if len(row) != len(cols) {
			span := q.HeaderSpan
			if len(row) > 0 {
				span = row[0].Span
			} else if len(cols) > 0 {
				span = cols[0].Span
			}
			diags = append(diags, diagnostics.Errorf(diagnostics.CodePairedGuards, span,
				"VALUES row %d has %d optional items but the column list has %d; every guarded column needs its guarded value in every row (R7)",
				r+1, len(row), len(cols)))
			continue
		}
		for i := range row {
			if !sameAtoms(row[i].Guards, cols[i].Guards) {
				diags = append(diags, diagnostics.Errorf(diagnostics.CodePairedGuards, row[i].Span,
					"VALUES row %d item %d is guarded by @if-present(%s) but the paired column %q is guarded by @if-present(%s); pairs must share the same guard (R7)",
					r+1, i+1, atomsParamList(row[i].Guards), cols[i].Name, atomsParamList(cols[i].Guards)))
			}
		}
	}
	return diags
}

func sameAtoms(a, b []template.GuardAtom) bool {
	return supersetAtoms(a, b) && supersetAtoms(b, a)
}

// checkAnchors implements R6 for v0.1: if the first optional WHERE
// conjunct directly follows the WHERE keyword, every conjunct is
// optional and the minimal shape would render `WHERE AND …` — the
// author must write the `WHERE TRUE` anchor.
func checkAnchors(profile dialect.LexerProfile, q *template.QueryTemplate) []diagnostics.Diagnostic {
	lastTok := ""
	for _, it := range q.Items {
		switch v := it.(type) {
		case *template.Skeleton:
			src := []byte(v.Text)
			pos := 0
			for {
				tok, err := profile.NextToken(src, pos)
				if err != nil || tok.Kind == dialect.KindEOF {
					break
				}
				switch tok.Kind {
				case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment:
				case dialect.KindIdent:
					lastTok = strings.ToUpper(tok.Text)
				default:
					lastTok = tok.Text
				}
				pos = tok.End
			}
		case *template.IfPresent:
			if v.Slot == template.SlotWhereConjunct && lastTok == "WHERE" {
				return []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodeUnanchoredClause, v.Span,
					"every WHERE conjunct is optional; the shape with all guards off would be invalid SQL (R6)").
					WithHint("write `WHERE TRUE` as the unconditional anchor")}
			}
			if v.Slot == template.SlotHavingConjunct && lastTok == "HAVING" {
				return []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodeUnanchoredClause, v.Span,
					"every HAVING conjunct is optional; the shape with all guards off would be invalid SQL (R6)").
					WithHint("write `HAVING TRUE` as the unconditional anchor")}
			}
			if v.Slot == template.SlotSetItem && lastTok == "SET" {
				return []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodeUnanchoredSet, v.Span,
					"every SET item is optional; the shape with all guards off would be `UPDATE ... SET` with no assignments (R6)").
					WithHint("add an unconditional item, e.g. `updated_at = now()`")}
			}
			if (v.Slot == template.SlotInsertColumn || v.Slot == template.SlotInsertValue) && lastTok == "(" {
				return []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodeUnanchoredSet, v.Span,
					"every item of this INSERT list is optional; the shape with all guards off would leave the parentheses empty (R6)").
					WithHint("keep at least one unconditional column/value pair")}
			}
			lastTok = "@construct"
		case *template.Choose:
			lastTok = "@construct"
		}
	}
	return nil
}

func checkParamDiscipline(q *template.QueryTemplate) []diagnostics.Diagnostic {
	var diags []diagnostics.Diagnostic

	chooseParams := map[string]*template.Choose{}
	orderParams := map[string]*template.OrderBy{}
	filterParams := map[string]*template.FilterTree{}
	for _, it := range q.Items {
		switch c := it.(type) {
		case *template.Choose:
			chooseParams[c.Param] = c
		case *template.OrderBy:
			orderParams[c.Param] = c
		case *template.FilterTree:
			filterParams[c.Param] = c
		}
	}

	for _, name := range q.ParamOrder {
		p := q.Params[name]

		if c, isChoose := chooseParams[name]; isChoose {
			if len(p.Occurrences) > 0 {
				diags = append(diags, diagnostics.Errorf(diagnostics.CodeChooseParamBinds,
					p.Occurrences[0].Span,
					"%q is a @choose control parameter; it selects a case and cannot also bind as a SQL value (R9)", ":"+name))
			}
			if p.GuardBit >= 0 {
				diags = append(diags, diagnostics.Errorf(diagnostics.CodeChooseParamBinds, c.Span,
					"%q cannot be both a @choose parameter and an @if-present guard (R9)", name))
			}
			continue
		}
		if o, isOrder := orderParams[name]; isOrder {
			if len(p.Occurrences) > 0 {
				diags = append(diags, diagnostics.Errorf(diagnostics.CodeChooseParamBinds,
					p.Occurrences[0].Span,
					"%q is an @order-by control parameter; it selects sort keys and cannot also bind as a SQL value (R9)", ":"+name))
			}
			if p.GuardBit >= 0 {
				diags = append(diags, diagnostics.Errorf(diagnostics.CodeChooseParamBinds, o.Span,
					"%q cannot be both an @order-by parameter and a guard (R9)", name))
			}
			continue
		}
		if ft, isFilter := filterParams[name]; isFilter {
			if len(p.Occurrences) > 0 {
				diags = append(diags, diagnostics.Errorf(diagnostics.CodeChooseParamBinds,
					p.Occurrences[0].Span,
					"%q is a @filter-tree control parameter; it carries the tree and cannot also bind as a SQL value (R9)", ":"+name))
			}
			if p.GuardBit >= 0 {
				diags = append(diags, diagnostics.Errorf(diagnostics.CodeChooseParamBinds, ft.Span,
					"%q cannot be both a @filter-tree parameter and a guard (R9)", name))
			}
			continue
		}
		// Predicate params are constructor arguments; mixing them with
		// non-tree bind sites would need two sources for one name.
		inFT, outFT := false, false
		for _, occ := range p.Occurrences {
			if occ.InFilterTree {
				inFT = true
			} else {
				outFT = true
			}
		}
		if inFT && outFT {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeChooseParamBinds,
				p.Occurrences[0].Span,
				"%q binds both inside a @predicate (constructor argument) and outside it; use distinct parameter names (R9)", ":"+name))
			continue
		}
		if inFT {
			p.Optional = false
			continue
		}

		hasPresence, hasValue := false, false
		for _, g := range q.GuardAtoms {
			if g.Param != name {
				continue
			}
			if g.IsValue() {
				hasValue = true
			} else {
				hasPresence = true
			}
		}
		if hasPresence && hasValue {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeVacuousGuard,
				guardSpan(q, template.GuardAtom{Param: name}),
				"%q is used both as an @if-present guard (presence, pointer) and in @when (value, required); pick one (R9)", name))
			p.Optional = false
			continue
		}
		if hasValue {
			// @when control parameter: required, typed by the literal;
			// it may also bind in SQL (agreement checked in phase 4).
			p.Optional = false
			continue
		}

		self := template.GuardAtom{Param: name}
		isGuard := p.GuardBit >= 0 && hasPresence
		hasSelfGuarded := false
		var firstUnguarded *diagnostics.Span
		allSelfGuarded := len(p.Occurrences) > 0
		for i := range p.Occurrences {
			occ := &p.Occurrences[i]
			if !occ.InChooseCase && containsAtom(occ.Guards, self) {
				hasSelfGuarded = true
			} else {
				allSelfGuarded = false
				if firstUnguarded == nil {
					firstUnguarded = &occ.Span
				}
			}
		}

		if isGuard {
			switch {
			case !hasSelfGuarded:
				diags = append(diags, diagnostics.Errorf(diagnostics.CodeGuardNeverBinds,
					guardSpan(q, self),
					"guard parameter %q never binds inside a fragment it guards; its Go type would be uninferable (R9)", name).
					WithHint("bind it in the guarded fragment, or use @when for pure control parameters"))
			case !allSelfGuarded:
				diags = append(diags, diagnostics.Errorf(diagnostics.CodeVacuousGuard, *firstUnguarded,
					"%q binds outside its own guard, making it required — guarding on a required parameter is always true (R9)", ":"+name))
			}
		}
		p.Optional = isGuard && allSelfGuarded && hasSelfGuarded
	}
	return diags
}

func containsAtom(atoms []template.GuardAtom, a template.GuardAtom) bool {
	return slices.Contains(atoms, a)
}

// guardSpan finds the span of the first @if-present using the atom.
func guardSpan(q *template.QueryTemplate, a template.GuardAtom) diagnostics.Span {
	for _, it := range q.Items {
		if ip, ok := it.(*template.IfPresent); ok && containsAtom(ip.Guards, a) {
			return ip.Span
		}
	}
	return q.HeaderSpan
}
