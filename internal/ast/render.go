// Package ast implements phase P2: building verified renderings from a
// scanned template, with exact source maps back to the template file.
// The emission algorithm here is the byte-for-byte mirror of the
// runtime composer (premise P2); a shared conformance test pins the
// equality. See docs/design/02-rendering.md.
package ast

import (
	"fmt"
	"strings"

	"github.com/moznion/sqletch/internal/dialect"
	"github.com/moznion/sqletch/internal/template"
)

type RenderKind int

const (
	RenderMaximal RenderKind = iota
	RenderCase
	RenderOrderDefault // an @order-by @default body substituted
)

// OrderSelection selects the emission of each @order-by block (by
// block order): nil = maximal (all keys, declaration order), empty
// non-nil = default-or-omit, else a sequence of key<<1|desc elements.
type OrderSelection [][]uint8

// FragRange records where a construct's emission landed in the
// rendered SQL, including synthesized wrapping.
type FragRange struct {
	Item       template.Item
	Start, End int
}

type Rendering struct {
	Kind      RenderKind
	ChooseIdx int // which @choose block differs from maximal (RenderCase)
	CaseIdx   int // selected ordinal: 0..len(Cases)-1, len(Cases)=default
	OrderIdx  int // which @order-by block (RenderOrderDefault)
	SQL       string
	ParamsSeq []string // template param name per placeholder ($1 = [0])
	Frags     []FragRange
	Map       SourceMap
}

// Seg maps a rendered byte range back to the template.
type Seg struct {
	ROff, RLen int
	TOff       int
	Synth      bool
}

type SourceMap struct{ segs []Seg }

// ToTemplate translates a rendered offset to a template offset. For
// synthesized text it returns the anchor (the construct or param the
// synthesis belongs to) and synth=true.
func (m SourceMap) ToTemplate(rOff int) (tOff int, synth bool) {
	lo, hi := 0, len(m.segs)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		s := m.segs[mid]
		switch {
		case rOff < s.ROff:
			hi = mid - 1
		case rOff >= s.ROff+s.RLen:
			lo = mid + 1
		default:
			if s.Synth {
				return s.TOff, true
			}
			return s.TOff + (rOff - s.ROff), false
		}
	}
	return 0, true
}

// CaseSelection selects one ordinal per @choose block (by block order
// among the template's Choose items). Missing entries default to 0.
type CaseSelection map[int]int

// maxOrdinal returns the number of selectable ordinals of a choose
// block: named cases plus the default if present.
func maxOrdinal(c *template.Choose) int {
	n := len(c.Cases)
	if c.Default != nil {
		n++
	}
	return n
}

func caseBody(c *template.Choose, ord int) (body string, tOff int) {
	if ord < len(c.Cases) {
		return c.Cases[ord].Body, c.Cases[ord].Span.Start
	}
	if c.Default != nil {
		return c.Default.Body, c.Default.Span.Start
	}
	return "", c.Span.Start
}

// Renderings produces the full verified-rendering set: the maximal
// rendering first (ordinal 0 everywhere, all @order-by keys listed),
// then one rendering per remaining @choose ordinal, then one per
// @order-by @default body.
func Renderings(profile dialect.LexerProfile, q *template.QueryTemplate) ([]Rendering, error) {
	max, err := Render(profile, q, nil)
	if err != nil {
		return nil, err
	}
	out := []Rendering{max}
	chooseIdx, orderCount := 0, 0
	for _, it := range q.Items {
		switch c := it.(type) {
		case *template.Choose:
			for ord := 1; ord < maxOrdinal(c); ord++ {
				r, err := Render(profile, q, CaseSelection{chooseIdx: ord})
				if err != nil {
					return nil, err
				}
				r.Kind = RenderCase
				r.ChooseIdx = chooseIdx
				r.CaseIdx = ord
				out = append(out, r)
			}
			chooseIdx++
		case *template.OrderBy:
			orderCount++
		}
	}
	orderIdx := 0
	for _, it := range q.Items {
		o, ok := it.(*template.OrderBy)
		if !ok {
			continue
		}
		if o.Default != nil {
			orders := make(OrderSelection, orderCount)
			orders[orderIdx] = []uint8{} // empty non-nil = default
			r, err := renderCore(profile, q, allActive, nil, orders)
			if err != nil {
				return nil, err
			}
			r.Kind = RenderOrderDefault
			r.OrderIdx = orderIdx
			out = append(out, r)
		}
		orderIdx++
	}
	return out, nil
}

