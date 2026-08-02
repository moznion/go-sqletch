package policy

import (
	"maps"
	"slices"
	"strings"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/template"
)

// tailKeywords bound the WHERE clause at paren depth 0 — the same
// vocabulary the scanner's clause tracking uses, plus set operators.
var tailKeywords = map[string]bool{
	"GROUP": true, "HAVING": true, "ORDER": true, "LIMIT": true,
	"OFFSET": true, "FETCH": true, "FOR": true, "RETURNING": true,
	"WINDOW": true, "UNION": true, "INTERSECT": true, "EXCEPT": true,
}

// joinKeywords end a join item (and therefore an ON expression) at
// paren depth 0.
var joinKeywords = map[string]bool{
	"JOIN": true, "INNER": true, "LEFT": true, "RIGHT": true,
	"FULL": true, "CROSS": true, "NATURAL": true, "STRAIGHT_JOIN": true,
}

// clauseScan is the shared result of lexically scanning a clause
// region (a WHERE clause or one join's ON expression) of a template's
// item stream: its depth-0 AND-separated conjunct segments in
// normalized token form, whether a depth-0 OR is present (segment
// matching would mis-model precedence, and an appended conjunct would
// bind below the OR — the caller must wrap), and the clause's extent.
type clauseScan struct {
	segs  [][]string
	hasOR bool
	lexOK bool
	// start is the absolute offset of the clause's first token; end is
	// just past its last content token (constructs included). Offsets
	// are meaningful only on scanned (pre-weave) templates — woven
	// items carry zero-width spans.
	start, end int
}

// segCollector accumulates normalized conjunct segments.
type segCollector struct {
	cs  clauseScan
	cur []string
}

func (c *segCollector) flush() {
	if len(c.cur) > 0 {
		c.cs.segs = append(c.cs.segs, c.cur)
		c.cur = nil
	}
}

func (c *segCollector) content(tok dialect.Token, abs int) {
	if c.cs.start < 0 {
		c.cs.start = abs
	}
	c.cs.end = abs + tok.End - tok.Start
	c.cur = append(c.cur, normalizeTok(tok))
}

// whereBoundary reports whether a construct item ends the WHERE
// clause (an ORDER BY/GROUP BY replacement does; conjunct-slot
// constructs are clause content).
func whereBoundary(it template.Item) bool {
	switch v := it.(type) {
	case *template.OrderBy:
		return true
	case *template.Choose:
		return v.Slot == template.SlotOrderBy || v.Slot == template.SlotGroupBy
	case *template.IfPresent:
		return v.Slot == template.SlotOrderBy || v.Slot == template.SlotGroupBy
	}
	return false
}

// whereClause scans the query's WHERE clause: segments for the
// idempotence/enforcement matchers, the OR flag for wrapping, and the
// clause end for the wrap's closing parenthesis. It works on the
// token stream alone (the first depth-0 WHERE keyword opens the
// clause), so scanned and woven templates read identically.
func whereClause(profile dialect.LexerProfile, q *template.QueryTemplate) clauseScan {
	col := &segCollector{cs: clauseScan{lexOK: true, start: -1, end: -1}}
	depth := 0
	inWhere := false
	for _, it := range q.Items {
		s, isSkel := it.(*template.Skeleton)
		if !isSkel {
			if inWhere {
				col.flush()
				if whereBoundary(it) {
					return col.cs
				}
				// Conjunct-slot construct: clause content.
				col.cs.end = it.Raw().End
			}
			continue
		}
		src := []byte(s.Text)
		pos := 0
		for {
			tok, err := profile.NextToken(src, pos)
			if err != nil {
				col.cs.lexOK = false
				return col.cs
			}
			if tok.Kind == dialect.KindEOF {
				break
			}
			pos = tok.End
			abs := s.Span.Start + tok.Start
			switch tok.Kind {
			case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment:
				continue
			case dialect.KindLParen:
				depth++
			case dialect.KindRParen:
				if depth > 0 {
					depth--
				}
			}
			if depth == 0 {
				switch tok.Kind {
				case dialect.KindSemicolon:
					col.flush()
					return col.cs
				case dialect.KindIdent:
					up := strings.ToUpper(tok.Text)
					if !inWhere {
						if up == "WHERE" {
							inWhere = true
						}
						continue
					}
					if tailKeywords[up] {
						col.flush()
						return col.cs
					}
					if up == "AND" {
						col.flush()
						continue
					}
					if up == "OR" {
						col.cs.hasOR = true
						col.flush()
						continue
					}
				}
			}
			if inWhere {
				col.content(tok, abs)
			}
		}
	}
	col.flush()
	return col.cs
}

