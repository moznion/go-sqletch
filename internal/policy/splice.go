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

// skeletonConjuncts returns the query's unconditional WHERE conjuncts
// as normalized token sequences: the depth-0 AND-separated segments of
// skeleton text between the WHERE keyword and the clause's end. It
// works on the token stream alone (the first depth-0 WHERE keyword
// opens the clause), so it treats scanned and woven templates — whose
// synthesized items carry zero-width spans — identically. Construct
// items end the current segment (a conjunct never spans a construct
// boundary). ok is false when the clause has a depth-0 OR — segment
// splitting would mis-model precedence, so the caller treats nothing
// as present.
func skeletonConjuncts(profile dialect.LexerProfile, q *template.QueryTemplate) (segs [][]string, ok bool) {
	var cur []string
	depth := 0
	inWhere := false
	flush := func() {
		if len(cur) > 0 {
			segs = append(segs, cur)
			cur = nil
		}
	}
	for _, it := range q.Items {
		s, isSkel := it.(*template.Skeleton)
		if !isSkel {
			if inWhere {
				flush()
			}
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
					flush()
					return segs, true
				case dialect.KindIdent:
					up := strings.ToUpper(tok.Text)
					if !inWhere {
						if up == "WHERE" {
							inWhere = true
						}
						continue
					}
					if tailKeywords[up] {
						flush()
						return segs, true
					}
					if up == "AND" {
						flush()
						continue
					}
					if up == "OR" {
						return nil, false
					}
				}
			}
			if inWhere {
				cur = append(cur, normalizeTok(tok))
			}
		}
	}
	flush()
	return segs, true
}

// normalizedTokens lexes text into the same normalized form
// skeletonConjuncts produces; nil when the text does not lex.
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

// splice returns a copy of q with the conjuncts inserted
// unconditionally into the WHERE clause: appended right after the
// WHERE keyword, or as a synthesized WHERE clause before the tail
// clauses (or at the statement's end) when the query has none. The
// woven Skeleton carries a zero-width span at the insertion point, so
// diagnostics that land in woven text attribute to the target query.
func splice(q *template.QueryTemplate, conjuncts []string) *template.QueryTemplate {
	joined := strings.Join(conjuncts, " AND ")
	var off int
	var text string
	switch {
	case q.WhereKwEnd >= 0:
		off, text = q.WhereKwEnd, " "+joined+" AND"
	case q.TailStart >= 0:
		off, text = q.TailStart, "WHERE "+joined+" "
	case q.StmtEnd >= 0:
		off, text = q.StmtEnd, " WHERE "+joined
	default:
		return q
	}

	// The insertion point lies in exactly one Skeleton item; at an
	// item boundary, prefer splitting the item the offset is interior
	// to, falling back to the one it ends.
	target := -1
	for i, it := range q.Items {
		if s, ok := it.(*template.Skeleton); ok && s.Span.Start <= off && off < s.Span.End {
			target = i
			break
		}
	}
	if target < 0 {
		for i, it := range q.Items {
			if s, ok := it.(*template.Skeleton); ok && off == s.Span.End {
				target = i
			}
		}
	}
	if target < 0 {
		return q
	}

	clone := *q
	clone.Items = make([]template.Item, 0, len(q.Items)+2)
	for i, it := range q.Items {
		if i != target {
			clone.Items = append(clone.Items, it)
			continue
		}
		s := it.(*template.Skeleton)
		rel := off - s.Span.Start
		if rel > 0 {
			clone.Items = append(clone.Items, &template.Skeleton{
				Text: s.Text[:rel],
				Span: diagnostics.Span{File: s.Span.File, Start: s.Span.Start, End: off},
			})
		}
		clone.Items = append(clone.Items, &template.Skeleton{
			Text: text,
			Span: diagnostics.Span{File: s.Span.File, Start: off, End: off},
		})
		if rel < len(s.Text) {
			clone.Items = append(clone.Items, &template.Skeleton{
				Text: s.Text[rel:],
				Span: diagnostics.Span{File: s.Span.File, Start: off, End: s.Span.End},
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
		if p.ParamName == "" {
			continue
		}
		if _, ok := woven.Params[p.ParamName]; !ok {
			// Policy records who injected this parameter. The query
			// author never wrote it, so codegen makes it a required
			// argument instead of a params-struct field: a caller that
			// omits it fails to compile rather than sending the zero
			// value and silently reading an unscoped row set.
			woven.Params[p.ParamName] = &template.Param{
				Name: p.ParamName, GuardBit: -1, Policy: p.Name,
			}
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
