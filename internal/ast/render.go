// Package ast implements phase P2: building verified renderings from a
// scanned template, with exact source maps back to the template file.
// The emission algorithm here is the byte-for-byte mirror of the
// runtime composer (premise P2); a shared conformance test pins the
// equality. See docs/design/02-rendering.md.
package ast

import (
	"fmt"
	"strings"

	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/template"
)

type RenderKind int

const (
	RenderMaximal RenderKind = iota
	RenderCase
	RenderOrderDefault // an @order-by @default body substituted
	RenderInEmpty      // an @in at arity 0 (expanding dialects only)
	RenderTreeEmpty    // a @filter-tree with the empty tree (renders TRUE)
)

// OrderSelection selects the emission of each @order-by block (by
// block order): nil = maximal (all keys, declaration order), empty
// non-nil = default-or-omit, else a sequence of key<<1|desc elements.
type OrderSelection [][]uint8

// InSelection selects the representative arity of each @in construct
// (by template order) on expanding dialects: 1 (default when nil or
// short) renders one placeholder — the verified stand-in for every
// non-empty list, since IN-list growth is parse-invariant — and 0
// renders the empty-list form. Ignored on dollar-style dialects.
type InSelection []uint8

// TreeSelection selects the emission of each @filter-tree block (by
// template order): 1 (default when nil or short) renders the maximal
// conjunction of all predicates, 0 renders the empty tree — the
// literal TRUE the runtime emits for a nil/Unscoped tree. The two are
// the tree space's verified representatives; arbitrary trees are sound
// by the compositional argument (spec, `@filter-tree`).
type TreeSelection []uint8

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
	InIdx     int // which @in construct (RenderInEmpty)
	TreeIdx   int // which @filter-tree block (RenderTreeEmpty)
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

// RenderingCount reports how many renderings Renderings would produce
// for q WITHOUT allocating any of them — the count is a linear sum, so
// a caller can refuse a pathological template (many @choose/@order-by/
// @filter-tree blocks over a large skeleton, each rendering a fresh
// full-SQL copy) before the set is materialised and exhausts memory.
// It MUST stay in lock-step with Renderings below: the maximal
// rendering, plus one per additional @choose ordinal, one per
// @order-by @default body, one per @in occurrence (question-style
// dialects only), and one per @filter-tree.
func RenderingCount(profile dialect.LexerProfile, q *template.QueryTemplate) int {
	n := 1 // maximal
	for _, it := range q.Items {
		switch c := it.(type) {
		case *template.Choose:
			if extra := maxOrdinal(c) - 1; extra > 0 {
				n += extra
			}
		case *template.OrderBy:
			if c.Default != nil {
				n++
			}
		case *template.FilterTree:
			n++
		}
	}
	if dialect.StyleOf(profile) == dialect.PlaceholderQuestion {
		for _, it := range q.Items {
			if _, ok := it.(*template.InExpr); ok {
				n++
			}
		}
	}
	return n
}

// Renderings produces the full verified-rendering set: the maximal
// rendering first (ordinal 0 everywhere, all @order-by keys listed),
// then one rendering per remaining @choose ordinal, then one per
// @order-by @default body. RenderingCount above projects len(result)
// without allocating; keep the two in lock-step.
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
			r, err := renderCore(profile, q, allActive, nil, orders, nil, nil)
			if err != nil {
				return nil, err
			}
			r.Kind = RenderOrderDefault
			r.OrderIdx = orderIdx
			out = append(out, r)
		}
		orderIdx++
	}
	// Expanding dialects: the arity-0 form of each @in is its own
	// verified rendering (the non-empty form is in the maximal).
	if dialect.StyleOf(profile) == dialect.PlaceholderQuestion {
		inCount := 0
		for _, it := range q.Items {
			if _, ok := it.(*template.InExpr); ok {
				inCount++
			}
		}
		for i := 0; i < inCount; i++ {
			ins := make(InSelection, inCount)
			for j := range ins {
				ins[j] = 1
			}
			ins[i] = 0
			r, err := renderCore(profile, q, allActive, nil, nil, ins, nil)
			if err != nil {
				return nil, err
			}
			r.Kind = RenderInEmpty
			r.InIdx = i
			out = append(out, r)
		}
	}
	// The empty form of each @filter-tree is its own verified rendering:
	// the runtime emits TRUE for a nil/Unscoped tree, so that shape must
	// be parsed, described, and planned like any other (the maximal
	// conjunction is in the maximal rendering).
	treeCount := 0
	for _, it := range q.Items {
		if _, ok := it.(*template.FilterTree); ok {
			treeCount++
		}
	}
	for i := 0; i < treeCount; i++ {
		trees := make(TreeSelection, treeCount)
		for j := range trees {
			trees[j] = 1
		}
		trees[i] = 0
		r, err := renderCore(profile, q, allActive, nil, nil, nil, trees)
		if err != nil {
			return nil, err
		}
		r.Kind = RenderTreeEmpty
		r.TreeIdx = i
		out = append(out, r)
	}
	return out, nil
}

func allActive(*template.IfPresent) bool { return true }

