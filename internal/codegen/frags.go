// Package codegen emits the generated Go package: fragment tables,
// typed params/row structs, and one function per query. See
// docs/design/06-codegen-runtime.md.
package codegen

import (
	"github.com/moznion/sqletch/internal/dialect"
	"github.com/moznion/sqletch/internal/template"
	"github.com/moznion/sqletch/runtime"
)

// BuildFrags lowers a scanned template into the runtime fragment
// table. Parameter tokens are located at generate time (via the same
// lexer profile as everything else) so runtime composition never scans
// SQL. Param indices refer to positions in q.ParamOrder.
//
// The conformance test asserts that runtime.Compose over this table is
// byte-identical to ast.RenderShape for every enumerable shape — the
// implementation of "what is verified is byte-wise what is composed".
func BuildFrags(profile dialect.LexerProfile, q *template.QueryTemplate) []runtime.Frag {
	bit := map[template.GuardAtom]int{}
	for i, g := range q.GuardAtoms {
		bit[g] = i
	}
	paramIdx := map[string]int16{}
	for i, name := range q.ParamOrder {
		paramIdx[name] = int16(i)
	}

	var frags []runtime.Frag
	for _, it := range q.Items {
		switch v := it.(type) {
		case *template.Skeleton:
			f := runtime.Frag{Kind: runtime.Skel, Text: v.Text}
			f.ParamSpans, f.ParamIdx = paramSpans(profile, v.Text, paramIdx)
			frags = append(frags, f)
		case *template.IfPresent:
			var mask uint64
			for _, g := range v.Guards {
				mask |= 1 << uint(bit[g])
			}
			f := runtime.Frag{Kind: runtime.Guarded, Text: v.Body, GuardMask: mask}
			switch v.Sep {
			case template.SepAnd:
				f.Sep = runtime.SepAnd
			case template.SepComma:
				f.Sep = runtime.SepComma
			}
			f.ParamSpans, f.ParamIdx = paramSpans(profile, v.Body, paramIdx)
			frags = append(frags, f)
		case *template.Choose:
			f := runtime.Frag{Kind: runtime.Choose}
			addCase := func(body string) {
				c := runtime.Case{Text: body}
				c.ParamSpans, c.ParamIdx = paramSpans(profile, body, paramIdx)
				f.Cases = append(f.Cases, c)
			}
			for _, cs := range v.Cases {
				addCase(cs.Body)
			}
			if v.Default != nil {
				addCase(v.Default.Body)
			}
			frags = append(frags, f)
		case *template.FilterTree:
			// Predicate ParamIdx values index the LEAF's argument list
			// (the predicate's distinct params in order), not the
			// params struct — the composer offsets them per leaf
			// instance into the flattened TreeArgs space.
			f := runtime.Frag{Kind: runtime.FilterTree}
			for _, pr := range v.Predicates {
				local := map[string]int16{}
				for i, name := range pr.Params {
					local[name] = int16(i)
				}
				c := runtime.Case{Text: pr.Body}
				c.ParamSpans, c.ParamIdx = paramSpans(profile, pr.Body, local)
				f.Cases = append(f.Cases, c)
			}
			frags = append(frags, f)
		case *template.OrderBy:
			f := runtime.Frag{Kind: runtime.OrderBy}
			for _, k := range v.Keys {
				c := runtime.Case{Text: k.Body}
				c.ParamSpans, c.ParamIdx = paramSpans(profile, k.Body, paramIdx)
				f.Cases = append(f.Cases, c)
			}
			if v.Default != nil && v.Default.Body != "" {
				d := runtime.Case{Text: v.Default.Body}
				d.ParamSpans, d.ParamIdx = paramSpans(profile, v.Default.Body, paramIdx)
				f.Default = &d
			}
			frags = append(frags, f)
		}
	}
	return frags
}

func paramSpans(profile dialect.LexerProfile, text string, paramIdx map[string]int16) ([]runtime.Span, []int16) {
	src := []byte(text)
	var spans []runtime.Span
	var idx []int16
	pos := 0
	for {
		tok, err := profile.NextToken(src, pos)
		if err != nil || tok.Kind == dialect.KindEOF {
			return spans, idx
		}
		if tok.Kind == dialect.KindParamRef {
			if i, ok := paramIdx[tok.Text[1:]]; ok {
				spans = append(spans, runtime.Span{Start: int32(tok.Start), End: int32(tok.End)})
				idx = append(idx, i)
			}
		}
		pos = tok.End
	}
}
