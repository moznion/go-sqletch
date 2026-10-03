// Package lint implements the opt-in performance lints SQLETCHL001–L006
// (docs/design/24-performance-lints.md).
//
// They are a separate axis from the structural rules in internal/rules:
// they run only when enabled (sqletch.yaml `lint`, or --lint), every
// finding is a WARNING, and the analysis is a LEXICAL WHITELIST over
// each verification rendering — it flags only the unambiguous forms
// and under-reports everything else. A missed lint costs nothing the
// database would not have charged anyway; a noisy one trains authors
// to sprinkle @nolint.
//
// Running over ast.Renderings (not the template) is what makes the
// lints guard-aware: every @if-present body is in the maximal
// rendering and every @choose case in its own, so a pattern anywhere
// reachable is seen, and the source map attributes it back to the
// template bytes. Findings anchored in synthesized text (policy-woven
// conjuncts) are dropped — the author did not write them.
package lint

import (
	"slices"
	"strconv"
	"strings"

	"github.com/moznion/go-sqletch/internal/ast"
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/template"
)

// Check runs the catalog-free performance lints — SQLETCHL001
// (function/cast on the column side), L002 (leading-wildcard LIKE), L003
// (:many without LIMIT) and L004 (OFFSET pagination) — over every
// verification rendering, applies the query's `-- @nolint` directives,
// and reports SQLETCHL006 for an @nolint of one of these codes that
// suppressed nothing.
func Check(profile dialect.LexerProfile, q *template.QueryTemplate, rs []ast.Rendering) []diagnostics.Diagnostic {
	c := newPerfCollector(q)
	for _, r := range rs {
		toks, ok := lexRendering(profile, r)
		if !ok {
			continue
		}
		ctx := predicateContexts(toks)
		lintPredicates(c, q, r, toks, ctx)
		lintTail(c, q, r, toks)
	}
	return c.finish([]diagnostics.Code{
		diagnostics.CodePerfWrappedColumn,
		diagnostics.CodePerfLeadingLike,
		diagnostics.CodePerfManyNoLimit,
		diagnostics.CodePerfOffsetPaging,
	})
}

// ---- collection, suppression, determinism ------------------------------

type perfKey struct {
	code  diagnostics.Code
	start int
	end   int
}

type perfCollector struct {
	q     *template.QueryTemplate
	seen  map[perfKey]bool
	diags []diagnostics.Diagnostic
}

func newPerfCollector(q *template.QueryTemplate) *perfCollector {
	return &perfCollector{q: q, seen: map[perfKey]bool{}}
}

// add records a finding once per (code, template span): the same
// predicate appears in several renderings.
func (c *perfCollector) add(d diagnostics.Diagnostic) {
	k := perfKey{d.Code, d.Span.Start, d.Span.End}
	if c.seen[k] {
		return
	}
	c.seen[k] = true
	c.diags = append(c.diags, d)
}

// finish applies the @nolint directives for the codes this pass owns,
// reports unused ones (SQLETCHL006), and returns the result in span
// order. Only owned codes are judged: an @nolint of a code another pass
// decides is that pass's business (SQLETCHL005 needs the catalog, which
// an offline pass may not have).
func (c *perfCollector) finish(owned []diagnostics.Code) []diagnostics.Diagnostic {
	fired := map[diagnostics.Code]bool{}
	for _, d := range c.diags {
		fired[d.Code] = true
	}
	allowed := map[diagnostics.Code]bool{}
	var out []diagnostics.Diagnostic
	for _, a := range c.q.NoLints {
		if !slices.Contains(owned, a.Code) {
			continue
		}
		allowed[a.Code] = true
		if fired[a.Code] {
			continue
		}
		out = append(out, diagnostics.Warnf(diagnostics.CodePerfNoLintUnused, a.Span,
			"@nolint %s suppresses nothing: the lint does not fire on this query, and a stale suppression would hide its next regression", a.Code).
			WithHint("remove %s from the @nolint directive", a.Code))
	}
	for _, d := range c.diags {
		if !allowed[d.Code] {
			out = append(out, d)
		}
	}
	slices.SortStableFunc(out, func(a, b diagnostics.Diagnostic) int {
		if a.Span.Start != b.Span.Start {
			return a.Span.Start - b.Span.Start
		}
		if a.Span.End != b.Span.End {
			return a.Span.End - b.Span.End
		}
		return strings.Compare(string(a.Code), string(b.Code))
	})
	return out
}

