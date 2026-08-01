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
)

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
// rendering first (ordinal 0 everywhere), then one rendering per
// remaining ordinal of each @choose block.
func Renderings(profile dialect.LexerProfile, q *template.QueryTemplate) ([]Rendering, error) {
	max, err := Render(profile, q, nil)
	if err != nil {
		return nil, err
	}
	out := []Rendering{max}
	chooseIdx := 0
	for _, it := range q.Items {
		c, ok := it.(*template.Choose)
		if !ok {
			continue
		}
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
	}
	return out, nil
}

// Render emits one rendering with every guard active and the given
// case selection. This is the reference implementation of premise P2's
// emission algorithm.
func Render(profile dialect.LexerProfile, q *template.QueryTemplate, sel CaseSelection) (Rendering, error) {
	return renderCore(profile, q, func(*template.IfPresent) bool { return true }, sel)
}

// RenderShape emits the SQL of one concrete shape: an @if-present
// fragment is active iff every one of its guard atoms' bits is set in
// guardMask (bit order = q.GuardAtoms). Used by shape enumeration,
// `check --exhaustive`, and the property test.
func RenderShape(profile dialect.LexerProfile, q *template.QueryTemplate,
	guardMask uint64, sel CaseSelection) (Rendering, error) {

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
	return renderCore(profile, q, active, sel)
}

func renderCore(profile dialect.LexerProfile, q *template.QueryTemplate,
	active func(*template.IfPresent) bool, sel CaseSelection) (Rendering, error) {

	r := &renderer{profile: profile, paramNum: map[string]int{}}
	chooseIdx := 0
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
			if v.Sep == template.SepAnd {
				r.emitSynth("AND (", v.Span.Start)
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