func allActive(*template.IfPresent) bool { return true }

// Render emits one rendering with every guard active and the given
// case selection. This is the reference implementation of premise P2's
// emission algorithm.
func Render(profile dialect.LexerProfile, q *template.QueryTemplate, sel CaseSelection) (Rendering, error) {
	return renderCore(profile, q, allActive, sel, nil)
}

// RenderShape emits the SQL of one concrete shape: an @if-present
// fragment is active iff every one of its guard atoms' bits is set in
// guardMask (bit order = q.GuardAtoms); orders selects each @order-by
// block's key sequence. Used by shape enumeration,
// `check --exhaustive`, and the property test.
func RenderShape(profile dialect.LexerProfile, q *template.QueryTemplate,
	guardMask uint64, sel CaseSelection, orders OrderSelection) (Rendering, error) {

	bit := map[template.GuardAtom]int{}
	for i, g := range q.GuardAtoms {
		bit[g] = i
	}
	active := func(v *template.IfPresent) bool {
		for _, g := range v.Guards {
			b, ok := bit[g]
			if !ok || guardMask&(1<<uint(b)) == 0 {
				return false
			}
		}
		return true
	}
	return renderCore(profile, q, active, sel, orders)
}

func renderCore(profile dialect.LexerProfile, q *template.QueryTemplate,
	active func(*template.IfPresent) bool, sel CaseSelection, orders OrderSelection) (Rendering, error) {

	r := &renderer{profile: profile, paramNum: map[string]int{}}
	chooseIdx, orderIdx := 0, 0
	for _, it := range q.Items {
		switch v := it.(type) {
		case *template.Skeleton:
			if err := r.emitVerbatim(v.Text, v.Span.Start); err != nil {
				return Rendering{}, err
			}
		case *template.IfPresent:
			if !active(v) {
				continue
			}
			r.emitSynth("\n", v.Span.Start)
			fragStart := r.len()
			switch v.Sep {
			case template.SepAnd:
				r.emitSynth("AND (", v.Span.Start)
			case template.SepComma:
				r.emitSynth(", ", v.Span.Start)
			}
			if err := r.emitVerbatim(v.Body, v.BodySpan.Start); err != nil {
				return Rendering{}, err
			}
			if v.Sep == template.SepAnd {
				r.emitSynth(")", v.Span.Start)
			}
			r.frags = append(r.frags, FragRange{Item: v, Start: fragStart, End: r.len()})
		case *template.Choose:
			ord := sel[chooseIdx]
			body, tOff := caseBody(v, ord)
			r.emitSynth("\n", v.Span.Start)
			fragStart := r.len()
			if body != "" {
				if err := r.emitVerbatim(body, tOff); err != nil {
					return Rendering{}, err
				}
			}
			r.frags = append(r.frags, FragRange{Item: v, Start: fragStart, End: r.len()})
			chooseIdx++
		case *template.OrderBy:
			var seq []uint8 // nil = maximal (all keys)
			if orders != nil && orderIdx < len(orders) {
				seq = orders[orderIdx]
			}
			r.emitSynth("\n", v.Span.Start)
			fragStart := r.len()
			switch {
			case seq == nil:
				r.emitSynth("ORDER BY ", v.Span.Start)
				for i, k := range v.Keys {
					if i > 0 {
						r.emitSynth(", ", v.Span.Start)
					}
					if err := r.emitVerbatim(k.Body, k.Span.Start); err != nil {
						return Rendering{}, err
					}
				}
			case len(seq) == 0:
				if v.Default != nil && v.Default.Body != "" {
					if err := r.emitVerbatim(v.Default.Body, v.Default.Span.Start); err != nil {
						return Rendering{}, err
					}
				}
			default:
				r.emitSynth("ORDER BY ", v.Span.Start)
				for i, e := range seq {
					if i > 0 {
						r.emitSynth(", ", v.Span.Start)
					}
					k := v.Keys[e>>1]
					if err := r.emitVerbatim(k.Body, k.Span.Start); err != nil {
						return Rendering{}, err
					}
					if e&1 == 1 {
						r.emitSynth(" DESC", v.Span.Start)
					}
				}
			}
			r.frags = append(r.frags, FragRange{Item: v, Start: fragStart, End: r.len()})
			orderIdx++
		case *template.InExpr:
			// Inline membership (PostgreSQL rendering): `= ANY($n)`.
			fragStart := r.len()
			r.emitSynth("= ANY(", v.Span.Start)
			r.emitParamRef(v.Param, v.Span.Start)
			r.emitSynth(")", v.Span.Start)
			r.frags = append(r.frags, FragRange{Item: v, Start: fragStart, End: r.len()})
		case *template.FilterTree:
			// Inline emission (the construct follows an unconditional
			// `AND ` in the skeleton). Maximal = all predicates
			// conjoined, each parenthesized, the whole wrapped — the
			// runtime tree And(p0..pn) is byte-identical.
			fragStart := r.len()
			r.emitSynth("(", v.Span.Start)
			for i, pr := range v.Predicates {
				if i > 0 {
					r.emitSynth(" AND ", v.Span.Start)
				}
				r.emitSynth("(", v.Span.Start)
				if err := r.emitVerbatim(pr.Body, pr.Span.Start); err != nil {
					return Rendering{}, err
				}
				r.emitSynth(")", v.Span.Start)
			}
			r.emitSynth(")", v.Span.Start)
			r.frags = append(r.frags, FragRange{Item: v, Start: fragStart, End: r.len()})
		}
	}
	return Rendering{
		Kind:      RenderMaximal,
		SQL:       r.sb.String(),
		ParamsSeq: r.paramSeq,
		Frags:     r.frags,
		Map:       SourceMap{segs: r.segs},
	}, nil
}