// ---- rendering tokens ----------------------------------------------------

// ptok is one significant (non-trivia) token of a rendering.
type ptok struct {
	dialect.Token
	// depth is the parenthesis depth the token sits at; a '(' and its
	// matching ')' sit at the OUTER depth, their contents one deeper.
	depth int
	// upper is the upper-cased text of an identifier ("" otherwise).
	upper string
	// param is the template parameter a placeholder binds ("" for
	// every other token).
	param string
}

func (t ptok) isIdent(words ...string) bool {
	return t.Kind == dialect.KindIdent && slices.Contains(words, t.upper)
}

// lexRendering tokenizes a rendering, dropping trivia and resolving
// each placeholder to its template parameter. ok is false when the
// rendering does not lex or its placeholders disagree with ParamsSeq
// (the lints then skip it rather than guess).
func lexRendering(profile dialect.LexerProfile, r ast.Rendering) ([]ptok, bool) {
	src := []byte(r.SQL)
	var out []ptok
	depth, question := 0, 0
	for pos := 0; pos < len(src); {
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
		pt := ptok{Token: tok, depth: depth}
		switch tok.Kind {
		case dialect.KindIdent:
			pt.upper = strings.ToUpper(tok.Text)
		case dialect.KindLParen:
			depth++
		case dialect.KindRParen:
			if depth > 0 {
				depth--
			}
			pt.depth = depth
		case dialect.KindPositionalParam:
			idx := -1
			if strings.HasPrefix(tok.Text, "$") {
				if n, err := strconv.Atoi(tok.Text[1:]); err == nil {
					idx = n - 1
				}
			} else {
				idx = question
				question++
			}
			if idx < 0 || idx >= len(r.ParamsSeq) {
				return nil, false
			}
			pt.param = r.ParamsSeq[idx]
		}
		out = append(out, pt)
	}
	return out, true
}

// templateSpan maps a run of rendering tokens back to one template
// span. A placeholder maps to its `:name`; any other synthesized token
// (policy-woven text, construct emissions) makes the run unattributable
// and ok is false.
func templateSpan(q *template.QueryTemplate, r ast.Rendering, toks []ptok) (diagnostics.Span, bool) {
	if len(toks) == 0 {
		return diagnostics.Span{}, false
	}
	start, end := -1, -1
	for _, t := range toks {
		s, synth := r.Map.ToTemplate(t.Start)
		var e int
		if synth {
			if t.param == "" {
				return diagnostics.Span{}, false
			}
			e = s + 1 + len(t.param)
		} else {
			last, synthEnd := r.Map.ToTemplate(t.End - 1)
			if synthEnd {
				return diagnostics.Span{}, false
			}
			e = last + 1
		}
		if start < 0 || s < start {
			start = s
		}
		if e > end {
			end = e
		}
	}
	return diagnostics.Span{File: q.HeaderSpan.File, Start: start, End: end}, true
}

// ---- predicate positions -------------------------------------------------

// predCtx says, per token, whether it sits directly in a boolean
// predicate context (a WHERE / JOIN ON clause or a parenthesized
// boolean group inside one, not inside a CASE, function argument list,
// or operand subquery), and whether that context is the TOP-level
// statement's own (not a subquery's).
type predCtx struct {
	pred []bool
	top  []bool
}