// joinOnResult reports one join's ON expression, located from the
// designated relation's template offset (design 14 §D2(a)).
type joinOnResult struct {
	found     bool // ON expression located
	noOn      bool // USING/NATURAL/comma join, or no ON at all
	notInSkel bool // the relation reference is not in skeleton text
	cs        clauseScan
}

// joinOn scans forward from the relation reference at template offset
// relOff to the first ON expression at the reference's paren depth,
// and collects it exactly like whereClause collects the WHERE clause.
// For a LEFT JOIN's right operand that is the join's own ON; for a
// RIGHT/FULL JOIN's left operand the scan crosses the join keywords
// and the joined relation to the same clause; and for a relation
// nested in an inner join group under an outer join it finds the
// inner ON — scoping there filters the designated rows before the
// group is null-extended, which is equivalent. USING and NATURAL
// quals terminate the search (nothing to extend). The expression ends
// at a depth-0 join/tail/WHERE keyword, a depth-0 comma, a closing
// parenthesis beyond the reference's depth, a semicolon, any
// construct item, or the end of the statement.
func joinOn(profile dialect.LexerProfile, q *template.QueryTemplate, relOff int) joinOnResult {
	res := joinOnResult{cs: clauseScan{lexOK: true, start: -1, end: -1}}
	startItem := -1
	for i, it := range q.Items {
		if s, ok := it.(*template.Skeleton); ok &&
			s.Span.Start < s.Span.End && s.Span.Start <= relOff && relOff < s.Span.End {
			startItem = i
			break
		}
	}
	if startItem < 0 {
		res.notInSkel = true
		return res
	}

	col := &segCollector{cs: res.cs}
	depth := 0
	inOn := false
	finish := func() joinOnResult {
		col.flush()
		res.cs = col.cs
		res.found = inOn
		res.noOn = !inOn
		return res
	}
	for i := startItem; i < len(q.Items); i++ {
		s, isSkel := q.Items[i].(*template.Skeleton)
		if !isSkel {
			return finish()
		}
		src := []byte(s.Text)
		pos := 0
		if i == startItem {
			pos = relOff - s.Span.Start
		}
		for {
			tok, err := profile.NextToken(src, pos)
			if err != nil {
				res.cs.lexOK = false
				col.cs.lexOK = false
				return finish()
			}
			if tok.Kind == dialect.KindEOF {
				break
			}
			pos = tok.End
			abs := s.Span.Start + tok.Start
			switch tok.Kind {
			case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment:
				continue
			case dialect.KindLParen:
				depth++
			case dialect.KindRParen:
				if depth == 0 {
					// A closing paren beyond the relation's depth ends
					// the enclosing parenthesized join item.
					return finish()
				}
				depth--
			case dialect.KindSemicolon:
				if depth == 0 {
					return finish()
				}
			case dialect.KindComma:
				if depth == 0 {
					return finish()
				}
			case dialect.KindIdent:
				if depth == 0 {
					up := strings.ToUpper(tok.Text)
					switch {
					case !inOn && up == "ON":
						inOn = true
						continue
					case !inOn && (up == "USING" || up == "NATURAL"):
						return finish()
					case !inOn && (tailKeywords[up] || up == "WHERE" || up == "SET"):
						return finish()
					case !inOn && joinKeywords[up]:
						// The reference is a left operand: its ON lies
						// past the join keywords and the joined
						// relation. Keep scanning.
						continue
					case inOn && (joinKeywords[up] || tailKeywords[up] || up == "WHERE"):
						return finish()
					case inOn && up == "AND":
						col.flush()
						continue
					case inOn && up == "OR":
						col.cs.hasOR = true
						col.flush()
						continue
					}
				}
			}
			if inOn {
				col.content(tok, abs)
			}
		}
	}
	return finish()
}

// normalizedTokens lexes text into the same normalized form the
// clause scanners produce; nil when the text does not lex.
func normalizedTokens(profile dialect.LexerProfile, text string) []string {
	src := []byte(text)
	var out []string
	pos := 0
	for {
		tok, err := profile.NextToken(src, pos)
		if err != nil {
			return nil
		}
		if tok.Kind == dialect.KindEOF {
			return out
		}
		pos = tok.End
		switch tok.Kind {
		case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment:
			continue
		}
		out = append(out, normalizeTok(tok))
	}
}

// normalizeTok folds plain identifiers (every dialect here either
// folds unquoted identifiers or compares them case-insensitively);
// all other tokens compare verbatim.
func normalizeTok(tok dialect.Token) string {
	if tok.Kind == dialect.KindIdent {
		return strings.ToLower(tok.Text)
	}
	return tok.Text
}