type renderer struct {
	profile  dialect.LexerProfile
	sb       strings.Builder
	segs     []Seg
	frags    []FragRange
	paramSeq []string
	paramNum map[string]int
}

func (r *renderer) len() int { return r.sb.Len() }

// emitParamRef emits a placeholder for a parameter that has no :name
// token in the template text (construct-owned bindings like @in).
func (r *renderer) emitParamRef(name string, anchorTOff int) {
	n, ok := r.paramNum[name]
	if !ok {
		n = len(r.paramSeq) + 1
		r.paramNum[name] = n
		r.paramSeq = append(r.paramSeq, name)
	}
	ph := fmt.Sprintf("$%d", n)
	r.segs = append(r.segs, Seg{ROff: r.sb.Len(), RLen: len(ph), TOff: anchorTOff, Synth: true})
	r.sb.WriteString(ph)
}

func (r *renderer) emitSynth(text string, anchorTOff int) {
	if text == "" {
		return
	}
	r.segs = append(r.segs, Seg{ROff: r.sb.Len(), RLen: len(text), TOff: anchorTOff, Synth: true})
	r.sb.WriteString(text)
}

// emitVerbatim copies text (whose first byte lives at template offset
// tOff) into the rendering, rewriting :name params to $n placeholders.
func (r *renderer) emitVerbatim(text string, tOff int) error {
	src := []byte(text)
	pos := 0
	runStart := 0
	flushRun := func(upTo int) {
		if upTo > runStart {
			r.segs = append(r.segs, Seg{
				ROff: r.sb.Len(), RLen: upTo - runStart, TOff: tOff + runStart,
			})
			r.sb.Write(src[runStart:upTo])
		}
	}
	for pos < len(src) {
		tok, err := r.profile.NextToken(src, pos)
		if err != nil {
			return fmt.Errorf("internal: lex error during render at %d: %w", tOff+pos, err)
		}
		if tok.Kind == dialect.KindEOF {
			break
		}
		if tok.Kind == dialect.KindParamRef {
			flushRun(tok.Start)
			name := tok.Text[1:]
			n, ok := r.paramNum[name]
			if !ok {
				n = len(r.paramSeq) + 1
				r.paramNum[name] = n
				r.paramSeq = append(r.paramSeq, name)
			}
			ph := fmt.Sprintf("$%d", n)
			r.segs = append(r.segs, Seg{
				ROff: r.sb.Len(), RLen: len(ph), TOff: tOff + tok.Start, Synth: true,
			})
			r.sb.WriteString(ph)
			runStart = tok.End
		}
		pos = tok.End
	}
	flushRun(len(src))
	return nil
}