// clauseEnders end a WHERE/ON predicate at the depth they appear.
var clauseEnders = []string{
	"GROUP", "ORDER", "HAVING", "LIMIT", "OFFSET", "FETCH", "WINDOW", "RETURNING",
	"UNION", "INTERSECT", "EXCEPT", "JOIN", "INNER", "LEFT", "RIGHT", "FULL", "CROSS",
	"NATURAL", "STRAIGHT_JOIN", "FOR", "LOCK", "DO", "USING", "SELECT", "FROM", "SET",
	"VALUES", "QUALIFY",
}

// boolOpeners are the tokens after which a '(' opens a boolean group.
var boolOpeners = []string{"WHERE", "ON", "AND", "OR", "NOT"}

func predicateContexts(toks []ptok) predCtx {
	pc := predCtx{pred: make([]bool, len(toks)), top: make([]bool, len(toks))}
	var active, top, filter []bool
	var cases []int
	grow := func(d int) {
		for len(active) <= d {
			active = append(active, false)
			top = append(top, false)
			filter = append(filter, false)
			cases = append(cases, 0)
		}
	}
	for i, t := range toks {
		d := t.depth
		grow(d + 1)
		switch {
		case t.Kind == dialect.KindLParen:
			opener := i > 0 && (toks[i-1].isIdent(boolOpeners...) || toks[i-1].Kind == dialect.KindLParen)
			inner := active[d] && cases[d] == 0 && opener
			active[d+1] = inner
			top[d+1] = inner && top[d]
			cases[d+1] = 0
			// An aggregate's FILTER (WHERE …) filters rows already
			// fetched — like HAVING, no index can serve it.
			filter[d+1] = i > 0 && toks[i-1].isIdent("FILTER")
		case t.isIdent("WHERE"):
			if filter[d] {
				break
			}
			active[d], top[d] = true, d == 0
		case t.isIdent("ON"):
			// ON CONFLICT / ON DUPLICATE KEY open no predicate.
			next := i+1 < len(toks) && toks[i+1].isIdent("CONFLICT", "DUPLICATE")
			active[d], top[d] = !next, !next && d == 0
		case t.isIdent(clauseEnders...), t.Kind == dialect.KindComma, t.Kind == dialect.KindSemicolon:
			active[d], top[d] = false, false
		case t.isIdent("CASE"):
			cases[d]++
		case t.isIdent("END"):
			if cases[d] > 0 {
				cases[d]--
			}
		}
		pc.pred[i] = active[d] && cases[d] == 0
		pc.top[i] = pc.pred[i] && top[d]
	}
	return pc
}

var comparisonOps = []string{"=", "<>", "!=", "<", ">", "<=", ">="}

// comparisonAt classifies toks[i] as a comparison operator: "op" for
// the symbolic ones, or "LIKE"/"ILIKE"/"IN"/"BETWEEN" (negated forms
// are not comparisons an index serves, and return "").
func comparisonAt(toks []ptok, i int) string {
	t := toks[i]
	if t.Kind == dialect.KindOperator && slices.Contains(comparisonOps, t.Text) {
		return "op"
	}
	if !t.isIdent("LIKE", "ILIKE", "IN", "BETWEEN") {
		return ""
	}
	if i > 0 && toks[i-1].isIdent("NOT") {
		return ""
	}
	if t.upper == "IN" && (i+1 >= len(toks) || toks[i+1].Kind != dialect.KindLParen) {
		return ""
	}
	return t.upper
}

// operandBoundaries end an operand at its own depth.
var operandBoundaries = []string{
	"AND", "OR", "NOT", "WHERE", "ON", "WHEN", "THEN", "ELSE", "END", "CASE",
	"IS", "ESCAPE", "COLLATE", "LIKE", "ILIKE", "IN", "BETWEEN",
}

