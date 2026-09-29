package policy

import (
	"strings"

	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/template"
)

// setOpKeywords separate the operands of a set operation. They are
// reserved words in every supported dialect, so a bare occurrence is
// always the operator (a dotted `t.union` is a column).
var setOpKeywords = map[string]bool{"UNION": true, "INTERSECT": true, "EXCEPT": true}

// coreTailKeywords open a SELECT core's post-WHERE clauses: a WHERE
// synthesized for a core that has none is inserted before the first of
// them (design 14 §13.3).
var coreTailKeywords = map[string]bool{
	"GROUP": true, "HAVING": true, "WINDOW": true, "ORDER": true,
	"LIMIT": true, "OFFSET": true, "FETCH": true, "FOR": true,
}

// branchScan is the lexical view of one set-operation branch — one
// leaf query core (design 14 §13).
type branchScan struct {
	// leadKw is the core's first keyword, uppercased (SELECT, VALUES,
	// TABLE); only a SELECT core can carry a WHERE clause.
	leadKw string
	// where is the core's WHERE clause (segments, OR flag, extent).
	where clauseScan
	// whereKwEnd is the template offset just past the core's WHERE
	// keyword, tailStart the offset of its first post-WHERE clause
	// keyword when it has no WHERE, end the offset just past its last
	// content — each -1 when absent. Like clauseScan offsets they are
	// meaningful only on scanned (pre-weave) templates.
	whereKwEnd, tailStart, end int
}

// setOpScan is the lexical split of a set-operation statement into its
// leaf cores, in document order — the independent counterpart of
// dialect.Tree.SetOpBranches that the weaver and the enforcement pass
// cross-check against it.
type setOpScan struct {
	// ok is false when the statement does not have the modeled shape
	// (then nothing may be woven into, or credited to, any branch).
	ok       bool
	branches []*branchScan
	// branchOf maps the template offset of every original (non-woven)
	// token inside a core to that core's index. It is how an AST
	// relation's location is attributed to a lexical branch.
	branchOf map[int]int
}

// soElem is one significant element of a query's item stream: a token
// of a skeleton item, or a whole construct item.
type soElem struct {
	tok    dialect.Token
	up     string // uppercased text of an identifier token
	abs    int    // template offset of the token
	synth  bool   // the token belongs to woven (synthesized) text
	depth  int    // paren depth OUTSIDE the token ('(' and ')' included)
	dotted bool   // the token follows a qualifier '.'
	item   template.Item
}

func (e soElem) ident(up string) bool {
	return e.item == nil && e.tok.Kind == dialect.KindIdent && !e.dotted && e.up == up
}

// scanSetOp splits the query's token stream into its set-operation
// branches. The modeled grammar (PostgreSQL, MySQL, SQLite):
//
//	query   := [WITH cte {, cte}] operand {setop [ALL|DISTINCT] operand} tail
//	operand := '(' query ')' | core
//
// A core runs from its first token to the next set operator at its own
// paren depth, the ')' closing its enclosing paren, or the statement
// end; an unparenthesized last core lexically swallows the set-level
// ORDER BY/LIMIT, which is harmless (they follow its WHERE slot). The
// tail after a parenthesized last operand belongs to no core. Anything
// off this shape — a construct where an operand must start, an
// unbalanced paren, trailing junk — yields ok=false.
func scanSetOp(profile dialect.LexerProfile, q *template.QueryTemplate) setOpScan {
	elems, ok := setOpElems(profile, q)
	if !ok {
		return setOpScan{}
	}
	p := &setOpParser{elems: elems, out: setOpScan{ok: true, branchOf: map[int]int{}}}
	p.query(0)
	if p.failed {
		return setOpScan{}
	}
	// Only the statement terminator may follow.
	for ; p.i < len(p.elems); p.i++ {
		if e := p.elems[p.i]; e.item != nil || e.tok.Kind != dialect.KindSemicolon {
			return setOpScan{}
		}
	}
	return p.out
}

// setOpElems flattens the item stream into significant elements.
func setOpElems(profile dialect.LexerProfile, q *template.QueryTemplate) ([]soElem, bool) {
	var out []soElem
	depth := 0
	prevDot := false
	for _, it := range q.Items {
		s, isSkel := it.(*template.Skeleton)
		if !isSkel {
			prevDot = false
			out = append(out, soElem{item: it, depth: depth, abs: it.Raw().Start})
			continue
		}
		src := []byte(s.Text)
		pos := 0
		for {
			tok, err := profile.NextToken(src, pos)
			if err != nil {
				return nil, false
			}
			if tok.Kind == dialect.KindEOF {
				break
			}
			pos = tok.End
			switch tok.Kind {
			case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment:
				continue
			}
			e := soElem{tok: tok, abs: s.Span.Start + tok.Start, synth: s.Synth, dotted: prevDot}
			prevDot = tok.Kind == dialect.KindOther && tok.Text == "."
			if tok.Kind == dialect.KindIdent {
				e.up = strings.ToUpper(tok.Text)
			}
			if tok.Kind == dialect.KindRParen {
				if depth == 0 {
					return nil, false
				}
				depth--
			}
			e.depth = depth
			if tok.Kind == dialect.KindLParen {
				depth++
			}
			out = append(out, e)
		}
	}
	return out, depth == 0
}

type setOpParser struct {
	elems  []soElem
	i      int
	out    setOpScan
	failed bool
}

func (p *setOpParser) peek() (soElem, bool) {
	if p.i >= len(p.elems) {
		return soElem{}, false
	}
	return p.elems[p.i], true
}

