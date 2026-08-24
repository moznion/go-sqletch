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

// infixOperandKeywords are word-form binary/prefix operators after which
// an OPERAND (not a clause keyword) is expected. They keep the
// operand-position tracker correct across `col LIKE x`, `col IS NULL`,
// `col IN (…)`, `col BETWEEN a AND b`, etc., so a keyword-named column
// used as their right operand (`name LIKE offset`) is not mistaken for a
// clause terminator. The set need not be exhaustive: an omission only
// leaves the pre-existing behavior (the RHS ident is treated as
// operand-complete), never a new terminator misread.
var infixOperandKeywords = map[string]bool{
	"IS": true, "IN": true, "LIKE": true, "ILIKE": true, "GLOB": true,
	"MATCH": true, "REGEXP": true, "RLIKE": true, "BETWEEN": true,
	"SIMILAR": true, "ESCAPE": true, "COLLATE": true, "ANY": true,
	"ALL": true, "SOME": true, "OVERLAPS": true, "DIV": true,
	"MOD": true, "XOR": true,
}

// afterOperand reports the operand-position state produced by consuming
// tok, given the current state. It returns true when the NEXT depth-0
// identifier is in operand position (a column/function name, never a
// clause keyword), and false when it is in operator position (where a
// bona-fide clause keyword such as GROUP/ORDER/OFFSET or a boolean
// AND/OR appears). A word matching a keyword is only ever a clause
// boundary in operator position; in operand position it is a column even
// if it spells `offset`/`group`/`window` (a reserved word after `.`, or
// a non-reserved word bare, is a legal column) — misreading it ends the
// scan early and hides a following OR, leaking the tenant scope
// (audit-15/16). Callers pass the identifier's uppercased text for the
// KindIdent case.
func afterOperand(tok dialect.Token, up string, cur bool) bool {
	switch tok.Kind {
	case dialect.KindNumber, dialect.KindString, dialect.KindQuotedIdent,
		dialect.KindParamRef, dialect.KindPositionalParam, dialect.KindRParen:
		return false // an operand just completed
	case dialect.KindLParen, dialect.KindComma, dialect.KindOperator, dialect.KindCast:
		return true // an operand is expected next
	case dialect.KindOther:
		if tok.Text == "." {
			return true // a column name follows a qualifier dot
		}
		return cur
	case dialect.KindIdent:
		// AND/OR/NOT and the infix word-operators expect a right operand;
		// any other identifier IS an operand (column/function name).
		if up == "AND" || up == "OR" || up == "NOT" || infixOperandKeywords[up] {
			return true
		}
		return false
	}
	return cur
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
	// A keyword-spelled identifier is a clause boundary only in OPERATOR
	// position; in OPERAND position it is a column/function name (a
	// reserved word after `.`, or a non-reserved word bare, is a legal
	// column). expectOperand tracks that position within the WHERE
	// expression — misreading a keyword-column as GROUP/ORDER/OFFSET/…
	// ends the scan early and hides a following depth-0 OR, so the weave
	// is not wrapped and the OR escapes the tenant scope (audit-15/16).
	// prevDot handles the same hazard for the WHERE keyword itself
	// (`t.where`) before the expression is entered.
	prevDot := false
	expectOperand := false
	for _, it := range q.Items {
		s, isSkel := it.(*template.Skeleton)
		if !isSkel {
			prevDot = false
			if inWhere {
				col.flush()
				expectOperand = false
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
			dotted := prevDot
			prevDot = tok.Kind == dialect.KindOther && tok.Text == "."
			var up string
			if tok.Kind == dialect.KindIdent {
				up = strings.ToUpper(tok.Text)
			}
			if depth == 0 {
				if tok.Kind == dialect.KindSemicolon {
					col.flush()
					return col.cs
				}
				if !inWhere {
					if tok.Kind == dialect.KindIdent && up == "WHERE" && !dotted {
						inWhere = true
						expectOperand = true
					}
					continue
				}
				if tok.Kind == dialect.KindIdent && !expectOperand {
					if tailKeywords[up] {
						col.flush()
						return col.cs
					}
					if up == "AND" {
						col.flush()
						expectOperand = true
						continue
					}
					if up == "OR" {
						col.cs.hasOR = true
						col.flush()
						expectOperand = true
						continue
					}
				}
			}
			if !inWhere {
				continue
			}
			expectOperand = afterOperand(tok, up, expectOperand)
			col.content(tok, abs)
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
	// wrongJoin is set when an ON WAS located but it does not belong to
	// the join that null-extends this occurrence — gating it would not
	// remove the designated table's own rows (a FULL join preserves both
	// sides; a table on the preserved side of its own join is
	// null-extended farther out; or the reference's own join is an
	// ON-less CROSS join and the located ON belongs to a later enclosing
	// join). Weaving there is a silent tenant leak, so the weaver refuses
	// (SQLETCH125) and the enforcer treats the conjunct as absent
	// (SQLETCH124). The proven-equivalent inner-join crossings (scoping
	// the inner ON removes the rows before the group is null-extended) do
	// NOT set it.
	wrongJoin bool
	cs        clauseScan
}

// joinOn scans forward from the relation reference at template offset
// relOff to the first ON expression at the reference's paren depth,
// and collects it exactly like whereClause collects the WHERE clause.
// ownJoin is the join type that directly binds the reference as its
// RIGHT operand (dialect.RelRef.Join); it is authoritative only when
// no join keyword is crossed before the ON (the reference is a right
// operand). When a join keyword IS crossed first the reference is a
// left operand and its introducing join is read from that first
// crossed keyword instead — ownJoin is unreliable there (the frontend
// inherits it from an enclosing level).
//
// For a LEFT JOIN's right operand that is the join's own ON; for a
// RIGHT JOIN's left operand the scan crosses the join keywords and the
// joined relation to the same clause; and for a relation nested in an
// inner join group under an outer join it finds the inner ON — scoping
// there filters the designated rows before the group is null-extended,
// which is equivalent. USING and NATURAL quals terminate the search
// (nothing to extend). The expression ends at a depth-0
// join/tail/WHERE keyword, a depth-0 comma, a closing parenthesis
// beyond the reference's depth, a semicolon, any construct item, or
// the end of the statement.
//
// Soundness (D2a): weaving into the located ON removes the designated
// table's own rows ONLY when that ON belongs to the join that
// null-extends this occurrence. The scan records the introducing join
// so finish() can flag wrongJoin (a FULL join preserves both sides; a
// table on the preserved side of its own outer join is null-extended
// farther out) — for those the weaver refuses rather than weave a
// leaking conjunct. It also flags wrongJoin when the reference's own
// introducing join is an ON-less CROSS join: the forward scan then
// reaches a LATER enclosing join's ON (a SECOND join-introducing keyword
// is crossed before the ON), which does not gate the reference's rows.
func joinOn(profile dialect.LexerProfile, q *template.QueryTemplate, relOff int, ownJoin dialect.JoinType) joinOnResult {
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
	// crossedJoin records the first join keyword crossed before the ON
	// (the reference is then a left operand and this is its introducing
	// join); crossedJoinKw distinguishes "not yet crossed" from a value.
	crossedJoinKw := false
	crossedJoin := dialect.JoinInner
	// firstJoinAnchored becomes true once the reference's own introducing
	// join is complete — its JOIN/STRAIGHT_JOIN anchor token has been
	// crossed (the modifier keywords LEFT/RIGHT/FULL/INNER/CROSS precede
	// that anchor). secondJoin then records that ANOTHER join-introducing
	// keyword was crossed before reaching the ON: the reference's own
	// join was therefore ON-less (a CROSS join, which has no ON) and the
	// located ON belongs to a later, enclosing join whose preserve/
	// null-extend relationship to the reference is not modeled. Weaving a
	// scoping conjunct into that mis-attributed ON gates nothing and
	// leaks the designated table's own rows, so it is a wrongJoin.
	firstJoinAnchored := false
	secondJoin := false
	// naturalRight reports that the reference is the RIGHT operand of a
	// NATURAL join. Such a join owns no ON expression, but the PG frontend
	// folds NATURAL to JoinInner (mapJoinType), erasing that fact from
	// ownJoin — so an ON reached without crossing a join keyword would be
	// mis-attributed to the reference's ON-less NATURAL join and gate
	// nothing (audit-12 H4). The introducing NATURAL keyword precedes the
	// reference, so it is detected by a backward-aware lexical scan.
	naturalRight := naturalIntroducesRight(profile, q, relOff)
	// prevDot: was the previous non-trivia token a `.`? An identifier in
	// indirection position (the column of `table.column`) is never a
	// clause keyword — used before the ON is entered (a dotted `t.on`).
	// expectOperand: once inside the ON expression, a keyword-spelled
	// identifier is a boundary only in OPERATOR position — see whereClause
	// and afterOperand for why a keyword-column (`o.group`, bare `window`)
	// in operand position must not end the ON and hide a following OR.
	prevDot := false
	expectOperand := false
	finish := func() joinOnResult {
		col.flush()
		res.cs = col.cs
		res.found = inOn
		res.noOn = !inOn
		if inOn {
			// A located ON belongs to the reference's own introducing join
			// only when that join can own one. For a RIGHT operand
			// (!crossedJoinKw) whose own join is an ON-less CROSS (onGates
			// refuses JoinCross) or NATURAL (naturalRight) join, the ON was
			// reached from a later/enclosing join and cannot be proven to
			// gate this occurrence's rows — a wrongJoin (audit-12 H4).
			res.wrongJoin = secondJoin ||
				!onGates(ownJoin, crossedJoinKw, crossedJoin) ||
				(!crossedJoinKw && naturalRight)
		}
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
			dotted := false
			if tok.Kind != dialect.KindWhitespace && tok.Kind != dialect.KindLineComment && tok.Kind != dialect.KindBlockComment {
				dotted = prevDot
				prevDot = tok.Kind == dialect.KindOther && tok.Text == "."
			}
			var up string
			if tok.Kind == dialect.KindIdent {
				up = strings.ToUpper(tok.Text)
			}
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
				if depth == 0 && !inOn && !dotted {
					switch {
					case up == "ON":
						inOn = true
						expectOperand = true
						continue
					case up == "USING" || up == "NATURAL":
						return finish()
					case tailKeywords[up] || up == "WHERE" || up == "SET":
						return finish()
					case joinKeywords[up]:
						// The reference is a left operand: its ON lies
						// past the join keywords and the joined
						// relation. The FIRST such keyword names its
						// introducing join (LEFT/RIGHT/FULL are outer;
						// everything else in joinKeywords is inner/cross).
						switch {
						case !crossedJoinKw:
							crossedJoinKw = true
							crossedJoin = joinKindOf(up)
							if up == "JOIN" || up == "STRAIGHT_JOIN" {
								firstJoinAnchored = true
							}
						case !firstJoinAnchored:
							// Still inside the first join's keyword group
							// (e.g. the JOIN of a `CROSS JOIN`, or a `LEFT
							// OUTER JOIN`): the anchor completes it.
							if up == "JOIN" || up == "STRAIGHT_JOIN" {
								firstJoinAnchored = true
							}
						default:
							// The first join was already complete and its
							// relation crossed: this keyword introduces a
							// SECOND join whose ON we are about to locate. The
							// reference's own join was ON-less, so that ON
							// cannot scope the reference's rows.
							secondJoin = true
						}
						continue
					}
				}
				// Inside the ON expression, classify a keyword as a
				// boundary only in OPERATOR position; in OPERAND position it
				// is a column even when spelled `group`/`window`/… — see
				// afterOperand and audit-15/16.
				if depth == 0 && inOn && !expectOperand {
					switch {
					case joinKeywords[up] || tailKeywords[up] || up == "WHERE" || up == "SET":
						// SET ends a MySQL multi-table UPDATE's join list
						// (the ON is followed by SET, not WHERE). Without
						// this the scan swallows the SET assignments as ON
						// content and splices the conjunct into the assigned
						// value — gating nothing, a silent leak (audit-14).
						return finish()
					case up == "AND":
						col.flush()
						expectOperand = true
						continue
					case up == "OR":
						col.cs.hasOR = true
						col.flush()
						expectOperand = true
						continue
					}
				}
			}
			if inOn {
				expectOperand = afterOperand(tok, up, expectOperand)
				col.content(tok, abs)
			}
		}
	}
	return finish()
}

// joinKindOf maps a crossed join keyword to its dialect.JoinType. Only
// the outer keywords need to be distinguished; every other join keyword
// (JOIN/INNER/CROSS/STRAIGHT_JOIN) is an inner/cross crossing, which the
// gating decision treats as proven-equivalent. NATURAL never reaches
// here — it terminates the scan (no ON to extend).
func joinKindOf(up string) dialect.JoinType {
	switch up {
	case "LEFT":
		return dialect.JoinLeft
	case "RIGHT":
		return dialect.JoinRight
	case "FULL":
		return dialect.JoinFull
	default:
		return dialect.JoinInner
	}
}

// onGates reports whether weaving into the located ON removes the
// designated table's own rows — the D2a soundness condition. The
// located ON belongs to the occurrence's introducing join: when a join
// keyword was crossed the reference is that join's LEFT operand
// (introducing = crossedJoin), otherwise it is ownJoin's RIGHT operand.
//
//   - Inner/cross joins always gate: scoping the inner ON physically
//     removes the rows before any enclosing outer join null-extends the
//     group (proven-equivalent to a WHERE conjunct on the inner result).
//   - A LEFT join null-extends its RIGHT operand only; a RIGHT join its
//     LEFT operand only. Gating the ON there removes the unmatched
//     designated rows (they are never preserved on that side).
//   - A FULL join preserves BOTH sides, so its ON gates neither; a table
//     on the preserved side of any outer join is null-extended farther
//     out and its own ON does not gate it. Those are the leak cases.
//   - A CROSS join owns no ON, so for a RIGHT operand of one a located ON
//     always belongs to a later/enclosing join and cannot be proven to
//     gate the reference's rows — refuse (audit-12 H4). (A LEFT-operand
//     crossing of a CROSS keyword maps to JoinInner via joinKindOf, so
//     the crossedJoin path never actually sees JoinCross.)
func onGates(ownJoin dialect.JoinType, crossedJoinKw bool, crossedJoin dialect.JoinType) bool {
	if crossedJoinKw {
		// Left operand of crossedJoin.
		switch crossedJoin {
		case dialect.JoinInner, dialect.JoinCross:
			return true
		case dialect.JoinRight:
			return true
		default: // JoinLeft, JoinFull, and anything unexpected: refuse.
			return false
		}
	}
	// Right operand of ownJoin.
	switch ownJoin {
	case dialect.JoinInner:
		return true
	case dialect.JoinLeft:
		return true
	default: // JoinRight, JoinFull, JoinCross, and anything unexpected: refuse.
		return false
	}
}

// naturalIntroducesRight reports whether the relation at template offset
// relOff is the RIGHT operand of a NATURAL join — i.e. the contiguous
// run of join keywords that introduces the relation at its own paren
// depth contains NATURAL. A NATURAL join owns no ON expression, so any
// ON located after such a reference belongs to an enclosing join; the PG
// frontend folds NATURAL to JoinInner, so this lexical check is the only
// signal (audit-12 H4). The scan walks the item stream from the start,
// resetting the run on paren boundaries and on any non-join content
// token, and answers as soon as it reaches relOff.
func naturalIntroducesRight(profile dialect.LexerProfile, q *template.QueryTemplate, relOff int) bool {
	runHasNatural := false
	// prevDot: a dotted `a.natural` is a column, not the NATURAL keyword —
	// without this guard it would spuriously flag the run and refuse a
	// perfectly gate-able join (a loud false refusal; consistency with the
	// audit-15 whereClause/joinOn guard).
	prevDot := false
	for _, it := range q.Items {
		s, isSkel := it.(*template.Skeleton)
		if !isSkel {
			// A construct item is a content boundary: it cannot be part of a
			// join-keyword run, so reset.
			prevDot = false
			runHasNatural = false
			continue
		}
		src := []byte(s.Text)
		pos := 0
		for {
			tok, err := profile.NextToken(src, pos)
			if err != nil {
				return false
			}
			if tok.Kind == dialect.KindEOF {
				break
			}
			pos = tok.End
			abs := s.Span.Start + tok.Start
			switch tok.Kind {
			case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment:
				continue
			}
			if abs >= relOff {
				return runHasNatural
			}
			dotted := prevDot
			prevDot = tok.Kind == dialect.KindOther && tok.Text == "."
			switch tok.Kind {
			case dialect.KindLParen, dialect.KindRParen:
				// A paren boundary changes depth; the introducing run of the
				// operand at the new depth starts fresh.
				runHasNatural = false
			case dialect.KindIdent:
				up := strings.ToUpper(tok.Text)
				if !dotted && joinKeywords[up] {
					if up == "NATURAL" {
						runHasNatural = true
					}
				} else {
					// Any other identifier (a relation name, a dotted
					// keyword-column like `a.natural`, FROM, SELECT, …)
					// ends the current join-keyword run.
					runHasNatural = false
				}
			default:
				// Operators, commas, dots, params: not join keywords.
				runHasNatural = false
			}
		}
	}
	return runHasNatural
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
// normalized token sequence — bare, or wrapped in one pair of
// parentheses. The weaver splices every occurrence parenthesized
// (design 14 §11.6), so the parenthesized form is what its own output
// carries; the bare form keeps a hand-written unparenthesized copy of
// a single-conjunct predicate matching. A bare hand-written copy of a
// predicate that itself carries a depth-0 AND/OR can never equal one
// AND-split segment — the weaver then weaves anyway (doubling is
// harmless, skipping leaks).
func segsContain(profile dialect.LexerProfile, segs [][]string, conjunct string) bool {
	want := normalizedTokens(profile, conjunct)
	if want == nil {
		return false
	}
	wrapped := make([]string, 0, len(want)+2)
	wrapped = append(wrapped, "(")
	wrapped = append(wrapped, want...)
	wrapped = append(wrapped, ")")
	for _, seg := range segs {
		if tokensEqual(seg, want) || tokensEqual(seg, wrapped) {
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
// Skeleton carries a zero-width span at its insertion point and is
// marked Synth, so the renderer maps its emission as synthesized text
// anchored at the insertion offset — diagnostics that land in woven
// text attribute to the target query at that point instead of the
// unrelated template bytes after it.
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
				Text:  ins[k].text,
				Span:  diagnostics.Span{File: s.Span.File, Start: off, End: off},
				Synth: true,
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