func isOperandBoundary(t ptok) bool {
	switch t.Kind {
	case dialect.KindComma, dialect.KindSemicolon:
		return true
	case dialect.KindOperator:
		return slices.Contains(comparisonOps, t.Text)
	case dialect.KindIdent:
		return slices.Contains(operandBoundaries, t.upper) || slices.Contains(clauseEnders, t.upper)
	}
	return false
}

// operands returns the left and right operand runs of the comparison
// at toks[i] (depth d). Deeper tokens belong to the operand; at depth d
// a boundary token ends it, and an enclosing parenthesis (which sits at
// a shallower depth) ends it too.
func operands(toks []ptok, i int) (left, right []ptok) {
	d := toks[i].depth
	j := i - 1
	for j >= 0 && toks[j].depth >= d && (toks[j].depth > d || !isOperandBoundary(toks[j])) {
		j--
	}
	k := i + 1
	for k < len(toks) && toks[k].depth >= d && (toks[k].depth > d || !isOperandBoundary(toks[k])) {
		k++
	}
	return toks[j+1 : i], toks[i+1 : k]
}

// ---- operand shapes ------------------------------------------------------

// endsValue reports whether t can end an INTERVAL's value operand.
func endsValue(t ptok) bool {
	switch t.Kind {
	case dialect.KindNumber, dialect.KindString, dialect.KindRParen:
		return true
	}
	return t.param != ""
}

// typeNameContinuations are the words that continue a type name after
// its first word (`double precision`, `character varying`,
// `timestamp with time zone`, `interval day to second`).
var typeNameContinuations = []string{
	"PRECISION", "VARYING", "CHARACTER", "CHAR", "WITH", "WITHOUT", "TIME", "ZONE",
	"YEAR", "MONTH", "DAY", "HOUR", "MINUTE", "SECOND", "TO", "UNSIGNED", "SIGNED",
}

// literalWords are identifiers that denote values, never columns.
var literalWords = []string{
	"NULL", "TRUE", "FALSE", "UNKNOWN", "DEFAULT", "CURRENT_DATE", "CURRENT_TIME",
	"CURRENT_TIMESTAMP", "LOCALTIME", "LOCALTIMESTAMP", "CURRENT_USER", "SESSION_USER",
	"INTERVAL", "ARRAY",
}

// intervalUnits may follow an INTERVAL value (MySQL `INTERVAL 1 DAY`).
var intervalUnits = []string{
	"MICROSECOND", "SECOND", "MINUTE", "HOUR", "DAY", "WEEK", "MONTH", "QUARTER", "YEAR",
}

// notFunctions are identifiers followed by '(' that are not scalar
// function calls over their argument.
var notFunctions = []string{
	"ANY", "ALL", "SOME", "EXISTS", "IN", "ARRAY", "ROW", "VALUES", "SELECT", "NOT", "CASE",
	"INTERVAL", "AND", "OR",
}

func isName(t ptok) bool {
	return t.Kind == dialect.KindIdent || t.Kind == dialect.KindQuotedIdent
}

func isDot(t ptok) bool { return t.Kind == dialect.KindOther && t.Text == "." }

// isColRef reports whether ops is exactly a (possibly qualified)
// column reference.
func isColRef(ops []ptok) bool {
	if len(ops) == 0 || len(ops)%2 == 0 {
		return false
	}
	for i, t := range ops {
		if i%2 == 1 {
			if !isDot(t) {
				return false
			}
			continue
		}
		if !isName(t) {
			return false
		}
		if t.Kind == dialect.KindIdent && slices.Contains(literalWords, t.upper) {
			return false
		}
	}
	return true
}

// matchingParen returns the index of the ')' closing the '(' at
// ops[open], or -1.
func matchingParen(ops []ptok, open int) int {
	for k := open + 1; k < len(ops); k++ {
		if ops[k].Kind == dialect.KindRParen && ops[k].depth == ops[open].depth {
			return k
		}
	}
	return -1
}