// query parses `[WITH …] operand {setop operand} tail` at paren depth d
// and stops before the ')' closing depth d (or at the statement end).
func (p *setOpParser) query(d int) {
	if e, ok := p.peek(); ok && e.ident("WITH") {
		p.skipWith(d)
	}
	p.operand(d)
	for !p.failed {
		e, ok := p.peek()
		if !ok || e.item != nil || e.depth != d || e.tok.Kind != dialect.KindIdent || e.dotted || !setOpKeywords[e.up] {
			break
		}
		p.i++
		if q, ok := p.peek(); ok && (q.ident("ALL") || q.ident("DISTINCT")) {
			p.i++
		}
		p.operand(d)
	}
	// Set-level tail (only non-empty after a parenthesized operand):
	// everything up to the ')' closing depth d or a statement-level
	// semicolon belongs to no core.
	for ; !p.failed && p.i < len(p.elems); p.i++ {
		e := p.elems[p.i]
		if e.item == nil && (e.depth < d || (e.depth == d && e.tok.Kind == dialect.KindSemicolon)) {
			return
		}
	}
}

// skipWith consumes a WITH clause at depth d: `WITH [RECURSIVE] name
// [(cols)] AS [NOT] [MATERIALIZED] (body) {, …}`.
func (p *setOpParser) skipWith(d int) {
	p.i++ // WITH
	if e, ok := p.peek(); ok && e.ident("RECURSIVE") {
		p.i++
	}
	for {
		// name
		if e, ok := p.peek(); !ok || e.item != nil || (e.tok.Kind != dialect.KindIdent && e.tok.Kind != dialect.KindQuotedIdent) {
			p.failed = true
			return
		}
		p.i++
		// optional column list
		if e, ok := p.peek(); ok && e.item == nil && e.tok.Kind == dialect.KindLParen {
			p.skipParen(d)
		}
		if e, ok := p.peek(); !ok || !e.ident("AS") {
			p.failed = true
			return
		}
		p.i++
		if e, ok := p.peek(); ok && e.ident("NOT") {
			p.i++
		}
		if e, ok := p.peek(); ok && e.ident("MATERIALIZED") {
			p.i++
		}
		if e, ok := p.peek(); !ok || e.item != nil || e.tok.Kind != dialect.KindLParen {
			p.failed = true
			return
		}
		p.skipParen(d)
		if p.failed {
			return
		}
		if e, ok := p.peek(); ok && e.item == nil && e.depth == d && e.tok.Kind == dialect.KindComma {
			p.i++
			continue
		}
		return
	}
}

// skipParen consumes a '(' at depth d through its matching ')'.
func (p *setOpParser) skipParen(d int) {
	p.i++ // '('
	for ; p.i < len(p.elems); p.i++ {
		e := p.elems[p.i]
		if e.item == nil && e.depth == d && e.tok.Kind == dialect.KindRParen {
			p.i++
			return
		}
	}
	p.failed = true
}

// operand parses one operand at depth d: a parenthesized query or a
// core.
func (p *setOpParser) operand(d int) {
	e, ok := p.peek()
	if !ok || e.item != nil || e.depth != d {
		p.failed = true
		return
	}
	if e.tok.Kind == dialect.KindLParen {
		p.i++
		p.query(d + 1)
		if p.failed {
			return
		}
		if c, ok := p.peek(); !ok || c.item != nil || c.depth != d || c.tok.Kind != dialect.KindRParen {
			p.failed = true
			return
		}
		p.i++
		return
	}
	p.core(d)
}

// core scans one leaf query core at depth d, recording its WHERE
// clause and insertion points.
func (p *setOpParser) core(d int) {
	idx := len(p.out.branches)
	b := &branchScan{whereKwEnd: -1, tailStart: -1, end: -1,
		where: clauseScan{lexOK: true, start: -1, end: -1}}
	p.out.branches = append(p.out.branches, b)
	if e := p.elems[p.i]; e.tok.Kind == dialect.KindIdent {
		b.leadKw = e.up
	}
	var ex *exprScan
	sawFrom := false
	for ; p.i < len(p.elems); p.i++ {
		e := p.elems[p.i]
		if e.item != nil {
			if ex != nil && ex.construct(e.item) {
				b.where = ex.result()
				ex = nil
			}
			if ex == nil && sawFrom && b.whereKwEnd < 0 && b.tailStart < 0 && whereBoundary(e.item) {
				b.tailStart = e.item.Raw().Start
			}
			b.end = e.item.Raw().End
			continue
		}
		if e.depth < d ||
			(e.depth == d && e.tok.Kind == dialect.KindSemicolon) ||
			(e.depth == d && e.tok.Kind == dialect.KindIdent && !e.dotted && setOpKeywords[e.up]) {
			break
		}
		if !e.synth {
			p.out.branchOf[e.abs] = idx
		}
		if ex != nil && ex.step(e.tok, e.up, e.abs, e.depth == d, e.dotted) {
			b.where = ex.result()
			ex = nil
		}
		if ex == nil && e.depth == d && e.tok.Kind == dialect.KindIdent && !e.dotted {
			switch {
			case e.up == "FROM":
				sawFrom = true
			case e.up == "WHERE" && b.whereKwEnd < 0:
				ex = newExprScan()
				b.whereKwEnd = e.abs + (e.tok.End - e.tok.Start)
			case sawFrom && b.whereKwEnd < 0 && b.tailStart < 0 && coreTailKeywords[e.up]:
				b.tailStart = e.abs
			}
		}
		b.end = e.abs + (e.tok.End - e.tok.Start)
	}
	if ex != nil {
		b.where = ex.result()
	}
}