// Render emits one rendering with every guard active and the given
// case selection. This is the reference implementation of premise P2's
// emission algorithm.
func Render(profile dialect.LexerProfile, q *template.QueryTemplate, sel CaseSelection) (Rendering, error) {
	return renderCore(profile, q, allActive, sel, nil, nil, nil)
}

// RenderShape emits the SQL of one concrete shape: an @if-present
// fragment is active iff every one of its guard atoms' bits is set in
// guardMask (bit order = q.GuardAtoms); orders selects each @order-by
// block's key sequence; ins selects each @in construct's
// representative arity (expanding dialects). Used by shape
// enumeration, `check --exhaustive`, and the property test.
func RenderShape(profile dialect.LexerProfile, q *template.QueryTemplate,
	guardMask uint64, sel CaseSelection, orders OrderSelection, ins InSelection) (Rendering, error) {

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
	return renderCore(profile, q, active, sel, orders, ins, nil)
}

func renderCore(profile dialect.LexerProfile, q *template.QueryTemplate,
	active func(*template.IfPresent) bool, sel CaseSelection, orders OrderSelection,
	ins InSelection, trees TreeSelection) (Rendering, error) {

	r := &renderer{
		profile:  profile,
		paramNum: map[string]int{},
		question: dialect.StyleOf(profile) == dialect.PlaceholderQuestion,
	}
	chooseIdx, orderIdx, inIdx, treeIdx := 0, 0, 0, 0
	for _, it := range q.Items {
		switch v := it.(type) {
		case *template.Skeleton:
			if err := r.emitText(v.Text, v.Span.Start, v.Synth); err != nil {
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
			fragStart := r.len()
			switch {
			case !r.question:
				// PostgreSQL: `= ANY($n)`, one static shape.
				r.emitSynth("= ANY(", v.Span.Start)
				r.emitParamRef(v.Param, v.Span.Start)
				r.emitSynth(")", v.Span.Start)
			case inIdx < len(ins) && ins[inIdx] == 0:
				// Expanding dialects, arity 0: the dialect's
				// matches-nothing emission, no bind at all.
				r.emitSynth(dialect.InEmptyOf(profile), v.Span.Start)
			default:
				// Expanding dialects, representative arity 1.
				r.emitSynth("IN (", v.Span.Start)
				r.emitParamRef(v.Param, v.Span.Start)
				r.emitSynth(")", v.Span.Start)
			}
			inIdx++
			r.frags = append(r.frags, FragRange{Item: v, Start: fragStart, End: r.len()})
		case *template.FilterTree:
			// Inline emission (the construct follows an unconditional
			// `AND ` in the skeleton, enforced at scan time and by the
			// R1 membership check on the empty rendering). Maximal =
			// all predicates conjoined, each parenthesized, the whole
			// wrapped — the runtime tree And(p0..pn) is byte-identical.
			// The empty selection emits TRUE, byte-identical to the
			// runtime's nil/Unscoped emission.
			fragStart := r.len()
			if treeIdx < len(trees) && trees[treeIdx] == 0 {
				r.emitSynth("TRUE", v.Span.Start)
			} else {
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
			}
			treeIdx++
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
	question bool // '?' per occurrence (Tier 2) vs numbered $n with reuse
}

func (r *renderer) len() int { return r.sb.Len() }

// placeholder returns the next placeholder token for name and records
// it in ParamsSeq. Dollar style dedups by name; question style appends
// one entry per occurrence.
func (r *renderer) placeholder(name string) string {
	if r.question {
		r.paramSeq = append(r.paramSeq, name)
		return "?"
	}
	n, ok := r.paramNum[name]
	if !ok {
		n = len(r.paramSeq) + 1
		r.paramNum[name] = n
		r.paramSeq = append(r.paramSeq, name)
	}
	return fmt.Sprintf("$%d", n)
}

// emitParamRef emits a placeholder for a parameter that has no :name
// token in the template text (construct-owned bindings like @in).
func (r *renderer) emitParamRef(name string, anchorTOff int) {
	ph := r.placeholder(name)
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
	return r.emitText(text, tOff, false)
}

// emitText is emitVerbatim with a synth switch. synth marks text that
// exists in no template file (a policy-woven Skeleton, whose
// zero-width span sits at the insertion offset): every emitted
// segment — text runs and placeholders alike — is then a synthesized
// segment anchored at tOff, so ToTemplate never attributes woven
// bytes to the unrelated template text after the insertion point.
func (r *renderer) emitText(text string, tOff int, synth bool) error {
	src := []byte(text)
	pos := 0
	runStart := 0
	flushRun := func(upTo int) {
		if upTo > runStart {
			seg := Seg{ROff: r.sb.Len(), RLen: upTo - runStart, TOff: tOff + runStart}
			if synth {
				seg.TOff = tOff
				seg.Synth = true
			}
			r.segs = append(r.segs, seg)
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
			ph := r.placeholder(tok.Text[1:])
			anchor := tOff + tok.Start
			if synth {
				anchor = tOff
			}
			r.segs = append(r.segs, Seg{
				ROff: r.sb.Len(), RLen: len(ph), TOff: anchor, Synth: true,
			})
			r.sb.WriteString(ph)
			runStart = tok.End
		}
		pos = tok.End
	}
	flushRun(len(src))
	return nil
}
