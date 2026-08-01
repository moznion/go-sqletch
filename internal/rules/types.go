package rules

import (
	"github.com/moznion/sqletch/internal/ast"
	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect"
	"github.com/moznion/sqletch/internal/template"
)

// ParamType is one template parameter's pinned type (premise P1),
// ordered like q.ParamOrder.
type ParamType struct {
	Name string
	Type dialect.TypeRef
}

// CheckTypeAgreement verifies the cross-rendering obligations of
// design 04 §5: every rendering agrees with the maximal one on result
// columns (SQLETCH210) and every template parameter has a single type
// across all renderings that bind it (SQLETCH211). descs[i] must
// correspond to rs[i].
func CheckTypeAgreement(q *template.QueryTemplate, rs []ast.Rendering,
	descs []dialect.Desc) []diagnostics.Diagnostic {

	var diags []diagnostics.Diagnostic
	if len(rs) == 0 || len(rs) != len(descs) {
		return nil
	}
	base := descs[0]

	for i := 1; i < len(descs); i++ {
		d := descs[i]
		if len(d.Columns) != len(base.Columns) {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeColumnAgreement,
				caseSpan(q, rs[i]),
				"this @choose case yields %d result columns; the maximal rendering yields %d (R2)",
				len(d.Columns), len(base.Columns)))
			continue
		}
		for c := range d.Columns {
			if d.Columns[c].Name != base.Columns[c].Name || d.Columns[c].Type.OID != base.Columns[c].Type.OID {
				diags = append(diags, diagnostics.Errorf(diagnostics.CodeColumnAgreement,
					caseSpan(q, rs[i]),
					"result column %d is %q %s here but %q %s in the maximal rendering; all cases must agree (R2)",
					c+1, d.Columns[c].Name, d.Columns[c].Type.Name,
					base.Columns[c].Name, base.Columns[c].Type.Name))
			}
		}
	}

	_, paramDiags := ResolveParamTypes(q, rs, descs)
	diags = append(diags, paramDiags...)

	// @when literal/SQL type agreement: when a control parameter also
	// binds in SQL, the literal-derived Go type must match the
	// SQL-inferred one (spec: @when, checked in Phase 3/4).
	types := map[string]dialect.TypeRef{}
	resolved, _ := ResolveParamTypes(q, rs, descs)
	for _, pt := range resolved {
		types[pt.Name] = pt.Type
	}
	for _, g := range q.GuardAtoms {
		if !g.IsValue() {
			continue
		}
		tr, bound := types[g.Param]
		if !bound {
			continue
		}
		if !oidCompatibleWithLiteral(tr.OID, g.Kind) {
			diags = append(diags, diagnostics.Errorf(diagnostics.CodeParamAgreement,
				paramSpan(q, g.Param),
				"@when compares %q (SQL type %s) against a %s literal; the types must agree (premise P1)",
				g.Param, tr.Name, kindName(g.Kind)))
		}
	}
	return diags
}

func oidCompatibleWithLiteral(oid uint32, k template.ValueKind) bool {
	switch k {
	case template.ValueString:
		switch oid {
		case 18, 19, 25, 1042, 1043, 2950:
			return true
		}
	case template.ValueInt:
		switch oid {
		case 20, 21, 23, 26, 700, 701, 1700:
			return true
		}
	case template.ValueBool:
		return oid == 16
	}
	return false
}

func kindName(k template.ValueKind) string {
	switch k {
	case template.ValueString:
		return "string"
	case template.ValueInt:
		return "integer"
	case template.ValueBool:
		return "boolean"
	}
	return "unknown"
}

// ResolveParamTypes unifies each template parameter's type across all
// renderings (they must agree — SQLETCH211) and returns the pinned
// types in q.ParamOrder.
func ResolveParamTypes(q *template.QueryTemplate, rs []ast.Rendering,
	descs []dialect.Desc) ([]ParamType, []diagnostics.Diagnostic) {

	var diags []diagnostics.Diagnostic
	types := map[string]dialect.TypeRef{}
	for i, r := range rs {
		if i >= len(descs) {
			break
		}
		for pos, name := range r.ParamsSeq {
			if pos >= len(descs[i].Params) {
				continue
			}
			tr := descs[i].Params[pos]
			prev, seen := types[name]
			if !seen {
				types[name] = tr
				continue
			}
			if prev.OID != tr.OID {
				diags = append(diags, diagnostics.Errorf(diagnostics.CodeParamAgreement,
					paramSpan(q, name),
					"parameter %q is typed %s in one rendering and %s in another; all renderings must agree (premise P1)",
					":"+name, prev.Name, tr.Name).
					WithHint("add an explicit cast (e.g. :%s::%s) to pin the type", name, prev.Name))
				break
			}
		}
	}

	var out []ParamType
	for _, name := range q.ParamOrder {
		if tr, ok := types[name]; ok {
			out = append(out, ParamType{Name: name, Type: tr})
		}
	}
	return out, diags
}

func caseSpan(q *template.QueryTemplate, r ast.Rendering) diagnostics.Span {
	if r.Kind == ast.RenderOrderDefault {
		idx := 0
		for _, it := range q.Items {
			if o, ok := it.(*template.OrderBy); ok {
				if idx == r.OrderIdx && o.Default != nil {
					return o.Default.Span
				}
				idx++
			}
		}
		return q.HeaderSpan
	}
	if r.Kind != ast.RenderCase {
		return q.HeaderSpan
	}
	idx := 0
	for _, it := range q.Items {
		c, ok := it.(*template.Choose)
		if !ok {
			continue
		}
		if idx == r.ChooseIdx {
			if r.CaseIdx < len(c.Cases) {
				return c.Cases[r.CaseIdx].Span
			}
			if c.Default != nil {
				return c.Default.Span
			}
			return c.Span
		}
		idx++
	}
	return q.HeaderSpan
}

func paramSpan(q *template.QueryTemplate, name string) diagnostics.Span {
	if p := q.Params[name]; p != nil && len(p.Occurrences) > 0 {
		return p.Occurrences[0].Span
	}
	return q.HeaderSpan
}