func tokensEqual(a, b []string) bool { return slices.Equal(a, b) }

// segsContain reports whether any segment equals the conjunct's
// normalized token sequence.
func segsContain(profile dialect.LexerProfile, segs [][]string, conjunct string) bool {
	want := normalizedTokens(profile, conjunct)
	if want == nil {
		return false
	}
	for _, seg := range segs {
		if tokensEqual(seg, want) {
			return true
		}
	}
	return false
}

// insertion is one text splice at a template byte offset. prio orders
// coincident inserts: wrap-opening/closing parentheses (0) precede
// conjunct text (1), and ON-clause text precedes a WHERE clause
// synthesized at the same boundary (both via seq, which preserves
// creation order as the final tie-break).
type insertion struct {
	off  int
	prio int
	seq  int
	text string
}

// splice returns a copy of q with every insertion applied. Each woven
// Skeleton carries a zero-width span at its insertion point, so
// diagnostics that land in woven text attribute to the target query.
// An insertion whose offset lies in no skeleton item is dropped — the
// caller pre-validates positions (a designated relation inside a
// construct body is rejected before splicing).
func splice(q *template.QueryTemplate, ins []insertion) *template.QueryTemplate {
	if len(ins) == 0 {
		return q
	}
	slices.SortStableFunc(ins, func(a, b insertion) int {
		if a.off != b.off {
			return a.off - b.off
		}
		if a.prio != b.prio {
			return a.prio - b.prio
		}
		return a.seq - b.seq
	})

	// Assign each insertion to an item: the item the offset is
	// interior to, else the last item it ends.
	target := make([]int, len(ins))
	for k := range ins {
		target[k] = -1
		for i, it := range q.Items {
			if s, ok := it.(*template.Skeleton); ok && s.Span.Start <= ins[k].off && ins[k].off < s.Span.End {
				target[k] = i
				break
			}
		}
		if target[k] < 0 {
			for i, it := range q.Items {
				if s, ok := it.(*template.Skeleton); ok && ins[k].off == s.Span.End {
					target[k] = i
				}
			}
		}
	}

	clone := *q
	clone.Items = make([]template.Item, 0, len(q.Items)+2*len(ins))
	for i, it := range q.Items {
		s, isSkel := it.(*template.Skeleton)
		if !isSkel {
			clone.Items = append(clone.Items, it)
			continue
		}
		prev := s.Span.Start
		emitted := false
		for k := range ins {
			if target[k] != i {
				continue
			}
			off := ins[k].off
			if off > prev {
				clone.Items = append(clone.Items, &template.Skeleton{
					Text: s.Text[prev-s.Span.Start : off-s.Span.Start],
					Span: diagnostics.Span{File: s.Span.File, Start: prev, End: off},
				})
			}
			clone.Items = append(clone.Items, &template.Skeleton{
				Text: ins[k].text,
				Span: diagnostics.Span{File: s.Span.File, Start: off, End: off},
			})
			prev = off
			emitted = true
		}
		if !emitted {
			clone.Items = append(clone.Items, it)
			continue
		}
		if prev < s.Span.End {
			clone.Items = append(clone.Items, &template.Skeleton{
				Text: s.Text[prev-s.Span.Start:],
				Span: diagnostics.Span{File: s.Span.File, Start: prev, End: s.Span.End},
			})
		}
	}
	clone.Params = maps.Clone(q.Params)
	clone.ParamOrder = slices.Clone(q.ParamOrder)
	clone.TypeHints = maps.Clone(q.TypeHints)
	return &clone
}

// registerParams makes each woven policy's parameter an ordinary
// parameter of the woven template (D3(a)) and injects its declared
// type as a `-- @param`-equivalent hint when the query has none
// (design 14 §11.4). Conflicting hints were rejected before weaving.
func registerParams(woven *template.QueryTemplate, wps []WovenPolicy) {
	for _, wp := range wps {
		p := wp.Policy
		if p.ParamName == "" || wp.OptedOut {
			continue
		}
		if _, ok := woven.Params[p.ParamName]; !ok {
			woven.Params[p.ParamName] = &template.Param{Name: p.ParamName, GuardBit: -1}
			woven.ParamOrder = append(woven.ParamOrder, p.ParamName)
		}
		if p.ParamType != "" {
			if _, ok := woven.TypeHints[p.ParamName]; !ok {
				if woven.TypeHints == nil {
					woven.TypeHints = map[string]template.TypeHint{}
				}
				woven.TypeHints[p.ParamName] = template.TypeHint{
					SQLType: p.ParamType,
					Span:    woven.HeaderSpan,
				}
			}
		}
	}
}