// isTypeSpec reports whether ops is a type name: identifiers, dots,
// array brackets, and a parenthesized numeric modifier list.
func isTypeSpec(ops []ptok) bool {
	if len(ops) == 0 || !isName(ops[0]) {
		return false
	}
	for _, t := range ops {
		switch t.Kind {
		case dialect.KindIdent, dialect.KindQuotedIdent, dialect.KindNumber,
			dialect.KindLParen, dialect.KindRParen, dialect.KindComma:
		case dialect.KindOther:
			if t.Text != "." && t.Text != "[" && t.Text != "]" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// splitArgs splits a call's argument tokens (between its parens) at
// the commas of their own depth.
func splitArgs(args []ptok) [][]ptok {
	if len(args) == 0 {
		return nil
	}
	d := args[0].depth
	var out [][]ptok
	start := 0
	for k, t := range args {
		if t.Kind == dialect.KindComma && t.depth == d {
			out = append(out, args[start:k])
			start = k + 1
		}
	}
	return append(out, args[start:])
}

// isWrappedColumn reports whether ops is a column hidden behind a
// function or cast: `f(…, col, …)` with the column the only non-constant
// argument, `col::type`, or `CAST(col AS type)`.
func isWrappedColumn(ops []ptok) bool {
	if len(ops) < 3 {
		return false
	}
	d := ops[0].depth
	// col::type (PostgreSQL)
	for k, t := range ops {
		if t.depth == d && t.Kind == dialect.KindCast {
			return isColRef(ops[:k]) && isTypeSpec(ops[k+1:])
		}
	}
	if !isName(ops[0]) {
		return false
	}
	// The callee: name ('.' name)* '('
	k := 1
	for k+1 < len(ops) && isDot(ops[k]) && isName(ops[k+1]) {
		k += 2
	}
	if k >= len(ops) || ops[k].Kind != dialect.KindLParen || matchingParen(ops, k) != len(ops)-1 {
		return false
	}
	if ops[0].Kind == dialect.KindIdent && k == 1 && slices.Contains(notFunctions, ops[0].upper) {
		return false
	}
	inner := ops[k+1 : len(ops)-1]
	if ops[0].isIdent("CAST") && k == 1 {
		for a, t := range inner {
			if t.isIdent("AS") && t.depth == d+1 {
				return isColRef(inner[:a]) && isTypeSpec(inner[a+1:])
			}
		}
		return false
	}
	cols := 0
	for _, arg := range splitArgs(inner) {
		switch {
		case isColRef(arg):
			cols++
		case !isColumnFree(arg):
			return false
		}
	}
	return cols == 1
}

// isColumnFree reports whether ops provably references no column:
// parameters, literals, operators, and calls over those.
func isColumnFree(ops []ptok) bool {
	if len(ops) == 0 {
		return false
	}
	// typeMode: 0 = not in a type name, 1 = expecting its first word
	// (after `::` or CAST's AS), 2 = after it, where only the words of
	// a multi-word type name continue it (`double precision`). Any
	// other identifier ends the type and is judged normally — so
	// `::timestamp AT TIME ZONE tz` still sees the column tz.
	// interval: an INTERVAL is pending its unit. A unit word counts
	// only right after the interval's value (`INTERVAL 1 DAY`); a later
	// `day` is a column again.
	typeMode, interval, skip := 0, false, 0
	for k, t := range ops {
		if skip > 0 {
			skip--
			continue
		}
		switch t.Kind {
		case dialect.KindIdent, dialect.KindQuotedIdent:
			next := ptok{}
			if k+1 < len(ops) {
				next = ops[k+1]
			}
			if typeMode == 2 && !t.isIdent(typeNameContinuations...) {
				typeMode = 0
			}
			switch {
			case typeMode == 1:
				typeMode = 2
			case typeMode == 2:
			case t.isIdent("AT") && k+2 < len(ops) && ops[k+1].isIdent("TIME") && ops[k+2].isIdent("ZONE"):
				skip = 2 // the AT TIME ZONE operator; its zone operand is judged next
			case t.isIdent("AS"):
				typeMode = 1
			case next.Kind == dialect.KindLParen && !t.isIdent("SELECT"):
				// a function name
			case next.Kind == dialect.KindString:
				// a typed literal: DATE '…', TIMESTAMP '…'
			case t.Kind == dialect.KindIdent && slices.Contains(literalWords, t.upper):
				if t.upper == "INTERVAL" {
					interval = true
				}
			case interval && t.Kind == dialect.KindIdent && slices.Contains(intervalUnits, t.upper) && k > 0 && endsValue(ops[k-1]):
				interval = false
			default:
				return false
			}
		case dialect.KindCast:
			typeMode = 1
			continue
		case dialect.KindString, dialect.KindNumber, dialect.KindPositionalParam,
			dialect.KindOperator, dialect.KindLParen, dialect.KindComma:
			typeMode = 0
		case dialect.KindRParen:
			typeMode = 0
		case dialect.KindOther:
			if t.Text != "[" && t.Text != "]" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// leadingWildcard reports whether a LIKE pattern operand provably
// begins with `%` or `_`: a string literal, the first operand of a
// concatenation, or CONCAT's first argument.
func leadingWildcard(ops []ptok) bool {
	if len(ops) == 0 {
		return false
	}
	first := ops[0]
	if first.isIdent("CONCAT") && len(ops) > 2 && ops[1].Kind == dialect.KindLParen {
		first = ops[2]
	}
	if first.Kind != dialect.KindString {
		return false
	}
	body, ok := quotedLiteralBody(first.Text)
	if !ok || body == "" {
		return false
	}
	return body[0] == '%' || body[0] == '_'
}

// quotedLiteralBody returns what follows the opening delimiter of a
// quote-delimited string literal: '…', "…" (MySQL), or an E/N prefix
// the lexer kept in the token (E'…'). Any other string token — dollar-quoted $$…$$ or
// $tag$…$tag$, x'…' / b'…' bit strings — is not judged (ok false):
// reading "the character after the first quote" inside it would
// misplace the pattern's start.
func quotedLiteralBody(text string) (string, bool) {
	switch {
	case strings.HasPrefix(text, "'"), strings.HasPrefix(text, `"`):
		return text[1:], true
	case len(text) >= 2 && strings.ContainsRune("EeNn", rune(text[0])) && text[1] == '\'':
		return text[2:], true
	}
	return "", false
}

// ---- SQLETCHL001 / L002 ----------------------------------------------------

func lintPredicates(c *perfCollector, q *template.QueryTemplate, r ast.Rendering, toks []ptok, pc predCtx) {
	for i := range toks {
		if !pc.pred[i] {
			continue
		}
		kind := comparisonAt(toks, i)
		if kind == "" {
			continue
		}
		left, right := operands(toks, i)
		switch {
		case isWrappedColumn(left) && isColumnFree(right):
			reportWrapped(c, q, r, left)
		case kind == "op" && isWrappedColumn(right) && isColumnFree(left):
			reportWrapped(c, q, r, right)
		}
		if (kind == "LIKE" || kind == "ILIKE") && isColRef(left) && leadingWildcard(right) {
			if span, ok := templateSpan(q, r, right); ok {
				c.add(diagnostics.Warnf(diagnostics.CodePerfLeadingLike, span,
					"this %s pattern starts with a wildcard: a B-tree index can only serve a known prefix, so every row of the column is scanned and matched", kind).
					WithHint("for a prefix search, compare a range (`col >= :lo AND col < :hi`), which a plain index serves on every dialect (anchoring the pattern helps only on some: see the performance-lints chapter); for a substring search, use a trigram / full-text index and `-- @nolint %s`", diagnostics.CodePerfLeadingLike))
			}
		}
	}
}

func reportWrapped(c *perfCollector, q *template.QueryTemplate, r ast.Rendering, ops []ptok) {
	span, ok := templateSpan(q, r, ops)
	if !ok {
		return
	}
	c.add(diagnostics.Warnf(diagnostics.CodePerfWrappedColumn, span,
		"the column is wrapped in a function or cast in this comparison: an index on the plain column cannot serve it, so the predicate is evaluated row by row").
		WithHint("compare the bare column and transform the parameter instead (e.g. `col >= :day_start AND col < :day_end`); if an expression index on exactly this expression exists, `-- @nolint %s`", diagnostics.CodePerfWrappedColumn))
}

// ---- SQLETCHL003 / L004 ----------------------------------------------------

// statementVerb is the leading keyword of the statement itself, past
// any WITH list (CTE bodies sit deeper, so the first depth-0 verb is
// the main statement's). Only this position decides "is DML": a
// depth-0 UPDATE in `FOR UPDATE` or a `replace(…)` call is not one.
func statementVerb(toks []ptok) string {
	for _, t := range toks {
		if t.depth == 0 && t.isIdent("SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE", "VALUES", "TABLE") {
			return t.upper
		}
	}
	return ""
}

func lintTail(c *perfCollector, q *template.QueryTemplate, r ast.Rendering, toks []ptok) {
	verb := statementVerb(toks)
	limited := false
	for i, t := range toks {
		if t.depth != 0 {
			continue
		}
		switch {
		case t.isIdent("LIMIT"):
			if i+1 >= len(toks) || !toks[i+1].isIdent("ALL") {
				limited = true
			}
		case t.isIdent("FETCH"):
			if i+1 < len(toks) && toks[i+1].isIdent("FIRST", "NEXT") {
				limited = true
			}
		}
	}
	if q.Annotation == template.AnnotationMany && verb == "SELECT" && !limited {
		c.add(diagnostics.Warnf(diagnostics.CodePerfManyNoLimit, q.HeaderSpan,
			"this :many query can run without a LIMIT: its result (and the slice the generated method builds) grows with the table").
			WithHint("add `LIMIT :limit` (keyset-paginate with an @if-present cursor), or `-- @nolint %s` for a result bounded by the data model", diagnostics.CodePerfManyNoLimit))
	}
	lintOffset(c, q, r, toks)
}

// tailEnders end an OFFSET/LIMIT operand at depth 0. Clause keywords
// are listed too: a real operand never contains one at its own depth,
// and an `offset` that is really a bare projection alias (MySQL/SQLite,
// `SELECT id offset FROM t`) then has an empty operand instead of
// swallowing the rest of the statement.
var tailEnders = []string{
	"ROW", "ROWS", "FETCH", "LIMIT", "OFFSET", "FOR", "LOCK", "UNION", "INTERSECT", "EXCEPT",
	"FROM", "WHERE", "GROUP", "HAVING", "ORDER", "WINDOW", "RETURNING", "INTO", "JOIN",
}

// nonClausePredecessors are tokens after which `offset` is a column
// or table name, not the clause (it is non-reserved on MySQL/SQLite):
// every keyword that takes an operand or a relation name. None of them
// can end an expression, so none can precede a real OFFSET clause —
// over-listing only ever costs a missed warning, never a wrong one
// (design 24 §6).
var nonClausePredecessors = []string{
	"SELECT", "BY", "WHERE", "AND", "OR", "NOT", "ON", "AS", "DISTINCT", "SET", "HAVING", "WHEN", "THEN", "ELSE",
	"BETWEEN", "LIKE", "ILIKE", "GLOB", "REGEXP", "RLIKE", "MATCH", "ESCAPE", "IS", "IN", "CASE",
	"INTERVAL", "BINARY", "DIV", "MOD", "XOR", "ALL", "ANY", "SOME",
	"FROM", "JOIN", "UPDATE", "INTO", "USING", "RETURNING",
	// MySQL SELECT/DML modifiers sit between the verb and the first
	// operand.
	"DISTINCTROW", "HIGH_PRIORITY", "LOW_PRIORITY", "STRAIGHT_JOIN", "SQL_SMALL_RESULT", "SQL_BIG_RESULT",
	"SQL_BUFFER_RESULT", "SQL_CACHE", "SQL_NO_CACHE", "SQL_CALC_FOUND_ROWS", "DELAYED", "IGNORE", "QUICK",
}

func tailOperand(toks []ptok, from int) []ptok {
	k := from
	for k < len(toks) {
		t := toks[k]
		if t.depth == 0 && (t.Kind == dialect.KindComma || t.Kind == dialect.KindSemicolon || t.isIdent(tailEnders...)) {
			break
		}
		k++
	}
	return toks[from:k]
}

// isPagingOffset reports whether an OFFSET (or MySQL/SQLite
// `LIMIT off, n`) operand is a caller-driven paging offset: it is built
// only from placeholders, numbers, arithmetic, parentheses and casts
// (`(:page - 1) * :size`, `:o::int`), and holds at least one
// placeholder. Any other shape — a constant, a subquery, or tokens that
// show `offset` was never the clause (a table alias before LEFT JOIN, …)
// — is not reported: a wrong SQLETCHL004 could only be silenced by an
// @nolint that would also hide real paging (design 24 §6).
func isPagingOffset(ops []ptok) bool {
	param, inType := false, false
	for _, t := range ops {
		switch {
		case t.param != "":
			param, inType = true, false
		case t.Kind == dialect.KindCast:
			inType = true
		case inType && t.Kind == dialect.KindIdent:
			// the cast's type name (possibly multi-word)
		case t.Kind == dialect.KindNumber, t.Kind == dialect.KindOperator,
			t.Kind == dialect.KindLParen, t.Kind == dialect.KindRParen:
			inType = false
		default:
			return false
		}
	}
	return param
}

func lintOffset(c *perfCollector, q *template.QueryTemplate, r ast.Rendering, toks []ptok) {
	seenLimit := false
	for i, t := range toks {
		if t.depth != 0 {
			continue
		}
		switch {
		case t.isIdent("LIMIT"):
			seenLimit = true
			first := tailOperand(toks, i+1)
			end := i + 1 + len(first)
			if end < len(toks) && toks[end].Kind == dialect.KindComma && isPagingOffset(first) {
				// MySQL/SQLite `LIMIT offset, count`.
				reportOffset(c, q, r, first)
			}
		case t.isIdent("OFFSET"):
			if i+1 >= len(toks) {
				continue
			}
			prev := ptok{}
			if i > 0 {
				prev = toks[i-1]
			}
			if !seenLimit && (prev.isIdent(nonClausePredecessors...) || prev.Kind == dialect.KindComma ||
				prev.Kind == dialect.KindLParen || prev.Kind == dialect.KindOperator || isDot(prev) || i == 0) {
				continue
			}
			next := toks[i+1]
			if next.Kind == dialect.KindOperator && slices.Contains(comparisonOps, next.Text) || isDot(next) {
				continue
			}
			ops := tailOperand(toks, i+1)
			if !isPagingOffset(ops) {
				continue
			}
			reportOffset(c, q, r, toks[i:i+1+len(ops)])
		}
	}
}

func reportOffset(c *perfCollector, q *template.QueryTemplate, r ast.Rendering, ops []ptok) {
	span, ok := templateSpan(q, r, ops)
	if !ok {
		return
	}
	c.add(diagnostics.Warnf(diagnostics.CodePerfOffsetPaging, span,
		"OFFSET pagination: the database must produce and discard every skipped row, so page N costs O(N × page size) and deep pages degrade linearly").
		WithHint("paginate by key instead: `@if-present(after_id) AND id > :after_id @endif … ORDER BY id LIMIT :limit`; or `-- @nolint %s` for a bounded page count", diagnostics.CodePerfOffsetPaging))
}
