package rules

import (
	"strings"

	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect"
	"github.com/moznion/sqletch/internal/template"
)

// CheckLexical runs the catalog-free rule pass: R6 (anchored clauses)
// and R9 (parameter discipline). As a side effect it classifies each
// parameter's optionality (consumed by codegen). See
// docs/design/03-structural-rules.md §3–4.
func CheckLexical(profile dialect.LexerProfile, q *template.QueryTemplate) []diagnostics.Diagnostic {
	var diags []diagnostics.Diagnostic
	diags = append(diags, checkAnchors(profile, q)...)
	diags = append(diags, checkParamDiscipline(q)...)
	return diags
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
			if v.Slot == template.SlotSetItem && lastTok == "SET" {
				return []diagnostics.Diagnostic{diagnostics.Errorf(diagnostics.CodeUnanchoredSet, v.Span,
					"every SET item is optional; the shape with all guards off would be `UPDATE ... SET` with no assignments (R6)").
					WithHint("add an unconditional item, e.g. `updated_at = now()`")}
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
	for _, it := range q.Items {
		if c, ok := it.(*template.Choose); ok {
			chooseParams[c.Param] = c
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

		self := template.GuardAtom{Param: name}
		isGuard := p.GuardBit >= 0
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
					WithHint("bind it in the guarded fragment, or wait for @when (v0.3) for pure control parameters"))
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
	for _, g := range atoms {
		if g == a {
			return true
		}
	}
	return false
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
