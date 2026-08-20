package template

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
)

// Scanner turns template source into QueryFiles. It is construct-
// generic; dialect lexical structure comes from the LexerProfile.
type Scanner struct {
	profile dialect.LexerProfile
}

func NewScanner(profile dialect.LexerProfile) *Scanner {
	return &Scanner{profile: profile}
}

var headerRe = regexp.MustCompile(`^--\s*name:\s*([A-Za-z][A-Za-z0-9_]*)\s+:(one|maybe-one|many|exec|execrows)\s*$`)
var (
	paramHintRe = regexp.MustCompile(`^--\s*@param\s+([a-z][a-z0-9_]*)\s*:\s*(.+?)\s*$`)
	colHintRe   = regexp.MustCompile(`^--\s*@column\s+([a-z][a-z0-9_]*)\s*:\s*(.+?)\s*$`)
	// optOutRe matches any @policy-optout-shaped comment; optOutFormRe
	// is the valid form — the split lets malformed annotations get a
	// diagnostic instead of silently staying skeleton text.
	optOutRe     = regexp.MustCompile(`^--\s*@policy-optout\b`)
	optOutFormRe = regexp.MustCompile(`^--\s*@policy-optout:\s*([a-z][a-z0-9_]*)\s+\((.+)\)\s*$`)
)
var snakeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// clause context, tracked at paren depth 0.
type clauseCtx int

const (
	ctxNone clauseCtx = iota
	ctxProjection
	ctxFrom
	ctxWhere
	ctxGroupBy
	ctxHaving
	ctxOrderBy
	ctxTail
	ctxUpdateTarget
	ctxSet
	ctxInsertTarget
	ctxInsertColumns  // inside the INSERT column-list parens (depth 1)
	ctxInsertAfterCol // after the column list, before VALUES
	ctxValues         // between VALUES rows (depth 0)
	ctxInsertValueRow // inside one VALUES row's parens (depth 1)
	ctxInsertTail     // after a closed VALUES list (ON CONFLICT, …): no more rows
	ctxReturning
)

func (c clauseCtx) String() string {
	switch c {
	case ctxProjection:
		return "SELECT list"
	case ctxFrom:
		return "FROM"
	case ctxWhere:
		return "WHERE"
	case ctxGroupBy:
		return "GROUP BY"
	case ctxHaving:
		return "HAVING"
	case ctxOrderBy:
		return "ORDER BY"
	case ctxTail:
		return "LIMIT/OFFSET/locking clause"
	case ctxSet:
		return "SET"
	case ctxInsertColumns:
		return "INSERT column list"
	case ctxInsertValueRow:
		return "VALUES row"
	default:
		return "statement"
	}
}

// Structural limits of the shape key's encoding (runtime.ShapeKey).
// They are compiler limits, not policy: past them the encoding decays
// silently — a truncated @choose ordinal selects a different case's
// SQL, a truncated @order-by element sorts by a different column — so
// the scanner refuses the template rather than let an unverified shape
// reach a database.
// They are exported so the encoding's other end can pin them: see
// TestShapeKeyLimitsAgree in internal/codegen.
const (
	// MaxGuards: one bit per guard atom in ShapeKey.Guards (uint64).
	MaxGuards = 64
	// MaxOrderKeys: elements pack as key<<1|desc into a uint8, and both
	// shape.orderOptions and runtime.OrderSeq track used keys in a
	// bitmask. 64 keeps the packing well inside uint8 and mirrors
	// MaxGuards.
	MaxOrderKeys = 64
	// MaxChooseOrdinals: ShapeKey.Choices holds one uint8 ordinal per
	// @choose block, counting the @default body.
	MaxChooseOrdinals = 255
	// MaxParams: a bind plan addresses the params struct with
	// runtime.Bind.Idx, an int16. This one bounds the bind plan rather
	// than the shape key, but it is the same kind of limit and the same
	// consequence — codegen would narrow the index silently and bind
	// the wrong value.
	MaxParams = 32767
)

type fileScan struct {
	path    string
	src     []byte
	profile dialect.LexerProfile
	diags   []diagnostics.Diagnostic

	qb            *queryBuilder
	names         map[string]diagnostics.Span
	strayReported bool

	// pendingDirectives buffers directive comments (`-- @param`,
	// `-- @column`, `-- @policy-optout`) until their target query is
	// known. A directive attaches to a query only once a real SQL token
	// of that query is seen (in-body directives) or, in the gap before
	// the next `-- name:` header, to the FOLLOWING query — never the
	// previous one, whose queryBuilder is still current at that point.
	pendingDirectives []dialect.Token

	// Per-code emission cap. A pathological input (e.g. a file that is
	// nothing but `$1$1$1…` positional params) would otherwise emit one
	// diagnostic per token — millions of structs, gigabytes of memory —
	// before anything renders. errorf stops appending past the cap and
	// records the overflow so ScanFile can emit one summary per code.
	emitted   map[diagnostics.Code]int
	overflow  map[diagnostics.Code]int
	firstOver map[diagnostics.Code]diagnostics.Span
}

// maxDiagsPerCode bounds how many diagnostics of a single code one file
// emits before the rest collapse into a summary. Chosen far above any
// realistic hand-written template so ordinary output is untouched.
const maxDiagsPerCode = 200

type queryBuilder struct {
	q              *QueryTemplate
	skelStart      int // -1 = no open skeleton chunk
	depth          int
	ctx            clauseCtx
	statementEnded bool
	extraReported  bool
	// R7 bookkeeping: guarded INSERT items must sit at the tail of
	// their clause (after every unconditional item).
	colGuardSeen bool
	rowGuardSeen bool
	// prevKw is the previous top-level keyword seen by transition, used
	// to recognize the two-token `ON CONFLICT` sequence. afterOnConflict
	// records that the INSERT conflict clause has begun: a WHERE inside
	// it (partial-index predicate or DO UPDATE row filter) is not the
	// statement's row filter and must not become WhereKwEnd.
	prevKw          string
	afterOnConflict bool
}

func (s *Scanner) ScanFile(path string, src []byte) (*QueryFile, []diagnostics.Diagnostic) {
	return s.ScanFileFrom(path, src, 0)
}

// ScanFileFrom is ScanFile that begins scanning at byte offset start,
// while still treating src as the whole offset-preserving buffer: every
// emitted span and offset indexes src exactly as ScanFile would, so for
// any start whose skipped prefix src[:start] is scan-inert trivia the
// result is byte-identical to ScanFile(path, src).
//
// It exists for gosrc's Go-source views (docs/design/13 §3): a marked
// const's view is the whole file with everything but that one literal
// blanked to spaces/newlines. Scanning such a view from 0 re-lexes the
// blank prefix — and copies it whole as a single whitespace token's
// Text — once per const, which is O(consts × file size) quadratic CPU
// (and hangs the LSP on file-open through cli.scanSource). Starting at
// the literal's own offset skips that inert prefix without moving any
// span. start is clamped to [0, len(src)].
func (s *Scanner) ScanFileFrom(path string, src []byte, start int) (*QueryFile, []diagnostics.Diagnostic) {
	fs := &fileScan{path: path, src: src, profile: s.profile, names: map[string]diagnostics.Span{}}
	file := &QueryFile{Path: path}

	if start < 0 {
		start = 0
	}
	if start > len(src) {
		start = len(src)
	}
	pos := start
	// A leading UTF-8 BOM (EF BB BF) is editor byte-order noise, not
	// template content. Skip it as leading trivia WITHOUT rewriting the
	// buffer, so every span/offset (and the LSP's UTF-16 mapping) still
	// indexes the original bytes. Otherwise the BOM lexes as an
	// identifier and trips SQLETCH003 "statement without a query header".
	// Only meaningful at the true file start; a windowed start (start>0)
	// begins inside a template literal, past any BOM.
	if start == 0 && len(src) >= 3 && src[0] == 0xEF && src[1] == 0xBB && src[2] == 0xBF {
		pos = 3
	}
	for pos <= len(src) {
		if pos < len(src) && src[pos] == '@' {
			if name, nameEnd := matchConstruct(src, pos); name != "" {
				pos = fs.handleConstruct(name, pos, nameEnd)
				if fs.qb != nil && !fs.qb.statementEnded {
					fs.qb.q.StmtEnd = pos
				}
				continue
			}
		}
		tok, err := s.profile.NextToken(src, pos)
		if err != nil {
			le := err.(*dialect.LexError)
			fs.errorf(diagnostics.CodeConstructGrammar, fs.span(le.Pos, le.Pos+1), "%s", le.Msg)
			break
		}
		if tok.Kind == dialect.KindEOF {
			break
		}
		fs.handleToken(file, tok)
		pos = tok.End
	}
	// Trailing directives with no following query belong to the last one.
	if fs.qb != nil {
		fs.applyDirectives(fs.qb.q)
	}
	fs.finalize(file, len(src))
	fs.flushOverflow()
	diagnostics.Sort(fs.diags)
	return file, fs.diags
}

// span builds a file span, clamped to the source bounds. Several call
// sites point at "the character after X" (`fs.span(p, p+1)`) to mark an
// empty body; when X ends at EOF there is no such character and the
// naive span runs one past the end. Consumers index the source with
// these — the excerpt renderer, and the LSP's UTF-16 position
// conversion — so the clamp lives in the single constructor rather than
// at each call site, making the in-bounds invariant structural.
func (fs *fileScan) span(start, end int) diagnostics.Span {
	n := len(fs.src)
	if start > n {
		start = n
	}
	if start < 0 {
		start = 0
	}
	if end > n {
		end = n
	}
	if end < start {
		end = start
	}
	return diagnostics.Span{File: fs.path, Start: start, End: end}
}

func (fs *fileScan) errorf(code diagnostics.Code, span diagnostics.Span, format string, args ...any) {
	fs.emit(diagnostics.Errorf(code, span, format, args...))
}

// emit records a diagnostic, enforcing the per-code cap so that
// adversarial input producing O(input) same-code errors cannot build an
// unbounded slab of diagnostic structs. Every emission site — errorf
// and the direct-append callers that attach a hint — must go through
// here; the cap belongs to the code, not to a particular call shape.
func (fs *fileScan) emit(d diagnostics.Diagnostic) {
	if fs.emitted[d.Code] >= maxDiagsPerCode {
		if fs.overflow == nil {
			fs.overflow = map[diagnostics.Code]int{}
			fs.firstOver = map[diagnostics.Code]diagnostics.Span{}
		}
		if fs.overflow[d.Code] == 0 {
			fs.firstOver[d.Code] = d.Span
		}
		fs.overflow[d.Code]++
		return
	}
	if fs.emitted == nil {
		fs.emitted = map[diagnostics.Code]int{}
	}
	fs.emitted[d.Code]++
	fs.diags = append(fs.diags, d)
}

// flushOverflow appends one summary diagnostic per code whose emission
// hit maxDiagsPerCode, so a capped file still reports that the code
// occurred and how many times were suppressed. Codes are visited in
// sorted order for deterministic output.
func (fs *fileScan) flushOverflow() {
	if len(fs.overflow) == 0 {
		return
	}
	codes := make([]diagnostics.Code, 0, len(fs.overflow))
	for c := range fs.overflow {
		codes = append(codes, c)
	}
	slices.Sort(codes)
	for _, c := range codes {
		fs.diags = append(fs.diags, diagnostics.Errorf(c, fs.firstOver[c],
			"%d further %s diagnostics in this file were suppressed; fix the reported occurrences and re-run", fs.overflow[c], c))
	}
}

func (fs *fileScan) handleToken(file *QueryFile, tok dialect.Token) {
	if tok.Kind == dialect.KindLineComment {
		if m := headerRe.FindStringSubmatch(tok.Text); m != nil {
			fs.finalize(file, tok.Start)
			fs.startQuery(m[1], m[2], tok)
			// Directives seen in the gap since the previous query's last
			// real token belong to THIS new query, not the previous one.
			fs.applyDirectives(fs.qb.q)
			return
		}
		if isDirectiveComment(tok.Text) && fs.qb != nil {
			// Buffer, don't attach yet: the target query is settled once a
			// real SQL token or the next header arrives. (Directives before
			// the first header, fs.qb == nil, stay ignored as before.)
			fs.pendingDirectives = append(fs.pendingDirectives, tok)
			// fall through: the comment remains skeleton text.
		}
	}
	if fs.qb == nil {
		switch tok.Kind {
		case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment:
			return
		default:
			if !fs.strayReported {
				fs.errorf(diagnostics.CodeMissingHeader, fs.span(tok.Start, tok.End),
					"statement without a query header; every query starts with `-- name: QueryName :many` (or :one/:maybe-one/:exec/:execrows)")
				fs.strayReported = true
			}
			return
		}
	}
	// A real (non-trivia) SQL token of the current query claims any
	// buffered directives: they annotate THIS query, since its body
	// continues past them.
	switch tok.Kind {
	case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment:
	default:
		fs.applyDirectives(fs.qb.q)
	}
	fs.qb.feed(fs, tok)
}

// isDirectiveComment reports whether a line comment is one of the
// per-query directives (`-- @param`, `-- @column`, `-- @policy-optout`),
// including a malformed @policy-optout (which still targets a query, so
// its diagnostic must attach there too).
func isDirectiveComment(text string) bool {
	return paramHintRe.MatchString(text) ||
		colHintRe.MatchString(text) ||
		optOutRe.MatchString(text)
}

// applyDirectives attaches every buffered directive to q (in source
// order) and clears the buffer.
func (fs *fileScan) applyDirectives(q *QueryTemplate) {
	for _, tok := range fs.pendingDirectives {
		fs.applyDirective(q, tok)
	}
	fs.pendingDirectives = nil
}

// applyDirective records one buffered directive comment against q. The
// comment stays in the skeleton verbatim (handled separately); this only
// populates the parsed directive fields.
func (fs *fileScan) applyDirective(q *QueryTemplate, tok dialect.Token) {
	if m := paramHintRe.FindStringSubmatch(tok.Text); m != nil {
		if q.TypeHints == nil {
			q.TypeHints = map[string]TypeHint{}
		}
		q.TypeHints[m[1]] = TypeHint{SQLType: m[2], Span: fs.span(tok.Start, tok.End)}
		return
	}
	if m := colHintRe.FindStringSubmatch(tok.Text); m != nil {
		if q.ColumnHints == nil {
			q.ColumnHints = map[string]TypeHint{}
		}
		q.ColumnHints[m[1]] = TypeHint{SQLType: m[2], Span: fs.span(tok.Start, tok.End)}
		return
	}
	if optOutRe.MatchString(tok.Text) {
		if m := optOutFormRe.FindStringSubmatch(tok.Text); m != nil {
			q.PolicyOptOuts = append(q.PolicyOptOuts, PolicyOptOut{
				Policy: m[1], Reason: strings.TrimSpace(m[2]), Span: fs.span(tok.Start, tok.End),
			})
		} else {
			fs.errorf(diagnostics.CodeConstructGrammar, fs.span(tok.Start, tok.End),
				"malformed @policy-optout; the reason is mandatory: `-- @policy-optout: policy_name (reason)`")
		}
	}
}

func (fs *fileScan) startQuery(name, ann string, headerTok dialect.Token) {
	if prev, dup := fs.names[name]; dup {
		fs.errorf(diagnostics.CodeDuplicateQueryName, fs.span(headerTok.Start, headerTok.End),
			"duplicate query name %q (previously defined at offset %d)", name, prev.Start)
	}
	fs.names[name] = fs.span(headerTok.Start, headerTok.End)
	annotation := map[string]Annotation{
		"one": AnnotationOne, "maybe-one": AnnotationMaybeOne, "many": AnnotationMany,
		"exec": AnnotationExec, "execrows": AnnotationExecRows,
	}[ann]
	fs.qb = &queryBuilder{
		q: &QueryTemplate{
			Name:       name,
			Annotation: annotation,
			HeaderSpan: fs.span(headerTok.Start, headerTok.End),
			Params:     map[string]*Param{},
			WhereKwEnd: -1,
			TailStart:  -1,
			StmtEnd:    -1,
		},
		skelStart: headerTok.End,
		ctx:       ctxNone,
	}
	fs.strayReported = false
}

func (qb *queryBuilder) feed(fs *fileScan, tok dialect.Token) {
	if qb.statementEnded {
		switch tok.Kind {
		case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment:
			return
		default:
			if !qb.extraReported {
				fs.errorf(diagnostics.CodeMultipleStatements, fs.span(tok.Start, tok.End),
					"only one statement per query header; start a new `-- name:` header for the next query")
				qb.extraReported = true
			}
			return
		}
	}
	switch tok.Kind {
	case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment, dialect.KindSemicolon:
	default:
		// The weaver's fallback insertion point: end of the last
		// statement token, semicolon excluded.
		qb.q.StmtEnd = tok.End
	}
	switch tok.Kind {
	case dialect.KindLParen:
		if qb.depth == 0 {
			switch qb.ctx {
			case ctxInsertTarget:
				qb.ctx = ctxInsertColumns
				qb.colGuardSeen = false
			case ctxValues:
				qb.ctx = ctxInsertValueRow
				qb.rowGuardSeen = false
				qb.q.InsertValGuards = append(qb.q.InsertValGuards, nil)
			}
		}
		qb.depth++
	case dialect.KindRParen:
		if qb.depth > 0 {
			qb.depth--
		}
		if qb.depth == 0 {
			switch qb.ctx {
			case ctxInsertColumns:
				qb.ctx = ctxInsertAfterCol
			case ctxInsertValueRow:
				qb.ctx = ctxValues
			}
		}
	case dialect.KindComma:
		// R7 tail rule: an unconditional item after a guarded one would
		// break positional alignment across shapes.
		if qb.depth == 1 &&
			((qb.ctx == ctxInsertColumns && qb.colGuardSeen) ||
				(qb.ctx == ctxInsertValueRow && qb.rowGuardSeen)) {
			fs.errorf(diagnostics.CodePairedGuards, fs.span(tok.Start, tok.End),
				"unconditional item after optional items in the %s; keep optional column/value pairs at the end (R7)", qb.ctx)
		}
	case dialect.KindSemicolon:
		if qb.depth == 0 {
			qb.statementEnded = true
		}
	case dialect.KindParamRef:
		fs.recordParam(qb.q, tok.Text[1:], fs.span(tok.Start, tok.End), nil, false, false)
	case dialect.KindPositionalParam:
		fs.errorf(diagnostics.CodePositionalParam, fs.span(tok.Start, tok.End),
			"positional parameter %q is not allowed; use a named parameter (:name)", tok.Text)
	case dialect.KindIdent:
		if qb.depth == 0 {
			qb.transition(strings.ToUpper(tok.Text), tok)
		}
	}
}

func (qb *queryBuilder) transition(kw string, tok dialect.Token) {
	prevKw := qb.prevKw
	qb.prevKw = kw
	// A closed VALUES list ends at the first top-level keyword after it
	// (ON CONFLICT, RETURNING, …). Leaving ctxValues here stops the
	// conflict-target / partial-index parens from being read as another
	// VALUES row, which appended a phantom InsertValGuards entry and then
	// false-rejected legitimate upserts under R7 (SQLETCH119). Rows are
	// separated by commas, never keywords, so no real row is lost.
	if qb.ctx == ctxValues && kw != "VALUES" {
		qb.ctx = ctxInsertTail
	}
	// `ON CONFLICT` opens the INSERT conflict clause: a WHERE within it
	// (a partial-index predicate or the DO UPDATE row filter) is not the
	// statement's row filter, so it must never be recorded as WhereKwEnd.
	if prevKw == "ON" && kw == "CONFLICT" {
		qb.afterOnConflict = true
	}
	switch kw {
	case "SELECT":
		qb.ctx = ctxProjection
	case "FROM":
		qb.ctx = ctxFrom
	case "WHERE":
		qb.ctx = ctxWhere
		if qb.q.WhereKwEnd < 0 && !qb.afterOnConflict {
			qb.q.WhereKwEnd = tok.End
		}
	case "GROUP":
		qb.ctx = ctxGroupBy
		qb.markTail(tok.Start)
	case "HAVING":
		qb.ctx = ctxHaving
		qb.markTail(tok.Start)
	case "ORDER":
		qb.ctx = ctxOrderBy
		qb.markTail(tok.Start)
	case "LIMIT", "OFFSET", "FETCH", "FOR":
		qb.ctx = ctxTail
		qb.markTail(tok.Start)
	case "UPDATE":
		qb.ctx = ctxUpdateTarget
	case "SET":
		if qb.ctx == ctxUpdateTarget {
			qb.ctx = ctxSet
		}
	case "INSERT":
		qb.ctx = ctxInsertTarget
	case "VALUES":
		qb.ctx = ctxValues
	case "RETURNING":
		qb.ctx = ctxReturning
		qb.markTail(tok.Start)
	}
}

// markTail records where a synthesized WHERE clause would be inserted:
// the start of the first post-WHERE clause of a statement that has no
// WHERE of its own.
func (qb *queryBuilder) markTail(start int) {
	if qb.q.WhereKwEnd < 0 && qb.q.TailStart < 0 {
		qb.q.TailStart = start
	}
}

func (fs *fileScan) recordParam(q *QueryTemplate, name string, span diagnostics.Span, guards []GuardAtom, inCase, inFT bool) {
	if !snakeRe.MatchString(name) {
		fs.errorf(diagnostics.CodeBadIdentifier, span,
			"parameter name %q must be snake_case ([a-z][a-z0-9_]*)", name)
		return
	}
	p := q.Params[name]
	if p == nil {
		p = &Param{Name: name, GuardBit: -1}
		q.Params[name] = p
		q.ParamOrder = append(q.ParamOrder, name)
	}
	p.Occurrences = append(p.Occurrences, Occurrence{Span: span, Guards: guards, InChooseCase: inCase, InFilterTree: inFT})
}

// ---- construct handling ----------------------------------------------------

var constructVocab = map[string]bool{
	"if-present": true, "endif": true,
	"choose": true, "case": true, "default": true, "end": true,
	"when": true, "order-by": true, "key": true,
	"filter-tree": true, "predicate": true,
	"in": true,
}

// matchConstruct reports whether src[pos:] starts a template construct.
// Returns the construct name and the offset just past it.
func matchConstruct(src []byte, pos int) (string, int) {
	i := pos + 1
	for i < len(src) && ((src[i] >= 'a' && src[i] <= 'z') || src[i] == '-') {
		i++
	}
	name := string(src[pos+1 : i])
	if !constructVocab[name] {
		return "", pos
	}
	return name, i
}

func (fs *fileScan) handleConstruct(name string, pos, nameEnd int) int {
	if fs.qb == nil {
		if !fs.strayReported {
			fs.errorf(diagnostics.CodeMissingHeader, fs.span(pos, nameEnd),
				"construct before any query header")
			fs.strayReported = true
		}
		return nameEnd
	}
	if fs.qb.statementEnded {
		if !fs.qb.extraReported {
			fs.errorf(diagnostics.CodeMultipleStatements, fs.span(pos, nameEnd),
				"construct after the statement terminator")
			fs.qb.extraReported = true
		}
		return nameEnd
	}
	switch name {
	case "if-present":
		return fs.parseIfPresent(pos, nameEnd)
	case "when":
		return fs.parseWhen(pos, nameEnd)
	case "choose":
		return fs.parseChoose(pos, nameEnd)
	case "order-by":
		// The construct replaces the statement-level ORDER BY clause,
		// so it bounds the WHERE slot like the literal keyword would.
		fs.qb.markTail(pos)
		return fs.parseOrderBy(pos, nameEnd)
	case "filter-tree":
		return fs.parseFilterTree(pos, nameEnd)
	case "in":
		return fs.parseIn(pos, nameEnd)
	default:
		fs.errorf(diagnostics.CodeConstructGrammar, fs.span(pos, nameEnd),
			"unmatched @%s", name)
		return nameEnd
	}
}

// lexAt returns the token at pos, treating lex errors as EOF after
// reporting them.
func (fs *fileScan) lexAt(pos int) dialect.Token {
	tok, err := fs.profile.NextToken(fs.src, pos)
	if err != nil {
		le := err.(*dialect.LexError)
		fs.errorf(diagnostics.CodeConstructGrammar, fs.span(le.Pos, le.Pos+1), "%s", le.Msg)
		return dialect.Token{Kind: dialect.KindEOF, Start: len(fs.src), End: len(fs.src)}
	}
	return tok
}

// skipTrivia advances past whitespace and comments.
func (fs *fileScan) skipTrivia(pos int) int {
	for {
		tok := fs.lexAt(pos)
		switch tok.Kind {
		case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment:
			pos = tok.End
		default:
			return pos
		}
	}
}

// parseArgList parses "(ident, ident, …)" starting at pos (which must
// point at '('). Returns the idents, their span, and the offset past
// ')'. ok=false means unrecoverable grammar error (already reported).
func (fs *fileScan) parseArgList(constructName string, markerStart, pos int) ([]string, int, bool) {
	if pos >= len(fs.src) || fs.src[pos] != '(' {
		fs.errorf(diagnostics.CodeConstructGrammar, fs.span(markerStart, pos),
			"@%s arguments must follow immediately: @%s(...)", constructName, constructName)
		return nil, pos, false
	}
	pos++ // consume '('
	var idents []string
	for {
		pos = fs.skipTrivia(pos)
		tok := fs.lexAt(pos)
		if tok.Kind != dialect.KindIdent {
			fs.errorf(diagnostics.CodeConstructGrammar, fs.span(tok.Start, tok.End),
				"@%s: expected an identifier", constructName)
			return nil, tok.End, false
		}
		if !snakeRe.MatchString(tok.Text) {
			fs.errorf(diagnostics.CodeBadIdentifier, fs.span(tok.Start, tok.End),
				"%q must be snake_case ([a-z][a-z0-9_]*)", tok.Text)
		}
		idents = append(idents, tok.Text)
		pos = fs.skipTrivia(tok.End)
		next := fs.lexAt(pos)
		switch next.Kind {
		case dialect.KindComma:
			pos = next.End
		case dialect.KindRParen:
			return idents, next.End, true
		default:
			fs.errorf(diagnostics.CodeConstructGrammar, fs.span(next.Start, next.End),
				"@%s: expected ',' or ')'", constructName)
			return nil, next.End, false
		}
	}
}

// consumeBody advances from pos to the next construct marker belonging
// to the *current* nesting level, collecting params along the way.
// terminators lists the construct names that end this body (e.g.
// {"endif"} or {"case","default","end"}). Nested @if-present/@choose
// are reported as SQLETCH012 and skipped with a marker stack.
// Returns (bodyEnd, terminatorName, posAtTerminatorStart).
func (fs *fileScan) consumeBody(pos int, terminators map[string]bool,
	q *QueryTemplate, guards []GuardAtom, inCase, inFT bool) (int, string) {

	var stack []string // expected closers of nested constructs
	for {
		if pos < len(fs.src) && fs.src[pos] == '@' {
			if name, nameEnd := matchConstruct(fs.src, pos); name != "" {
				switch name {
				case "if-present", "choose", "when":
					fs.emit(diagnostics.Errorf(diagnostics.CodeConstructNesting,
						fs.span(pos, nameEnd), "constructs do not nest (R5)").
						WithHint("use a multi-parameter guard: @if-present(a, b)"))
					if name == "if-present" {
						stack = append(stack, "endif")
					} else {
						stack = append(stack, "end")
					}
					pos = nameEnd
					continue
				case "endif", "end":
					if len(stack) > 0 {
						if stack[len(stack)-1] == name {
							stack = stack[:len(stack)-1]
						}
						pos = nameEnd
						continue
					}
					if terminators[name] {
						return pos, name
					}
					fs.errorf(diagnostics.CodeConstructGrammar, fs.span(pos, nameEnd), "unmatched @%s", name)
					pos = nameEnd
					continue
				case "order-by", "filter-tree":
					fs.emit(diagnostics.Errorf(diagnostics.CodeConstructNesting,
						fs.span(pos, nameEnd), "constructs do not nest (R5)"))
					stack = append(stack, "end")
					pos = nameEnd
					continue
				case "in":
					if len(stack) == 0 {
						fs.errorf(diagnostics.CodeConstructBadSlot, fs.span(pos, nameEnd),
							"@in inside guarded fragments is not supported yet; write the membership test directly (PostgreSQL: `= ANY(:param)`)")
					}
					pos = nameEnd
					continue
				case "case", "default", "key", "predicate":
					if len(stack) == 0 && terminators[name] {
						return pos, name
					}
					pos = nameEnd
					continue
				}
			}
		}
		tok := fs.lexAt(pos)
		if tok.Kind == dialect.KindEOF {
			return pos, ""
		}
		if len(stack) == 0 {
			switch tok.Kind {
			case dialect.KindParamRef:
				fs.recordParam(q, tok.Text[1:], fs.span(tok.Start, tok.End), guards, inCase, inFT)
			case dialect.KindPositionalParam:
				fs.errorf(diagnostics.CodePositionalParam, fs.span(tok.Start, tok.End),
					"positional parameter %q is not allowed; use a named parameter (:name)", tok.Text)
			}
		}
		pos = tok.End
	}
}

func (fs *fileScan) parseIfPresent(pos, nameEnd int) int {
	args, afterArgs, ok := fs.parseArgList("if-present", pos, nameEnd)
	if !ok {
		return afterArgs
	}
	guards := make([]GuardAtom, 0, len(args))
	seen := map[string]bool{}
	for _, a := range args {
		if seen[a] {
			fs.errorf(diagnostics.CodeConstructGrammar, fs.span(pos, afterArgs),
				"@if-present: duplicate guard parameter %q", a)
			continue
		}
		seen[a] = true
		guards = append(guards, GuardAtom{Param: a})
	}
	return fs.finishGuardBlock(pos, afterArgs, guards, "if-present", "endif")
}

// parseWhen parses `@when(param op literal) … @end` into an IfPresent
// item carrying a single value atom — everything downstream (render,
// frags, rules, shapes) treats value and presence atoms uniformly.
func (fs *fileScan) parseWhen(pos, nameEnd int) int {
	bad := func(span diagnostics.Span, format string, args ...any) int {
		fs.errorf(diagnostics.CodeConstructGrammar, span, format, args...)
		return span.End
	}
	p := nameEnd
	if p >= len(fs.src) || fs.src[p] != '(' {
		return bad(fs.span(pos, p), "@when arguments must follow immediately: @when(param = literal)")
	}
	p = fs.skipTrivia(p + 1)
	ident := fs.lexAt(p)
	if ident.Kind != dialect.KindIdent {
		return bad(fs.span(ident.Start, ident.End), "@when: expected a parameter name")
	}
	if !snakeRe.MatchString(ident.Text) {
		fs.errorf(diagnostics.CodeBadIdentifier, fs.span(ident.Start, ident.End),
			"%q must be snake_case ([a-z][a-z0-9_]*)", ident.Text)
	}
	p = fs.skipTrivia(ident.End)
	op := fs.lexAt(p)
	if op.Kind != dialect.KindOperator || (op.Text != "=" && op.Text != "!=" && op.Text != "<>") {
		return bad(fs.span(op.Start, op.End), "@when: expected `=` or `!=`")
	}
	opText := op.Text
	if opText == "<>" {
		opText = "!="
	}
	p = fs.skipTrivia(op.End)
	lit := fs.lexAt(p)
	atom := GuardAtom{Param: ident.Text, Op: opText, RawValue: lit.Text}
	switch {
	case lit.Kind == dialect.KindString:
		if msg, hint := whenStringError(lit.Text); msg != "" {
			fs.emit(diagnostics.Errorf(
				diagnostics.CodeWhenStringLiteral, fs.span(lit.Start, lit.End), "%s", msg).
				WithHint("%s", hint))
			return fs.span(lit.Start, lit.End).End
		}
		atom.Kind = ValueString
		atom.Value = unquoteSQLString(lit.Text)
	case lit.Kind == dialect.KindNumber && !strings.ContainsAny(lit.Text, ".eE"):
		if msg, hint := whenIntError(lit.Text); msg != "" {
			fs.emit(diagnostics.Errorf(
				diagnostics.CodeWhenIntLiteral, fs.span(lit.Start, lit.End), "%s", msg).
				WithHint("%s", hint))
			return fs.span(lit.Start, lit.End).End
		}
		atom.Kind = ValueInt
		atom.Value = lit.Text
	case lit.Kind == dialect.KindIdent &&
		(strings.EqualFold(lit.Text, "true") || strings.EqualFold(lit.Text, "false")):
		atom.Kind = ValueBool
		atom.Value = strings.ToLower(lit.Text)
		atom.RawValue = atom.Value
	default:
		return bad(fs.span(lit.Start, lit.End),
			"@when: the literal must be a string, integer, or boolean")
	}
	p = fs.skipTrivia(lit.End)
	rp := fs.lexAt(p)
	if rp.Kind != dialect.KindRParen {
		return bad(fs.span(rp.Start, rp.End), "@when: expected ')'")
	}
	return fs.finishGuardBlock(pos, rp.End, []GuardAtom{atom}, "when", "end")
}

// whenIntError validates a lexed `@when` integer literal, returning a
// diagnostic message and compliant-rewrite hint (both empty when the
// literal is fine). @when integers are non-negative decimal digit runs:
// the lexer splits off signs, and dots/exponents route to the float
// rejection above. Two shapes must be refused because codegen emits the
// literal verbatim into a Go comparison (goLiteral):
//   - a leading zero would be read as a Go OCTAL literal (010 == 8), so
//     the generated guard would silently match a different value than
//     the template names;
//   - a run past int64 parses here but fails to compile in the consumer
//     module.
func whenIntError(text string) (msg, hint string) {
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return fmt.Sprintf("@when integer literal %q must be a plain decimal integer", text),
				"use decimal digits only — hex/octal/binary prefixes (0x, 0o, 0b) are not allowed"
		}
	}
	if len(text) > 1 && text[0] == '0' {
		trimmed := strings.TrimLeft(text, "0")
		if trimmed == "" {
			trimmed = "0"
		}
		return fmt.Sprintf("@when integer literal %q is ambiguous: a leading zero is read as an octal literal in the generated guard, not the decimal value written here", text),
			fmt.Sprintf("drop the leading zero: write %s", trimmed)
	}
	if _, err := strconv.ParseInt(text, 10, 64); err != nil {
		return fmt.Sprintf("@when integer literal %q does not fit a 64-bit signed integer", text),
			"use a value in the range [-9223372036854775808, 9223372036854775807]"
	}
	return "", ""
}

// whenStringError validates a lexed `@when` string literal, returning a
// diagnostic message and compliant-rewrite hint (both empty when the
// literal is fine). @when is documented as a plain string / integer /
// boolean literal, and only a plain single-quoted SQL string is
// decodable by unquoteSQLString below. Every other form the dialect
// lexers classify as KindString — Postgres E-strings (E'…') and
// dollar-quoted strings ($$…$$, $tag$…$tag$), MySQL double-quoted
// strings ("…") and backslash escapes ('a\'b'), and SQLite blob
// literals (x'…') — keeps its delimiters/escapes in the stored guard
// value. Codegen emits that value verbatim into a Go comparison
// (goLiteral), so the guarded fragment would compare against a value
// the runtime never produces and become permanently dead, with no
// other signal. Refuse it here rather than widen the decoder: the
// author must spell the intended value as a plain literal.
//
// "Plain" = leading char `'`, trailing char `'`, and a doubled
// single-quote the only escape inside (no backslash escapes, which are
// dialect-dependent).
func whenStringError(text string) (msg, hint string) {
	if isPlainSQLString(text) {
		return "", ""
	}
	return fmt.Sprintf("@when string literal %s must be a plain single-quoted SQL string: E-strings, dollar-quoted strings, double-quoted strings, blob literals, and backslash escapes keep their delimiters in the guard value, so the generated comparison never matches the runtime value and the guarded fragment is silently dead", text),
		"write a plain single-quoted literal — e.g. 'abc' — and double an embedded quote to escape it ('it''s')"
}

// isPlainSQLString reports whether text is a plain single-quoted SQL
// string literal: it opens and closes with `'` and the only special
// sequence inside is a doubled single-quote. Backslash escapes are rejected
// because their meaning is dialect-dependent (MySQL treats `\` as an
// escape, standard SQL does not) and unquoteSQLString does not honor
// them.
func isPlainSQLString(text string) bool {
	if len(text) < 2 || text[0] != '\'' || text[len(text)-1] != '\'' {
		return false
	}
	inner := text[1 : len(text)-1]
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '\\':
			return false
		case '\'':
			if i+1 >= len(inner) || inner[i+1] != '\'' {
				return false
			}
			i++ // consume the second quote of a doubled ''
		}
	}
	return true
}

// unquoteSQLString converts a lexed plain single-quoted SQL string
// literal ('a”b') to its Go value (a'b). The caller (parseWhen) has
// already rejected every non-plain KindString form via whenStringError,
// so this only ever sees a leading `'` and doubled-single-quote escaping.
func unquoteSQLString(text string) string {
	inner := text
	if len(inner) >= 2 && inner[0] == '\'' {
		inner = inner[1 : len(inner)-1]
	}
	return strings.ReplaceAll(inner, "''", "'")
}

// finishGuardBlock consumes the body and closing marker of a guarded
// block and classifies its slot — shared by @if-present and @when.
func (fs *fileScan) finishGuardBlock(pos, afterArgs int, guards []GuardAtom, markerName, terminator string) int {
	qb := fs.qb
	allowedDepth := 0
	if qb.ctx == ctxInsertColumns || qb.ctx == ctxInsertValueRow {
		allowedDepth = 1 // the INSERT list parens are sanctioned slots
	}
	if qb.depth != allowedDepth {
		fs.errorf(diagnostics.CodeConstructNested, fs.span(pos, afterArgs),
			"constructs may not appear inside parentheses, subqueries, or CTEs (R1); they belong at the statement's top level")
	}
	ctx := qb.ctx

	qb.flushSkeleton(fs, pos)

	bodyStart := afterArgs
	bodyEnd, term := fs.consumeBody(bodyStart, map[string]bool{terminator: true}, qb.q, guards, false, false)
	endPos := bodyEnd
	if term == "" {
		fs.errorf(diagnostics.CodeConstructGrammar, fs.span(pos, afterArgs),
			"unterminated @%s: missing @%s", markerName, terminator)
	} else {
		_, e := matchConstruct(fs.src, bodyEnd)
		endPos = e
	}

	body, bodyOff := trimEdges(fs.src, bodyStart, bodyEnd)
	item := &IfPresent{
		Guards:   guards,
		Body:     body,
		Span:     fs.span(pos, endPos),
		BodySpan: fs.span(bodyOff, bodyOff+len(body)),
	}

	switch ctx {
	case ctxWhere, ctxHaving:
		item.Slot = SlotWhereConjunct
		if ctx == ctxHaving {
			item.Slot = SlotHavingConjunct
		}
		stripped, off, hadAnd := fs.stripLeadingToken(body, bodyOff, isAndToken)
		if !hadAnd {
			fs.emit(diagnostics.Errorf(diagnostics.CodeConjunctNeedsAnd, item.BodySpan,
				"an optional conjunct must be written as `AND <predicate>`; sqletch owns the separator"))
		} else {
			item.Sep = SepAnd
			item.Body = stripped
			item.BodySpan = fs.span(off, off+len(stripped))
		}
	case ctxSet:
		item.Slot = SlotSetItem
		stripped, off, hadComma := fs.stripLeadingToken(body, bodyOff, isCommaToken)
		if !hadComma {
			fs.emit(diagnostics.Errorf(diagnostics.CodeConjunctNeedsAnd, item.BodySpan,
				"an optional SET item must be written as `, column = <expr>`; sqletch owns the separator"))
		} else {
			item.Sep = SepComma
			item.Body = stripped
			item.BodySpan = fs.span(off, off+len(stripped))
		}
	case ctxInsertColumns:
		item.Slot = SlotInsertColumn
		stripped, off, hadComma := fs.stripLeadingToken(body, bodyOff, isCommaToken)
		if !hadComma {
			fs.emit(diagnostics.Errorf(diagnostics.CodeConjunctNeedsAnd, item.BodySpan,
				"an optional INSERT column must be written as `, column_name`; sqletch owns the separator"))
		} else {
			item.Sep = SepComma
			item.Body = stripped
			item.BodySpan = fs.span(off, off+len(stripped))
		}
		if !fs.isSingleIdent(item.Body) {
			fs.errorf(diagnostics.CodeBadIdentifier, item.BodySpan,
				"an optional INSERT column item must be exactly one column name (got %q)", item.Body)
		}
		qb.q.InsertColGuards = append(qb.q.InsertColGuards,
			GuardedItem{Name: item.Body, Guards: guards, Span: item.BodySpan})
		qb.colGuardSeen = true
	case ctxInsertValueRow:
		item.Slot = SlotInsertValue
		stripped, off, hadComma := fs.stripLeadingToken(body, bodyOff, isCommaToken)
		if !hadComma {
			fs.emit(diagnostics.Errorf(diagnostics.CodeConjunctNeedsAnd, item.BodySpan,
				"an optional VALUES item must be written as `, <expr>`; sqletch owns the separator"))
		} else {
			item.Sep = SepComma
			item.Body = stripped
			item.BodySpan = fs.span(off, off+len(stripped))
		}
		row := len(qb.q.InsertValGuards) - 1
		if row >= 0 {
			qb.q.InsertValGuards[row] = append(qb.q.InsertValGuards[row],
				GuardedItem{Guards: guards, Span: item.BodySpan})
		}
		qb.rowGuardSeen = true
	case ctxFrom:
		item.Slot = SlotJoinItem
		item.Sep = SepNone
	default:
		fs.errorf(diagnostics.CodeConstructBadSlot, fs.span(pos, endPos),
			"@%s is not allowed in the %s position; allowed slots: WHERE/HAVING conjunct, SET item, INSERT column/value pair, FROM join item", markerName, ctx)
	}

	qb.q.Items = append(qb.q.Items, item)
	qb.skelStart = endPos
	return endPos
}

func (fs *fileScan) parseChoose(pos, nameEnd int) int {
	qb := fs.qb
	args, afterArgs, ok := fs.parseArgList("choose", pos, nameEnd)
	if !ok || len(args) != 1 {
		if ok {
			fs.errorf(diagnostics.CodeConstructGrammar, fs.span(pos, afterArgs),
				"@choose takes exactly one parameter")
		}
		return afterArgs
	}
	if qb.depth > 0 {
		fs.errorf(diagnostics.CodeConstructNested, fs.span(pos, afterArgs),
			"constructs may not appear inside parentheses, subqueries, or CTEs (R1); they belong at the statement's top level")
	}
	if qb.ctx == ctxNone {
		fs.errorf(diagnostics.CodeConstructBadSlot, fs.span(pos, afterArgs),
			"@choose is not allowed before the statement begins")
	}
	inProjection := qb.ctx == ctxProjection

	qb.flushSkeleton(fs, pos)

	item := &Choose{Param: args[0]}
	caseNames := map[string]bool{}
	p := afterArgs
	terminators := map[string]bool{"case": true, "default": true, "end": true}

	// leading trivia before the first @case
	lead := fs.skipTrivia(p)
	if lead < len(fs.src) && fs.src[lead] != '@' {
		fs.errorf(diagnostics.CodeChooseStructure, fs.span(lead, lead+1),
			"@choose: expected @case before any content")
	}

	endPos := p
	sawDefault := false
	for {
		bodyEnd, term := fs.consumeBody(p, terminators, qb.q, nil, true, false)
		if term == "" {
			fs.errorf(diagnostics.CodeConstructGrammar, fs.span(pos, nameEnd),
				"unterminated @choose: missing @end")
			endPos = bodyEnd
			break
		}
		_, markerEnd := matchConstruct(fs.src, bodyEnd)
		switch term {
		case "end":
			endPos = markerEnd
		case "case":
			if sawDefault {
				fs.errorf(diagnostics.CodeChooseStructure, fs.span(bodyEnd, markerEnd),
					"@default must be the last case of a @choose")
			}
			names, afterCase, ok := fs.parseArgList("case", bodyEnd, markerEnd)
			if !ok || len(names) != 1 {
				if ok {
					fs.errorf(diagnostics.CodeConstructGrammar, fs.span(bodyEnd, afterCase),
						"@case takes exactly one name")
				}
				p = afterCase
				continue
			}
			if caseNames[names[0]] {
				fs.errorf(diagnostics.CodeChooseStructure, fs.span(bodyEnd, afterCase),
					"@choose: duplicate case %q", names[0])
			}
			caseNames[names[0]] = true
			caseBodyEnd, _ := fs.peekBodyEnd(afterCase, terminators)
			body, bodyOff := trimEdges(fs.src, afterCase, caseBodyEnd)
			cs := ChooseCase{Name: names[0], Body: body, Span: fs.span(bodyOff, bodyOff+len(body))}
			item.Cases = append(item.Cases, cs)
			p = afterCase
			continue
		case "default":
			if sawDefault {
				fs.errorf(diagnostics.CodeChooseStructure, fs.span(bodyEnd, markerEnd),
					"@choose: duplicate @default")
			}
			sawDefault = true
			caseBodyEnd, _ := fs.peekBodyEnd(markerEnd, terminators)
			body, bodyOff := trimEdges(fs.src, markerEnd, caseBodyEnd)
			cs := ChooseCase{Body: body, Span: fs.span(bodyOff, bodyOff+len(body))}
			item.Default = &cs
			p = markerEnd
			continue
		}
		break
	}

	if len(item.Cases) == 0 {
		fs.errorf(diagnostics.CodeChooseStructure, fs.span(pos, endPos),
			"@choose needs at least one @case")
	}

	item.Span = fs.span(pos, endPos)
	fs.classifyChoose(item, inProjection)
	qb.q.Items = append(qb.q.Items, item)
	switch item.Slot {
	case SlotOrderBy:
		qb.ctx = ctxOrderBy
	case SlotGroupBy:
		qb.ctx = ctxGroupBy
	}
	qb.skelStart = endPos
	return endPos
}

// parseOrderBy parses `@order-by(param) @key(name) expr … [@default
// clause] @end`.
func (fs *fileScan) parseOrderBy(pos, nameEnd int) int {
	qb := fs.qb
	args, afterArgs, ok := fs.parseArgList("order-by", pos, nameEnd)
	if !ok || len(args) != 1 {
		if ok {
			fs.errorf(diagnostics.CodeConstructGrammar, fs.span(pos, afterArgs),
				"@order-by takes exactly one parameter")
		}
		return afterArgs
	}
	if qb.depth > 0 {
		fs.errorf(diagnostics.CodeConstructNested, fs.span(pos, afterArgs),
			"constructs may not appear inside parentheses, subqueries, or CTEs (R1); they belong at the statement's top level")
	}
	if qb.ctx == ctxProjection || qb.ctx == ctxNone {
		fs.errorf(diagnostics.CodeConstructBadSlot, fs.span(pos, afterArgs),
			"@order-by is not allowed in the %s position; it replaces the statement-level ORDER BY clause", qb.ctx)
	}

	qb.flushSkeleton(fs, pos)

	item := &OrderBy{Param: args[0]}
	keyNames := map[string]bool{}
	terminators := map[string]bool{"key": true, "default": true, "end": true}
	p := afterArgs

	lead := fs.skipTrivia(p)
	if lead < len(fs.src) && fs.src[lead] != '@' {
		fs.errorf(diagnostics.CodeChooseStructure, fs.span(lead, lead+1),
			"@order-by: expected @key before any content")
	}

	endPos := p
	sawDefault := false
	for {
		bodyEnd, term := fs.consumeBody(p, terminators, qb.q, nil, true, false)
		if term == "" {
			fs.errorf(diagnostics.CodeConstructGrammar, fs.span(pos, nameEnd),
				"unterminated @order-by: missing @end")
			endPos = bodyEnd
			break
		}
		_, markerEnd := matchConstruct(fs.src, bodyEnd)
		switch term {
		case "end":
			endPos = markerEnd
		case "key":
			if sawDefault {
				fs.errorf(diagnostics.CodeChooseStructure, fs.span(bodyEnd, markerEnd),
					"@default must be the last entry of an @order-by")
			}
			names, afterKey, ok := fs.parseArgList("key", bodyEnd, markerEnd)
			if !ok || len(names) != 1 {
				if ok {
					fs.errorf(diagnostics.CodeConstructGrammar, fs.span(bodyEnd, afterKey),
						"@key takes exactly one name")
				}
				p = afterKey
				continue
			}
			if keyNames[names[0]] {
				fs.errorf(diagnostics.CodeChooseStructure, fs.span(bodyEnd, afterKey),
					"@order-by: duplicate key %q", names[0])
			}
			keyNames[names[0]] = true
			keyBodyEnd, _ := fs.peekBodyEnd(afterKey, terminators)
			body, bodyOff := trimEdges(fs.src, afterKey, keyBodyEnd)
			if body == "" {
				fs.errorf(diagnostics.CodeChooseStructure, fs.span(afterKey, afterKey+1),
					"@key body must be a non-empty sort expression")
			}
			item.Keys = append(item.Keys, OrderKey{Name: names[0], Body: body,
				Span: fs.span(bodyOff, bodyOff+len(body))})
			p = afterKey
			continue
		case "default":
			if sawDefault {
				fs.errorf(diagnostics.CodeChooseStructure, fs.span(bodyEnd, markerEnd),
					"@order-by: duplicate @default")
			}
			sawDefault = true
			caseBodyEnd, _ := fs.peekBodyEnd(markerEnd, terminators)
			body, bodyOff := trimEdges(fs.src, markerEnd, caseBodyEnd)
			if body != "" && fs.clauseKeywordOf(body) != "ORDER" {
				fs.errorf(diagnostics.CodeChooseStructure, fs.span(bodyOff, bodyOff+len(body)),
					"the @order-by @default body must be a whole `ORDER BY …` clause (or empty)")
			}
			item.Default = &ChooseCase{Body: body, Span: fs.span(bodyOff, bodyOff+len(body))}
			p = markerEnd
			continue
		}
		break
	}

	if len(item.Keys) == 0 {
		fs.errorf(diagnostics.CodeChooseStructure, fs.span(pos, endPos),
			"@order-by needs at least one @key")
	}

	item.Span = fs.span(pos, endPos)
	qb.q.Items = append(qb.q.Items, item)
	qb.ctx = ctxOrderBy
	qb.skelStart = endPos
	return endPos
}

// parseFilterTree parses `@filter-tree[!](param) @predicate(name)
// expr … @end`. Restrictions: at most one block per query (v0.3), and
// a WHERE/HAVING conjunct position written directly after an
// unconditional `AND` (the tail side is checked in finalize).
func (fs *fileScan) parseFilterTree(pos, nameEnd int) int {
	qb := fs.qb
	required := false
	p := nameEnd
	if p < len(fs.src) && fs.src[p] == '!' {
		required = true
		p++
	}
	args, afterArgs, ok := fs.parseArgList("filter-tree", pos, p)
	if !ok || len(args) != 1 {
		if ok {
			fs.errorf(diagnostics.CodeConstructGrammar, fs.span(pos, afterArgs),
				"@filter-tree takes exactly one parameter")
		}
		return afterArgs
	}
	if qb.depth > 0 {
		fs.errorf(diagnostics.CodeConstructNested, fs.span(pos, afterArgs),
			"constructs may not appear inside parentheses, subqueries, or CTEs (R1); they belong at the statement's top level")
	}
	if qb.ctx != ctxWhere && qb.ctx != ctxHaving {
		fs.errorf(diagnostics.CodeConstructBadSlot, fs.span(pos, afterArgs),
			"@filter-tree is only allowed as a WHERE or HAVING conjunct (write it after `AND`)")
	} else if fs.lastSignificantToken(qb.skelStart, pos) != "AND" {
		// The empty tree renders TRUE, which is only sound when it
		// substitutes one whole AND-conjunct: under OR or NOT the
		// filter would silently vanish or change meaning.
		fs.emit(diagnostics.Errorf(diagnostics.CodeConjunctNeedsAnd,
			fs.span(pos, afterArgs),
			"@filter-tree must directly follow an unconditional `AND`; its empty tree renders TRUE, which must substitute one whole conjunct").
			WithHint("anchor the clause and give the construct its own conjunct: `WHERE TRUE` then `AND @filter-tree(...)`"))
	}
	for _, it := range qb.q.Items {
		if _, dup := it.(*FilterTree); dup {
			fs.errorf(diagnostics.CodeChooseStructure, fs.span(pos, afterArgs),
				"at most one @filter-tree per query (v0.3)")
			break
		}
	}

	qb.flushSkeleton(fs, pos)

	item := &FilterTree{Param: args[0], Required: required, Slot: SlotWhereConjunct}
	if qb.ctx == ctxHaving {
		item.Slot = SlotHavingConjunct
	}
	predNames := map[string]bool{}
	terminators := map[string]bool{"predicate": true, "end": true}
	p = afterArgs

	lead := fs.skipTrivia(p)
	if lead < len(fs.src) && fs.src[lead] != '@' {
		fs.errorf(diagnostics.CodeChooseStructure, fs.span(lead, lead+1),
			"@filter-tree: expected @predicate before any content")
	}

	endPos := p
	for {
		bodyEnd, term := fs.consumeBody(p, terminators, qb.q, nil, false, true)
		if term == "" {
			fs.errorf(diagnostics.CodeConstructGrammar, fs.span(pos, nameEnd),
				"unterminated @filter-tree: missing @end")
			endPos = bodyEnd
			break
		}
		_, markerEnd := matchConstruct(fs.src, bodyEnd)
		switch term {
		case "end":
			endPos = markerEnd
		case "predicate":
			names, afterPred, ok := fs.parseArgList("predicate", bodyEnd, markerEnd)
			if !ok || len(names) != 1 {
				if ok {
					fs.errorf(diagnostics.CodeConstructGrammar, fs.span(bodyEnd, afterPred),
						"@predicate takes exactly one name")
				}
				p = afterPred
				continue
			}
			if predNames[names[0]] {
				fs.errorf(diagnostics.CodeChooseStructure, fs.span(bodyEnd, afterPred),
					"@filter-tree: duplicate predicate %q", names[0])
			}
			predNames[names[0]] = true
			predBodyEnd, _ := fs.peekBodyEnd(afterPred, terminators)
			body, bodyOff := trimEdges(fs.src, afterPred, predBodyEnd)
			if body == "" {
				fs.errorf(diagnostics.CodeChooseStructure, fs.span(afterPred, afterPred+1),
					"@predicate body must be a non-empty boolean expression")
			}
			item.Predicates = append(item.Predicates, Predicate{
				Name: names[0], Body: body,
				Params: fs.distinctParams(body),
				Span:   fs.span(bodyOff, bodyOff+len(body)),
			})
			p = afterPred
			continue
		}
		break
	}

	if len(item.Predicates) == 0 {
		fs.errorf(diagnostics.CodeChooseStructure, fs.span(pos, endPos),
			"@filter-tree needs at least one @predicate")
	}

	item.Span = fs.span(pos, endPos)
	qb.q.Items = append(qb.q.Items, item)
	qb.skelStart = endPos
	return endPos
}

// parseIn parses the inline membership construct `@in(:param)`. On
// PostgreSQL it renders as a single static `= ANY($n)`; expanding
// dialects render per-arity IN lists.
func (fs *fileScan) parseIn(pos, nameEnd int) int {
	qb := fs.qb
	p := nameEnd
	if p >= len(fs.src) || fs.src[p] != '(' {
		fs.errorf(diagnostics.CodeConstructGrammar, fs.span(pos, p),
			"@in arguments must follow immediately: @in(:param)")
		return p
	}
	p = fs.skipTrivia(p + 1)
	tok := fs.lexAt(p)
	if tok.Kind != dialect.KindParamRef {
		fs.errorf(diagnostics.CodeConstructGrammar, fs.span(tok.Start, tok.End),
			"@in: expected a named parameter (:name)")
		return tok.End
	}
	name := tok.Text[1:]
	p = fs.skipTrivia(tok.End)
	rp := fs.lexAt(p)
	if rp.Kind != dialect.KindRParen {
		fs.errorf(diagnostics.CodeConstructGrammar, fs.span(rp.Start, rp.End),
			"@in: expected ')'")
		return rp.End
	}
	if qb.depth != 0 {
		fs.errorf(diagnostics.CodeConstructNested, fs.span(pos, rp.End),
			"@in may not appear inside parentheses or subqueries (v0.4)")
	}
	if qb.ctx != ctxWhere && qb.ctx != ctxHaving {
		fs.errorf(diagnostics.CodeConstructBadSlot, fs.span(pos, rp.End),
			"@in is allowed in WHERE/HAVING expressions (e.g. `u.status @in(:statuses)`)")
	}
	fs.recordParam(qb.q, name, fs.span(tok.Start, tok.End), nil, false, false)
	// Mark this occurrence as an @in list bind so R9 can forbid the same
	// parameter also appearing as a plain scalar (SQLETCH120).
	if p := qb.q.Params[name]; p != nil && len(p.Occurrences) > 0 {
		p.Occurrences[len(p.Occurrences)-1].InIn = true
	}

	qb.flushSkeleton(fs, pos)
	qb.q.Items = append(qb.q.Items, &InExpr{Param: name, Span: fs.span(pos, rp.End)})
	qb.skelStart = rp.End
	return rp.End
}

// distinctParams lists a body's :params in first-occurrence order —
// the predicate constructor's argument order.
func (fs *fileScan) distinctParams(body string) []string {
	src := []byte(body)
	var out []string
	seen := map[string]bool{}
	pos := 0
	for {
		tok, err := fs.profile.NextToken(src, pos)
		if err != nil || tok.Kind == dialect.KindEOF {
			return out
		}
		if tok.Kind == dialect.KindParamRef && !seen[tok.Text[1:]] {
			seen[tok.Text[1:]] = true
			out = append(out, tok.Text[1:])
		}
		pos = tok.End
	}
}

// classifyChoose determines the block's slot from its position and
// case bodies, and validates the cases against that slot:
//   - projection position: every body (incl. @default) is a non-empty
//     expression — an empty body would change the result shape (R2)
//   - clause position: bodies are whole ORDER BY / GROUP BY clauses,
//     all agreeing on the same clause keyword; only @default may be
//     empty (the clause is omissible, R6)
func (fs *fileScan) classifyChoose(item *Choose, inProjection bool) {
	if inProjection {
		item.Slot = SlotProjExpr
		check := func(body string, span diagnostics.Span) {
			if body == "" {
				fs.errorf(diagnostics.CodeChooseStructure, span,
					"in a projection slot every case (including @default) must be a non-empty expression; an empty case would change the result shape (R2)")
			}
		}
		for _, cs := range item.Cases {
			check(cs.Body, cs.Span)
		}
		if item.Default != nil {
			check(item.Default.Body, item.Default.Span)
		}
		return
	}

	kw := ""
	for _, cs := range item.Cases {
		k := fs.clauseKeywordOf(cs.Body)
		if k == "" {
			fs.errorf(diagnostics.CodeChooseStructure, cs.Span,
				"every @case body must start with `ORDER BY` or `GROUP BY` in this position")
			return
		}
		if kw == "" {
			kw = k
		} else if kw != k {
			fs.errorf(diagnostics.CodeChooseStructure, cs.Span,
				"all cases of one @choose must target the same clause (mixing %s and %s)", kw, k)
			return
		}
	}
	if item.Default != nil && item.Default.Body != "" {
		if k := fs.clauseKeywordOf(item.Default.Body); k != kw {
			fs.errorf(diagnostics.CodeChooseStructure, item.Default.Span,
				"the @default body must start with `%s BY` like the other cases", kw)
			return
		}
	}
	switch kw {
	case "ORDER":
		item.Slot = SlotOrderBy
	case "GROUP":
		item.Slot = SlotGroupBy
	}
}

// clauseKeywordOf returns "ORDER" or "GROUP" when the body starts with
// that clause ("" otherwise).
func (fs *fileScan) clauseKeywordOf(body string) string {
	b := []byte(body)
	t1 := fs.firstTokenOf(b)
	if t1 == nil || t1.Kind != dialect.KindIdent {
		return ""
	}
	kw := strings.ToUpper(t1.Text)
	if kw != "ORDER" && kw != "GROUP" {
		return ""
	}
	t2 := fs.firstTokenOf(b[t1.End:])
	if t2 == nil || t2.Kind != dialect.KindIdent || strings.ToUpper(t2.Text) != "BY" {
		return ""
	}
	return kw
}

// peekBodyEnd finds where the body starting at pos ends (next
// terminator at level 0) without recording params (consumeBody already
// recorded them when the enclosing loop passed over this region — the
// choose parser walks bodies twice: once to find structure, once here
// for the text span). To avoid double param recording, this variant
// records nothing.
func (fs *fileScan) peekBodyEnd(pos int, terminators map[string]bool) (int, string) {
	var stack []string
	for {
		if pos < len(fs.src) && fs.src[pos] == '@' {
			if name, nameEnd := matchConstruct(fs.src, pos); name != "" {
				switch name {
				case "if-present":
					stack = append(stack, "endif")
					pos = nameEnd
					continue
				case "choose", "when", "order-by", "filter-tree":
					stack = append(stack, "end")
					pos = nameEnd
					continue
				case "endif":
					if len(stack) > 0 && stack[len(stack)-1] == "endif" {
						stack = stack[:len(stack)-1]
					}
					pos = nameEnd
					continue
				case "end":
					if len(stack) > 0 {
						if stack[len(stack)-1] == "end" {
							stack = stack[:len(stack)-1]
						}
						pos = nameEnd
						continue
					}
					if terminators[name] {
						return pos, name
					}
					pos = nameEnd
					continue
				case "case", "default", "key", "predicate":
					if len(stack) == 0 && terminators[name] {
						return pos, name
					}
					pos = nameEnd
					continue
				case "in":
					pos = nameEnd
					continue
				}
			}
		}
		tok := fs.lexAt(pos)
		if tok.Kind == dialect.KindEOF {
			return pos, ""
		}
		pos = tok.End
	}
}

// firstTokenOf returns the first non-trivia token of b, or nil.
func (fs *fileScan) firstTokenOf(b []byte) *dialect.Token {
	pos := 0
	for {
		tok, err := fs.profile.NextToken(b, pos)
		if err != nil || tok.Kind == dialect.KindEOF {
			return nil
		}
		switch tok.Kind {
		case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment:
			pos = tok.End
		default:
			return &tok
		}
	}
}

// isSingleIdent reports whether body lexes to exactly one (possibly
// quoted) identifier.
func (fs *fileScan) isSingleIdent(body string) bool {
	b := []byte(body)
	t := fs.firstTokenOf(b)
	if t == nil || (t.Kind != dialect.KindIdent && t.Kind != dialect.KindQuotedIdent) {
		return false
	}
	return fs.firstTokenOf(b[t.End:]) == nil
}

func isAndToken(t *dialect.Token) bool {
	return t.Kind == dialect.KindIdent && strings.ToUpper(t.Text) == "AND"
}

func isCommaToken(t *dialect.Token) bool { return t.Kind == dialect.KindComma }

// stripLeadingToken removes the composer-owned separator token from a
// body (which starts at absolute offset bodyOff). Returns the stripped
// body, its new absolute offset, and whether the separator was present.
func (fs *fileScan) stripLeadingToken(body string, bodyOff int, match func(*dialect.Token) bool) (string, int, bool) {
	b := []byte(body)
	t := fs.firstTokenOf(b)
	if t == nil || !match(t) {
		return body, bodyOff, false
	}
	rest := body[t.End:]
	trimmed := strings.TrimLeft(rest, " \t\r\n")
	off := bodyOff + t.End + (len(rest) - len(trimmed))
	return strings.TrimRight(trimmed, " \t\r\n"), off, true
}

func (qb *queryBuilder) flushSkeleton(fs *fileScan, upTo int) {
	if qb.skelStart >= 0 && qb.skelStart < upTo {
		qb.q.Items = append(qb.q.Items, &Skeleton{
			Text: string(fs.src[qb.skelStart:upTo]),
			Span: fs.span(qb.skelStart, upTo),
		})
	}
	qb.skelStart = -1
}

// trimEdges trims whitespace-only edges of src[start:end], returning
// the trimmed text and its absolute start offset.
func trimEdges(src []byte, start, end int) (string, int) {
	for start < end && isTrimByte(src[start]) {
		start++
	}
	for end > start && isTrimByte(src[end-1]) {
		end--
	}
	return string(src[start:end]), start
}

func isTrimByte(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func (fs *fileScan) finalize(file *QueryFile, upTo int) {
	qb := fs.qb
	if qb == nil {
		return
	}
	qb.flushSkeleton(fs, upTo)
	fs.checkFilterTreeTail(qb.q)
	fs.checkEncodingLimits(qb.q)
	fs.assignGuardBits(qb.q)
	file.Queries = append(file.Queries, qb.q)
	fs.qb = nil
}

// lastSignificantToken returns the last non-trivia token of the source
// range [from, to) — identifiers uppercased, other tokens verbatim —
// or "" when the range holds none.
func (fs *fileScan) lastSignificantToken(from, to int) string {
	if from < 0 || to > len(fs.src) || from >= to {
		return ""
	}
	src := fs.src[from:to]
	last := ""
	pos := 0
	for {
		tok, err := fs.profile.NextToken(src, pos)
		if err != nil || tok.Kind == dialect.KindEOF {
			return last
		}
		switch tok.Kind {
		case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment:
		case dialect.KindIdent:
			last = strings.ToUpper(tok.Text)
		default:
			last = tok.Text
		}
		pos = tok.End
	}
}

// filterTreeTailTokens are the tokens allowed to follow a @filter-tree
// block: the next conjunct's AND, a clause keyword, or the end of the
// statement. Anything else would splice the construct into a larger
// expression, and the empty tree's TRUE rendering would no longer
// substitute one whole conjunct (R1).
var filterTreeTailTokens = map[string]bool{
	"AND": true, "GROUP": true, "HAVING": true, "WINDOW": true,
	"ORDER": true, "LIMIT": true, "OFFSET": true, "FETCH": true,
	"FOR": true, "UNION": true, "INTERSECT": true, "EXCEPT": true,
	"RETURNING": true, ";": true,
}

// checkFilterTreeTail enforces the closing half of the conjunct-anchor
// discipline (the opening half — the preceding unconditional `AND` —
// is checked at parse time in parseFilterTree). A following construct
// is fine: every construct emits its own complete clause item.
func (fs *fileScan) checkFilterTreeTail(q *QueryTemplate) {
	for i, it := range q.Items {
		ft, ok := it.(*FilterTree)
		if !ok || i+1 >= len(q.Items) {
			continue
		}
		sk, ok := q.Items[i+1].(*Skeleton)
		if !ok {
			continue
		}
		first := fs.firstSignificantToken(sk.Text)
		if first == "" || filterTreeTailTokens[first] {
			continue
		}
		fs.emit(diagnostics.Errorf(diagnostics.CodeConjunctNeedsAnd, ft.Span,
			"@filter-tree must end its conjunct but is followed by %q; its empty tree renders TRUE, which must substitute one whole conjunct", first).
			WithHint("continue with `AND`, a clause keyword, or `;` after @end"))
	}
}

// firstSignificantToken returns the first non-trivia token of text
// (identifiers uppercased), or "" when there is none.
func (fs *fileScan) firstSignificantToken(text string) string {
	src := []byte(text)
	pos := 0
	for {
		tok, err := fs.profile.NextToken(src, pos)
		if err != nil || tok.Kind == dialect.KindEOF {
			return ""
		}
		switch tok.Kind {
		case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment:
			pos = tok.End
			continue
		case dialect.KindIdent:
			return strings.ToUpper(tok.Text)
		default:
			return tok.Text
		}
	}
}

// checkEncodingLimits rejects a query the compiler's fixed-width
// encodings cannot represent: the shape key's selection space, and the
// bind plan's parameter index. Unlike the guard limit these cannot be
// folded into assignGuardBits — the construct dimensions are per block,
// and a query may carry several blocks, each of which should be
// reported.
func (fs *fileScan) checkEncodingLimits(q *QueryTemplate) {
	if n := len(q.ParamOrder); n > MaxParams {
		fs.errorf(diagnostics.CodeTooManyParams, q.HeaderSpan,
			"query %s declares %d parameters; at most %d are supported",
			q.Name, n, MaxParams)
	}
	for _, it := range q.Items {
		switch v := it.(type) {
		case *OrderBy:
			if n := len(v.Keys); n > MaxOrderKeys {
				fs.errorf(diagnostics.CodeTooManyGuards, v.Span,
					"@order-by(%s) declares %d sort keys; at most %d are supported",
					v.Param, n, MaxOrderKeys)
			}
		case *Choose:
			n := len(v.Cases)
			if v.Default != nil {
				n++
			}
			if n > MaxChooseOrdinals {
				fs.errorf(diagnostics.CodeTooManyGuards, v.Span,
					"@choose(%s) declares %d cases (counting @default); at most %d are supported",
					v.Param, n, MaxChooseOrdinals)
			}
		}
	}
}

func (fs *fileScan) assignGuardBits(q *QueryTemplate) {
	seen := map[GuardAtom]int{}
	for _, it := range q.Items {
		ip, ok := it.(*IfPresent)
		if !ok {
			continue
		}
		for _, g := range ip.Guards {
			if _, dup := seen[g]; dup {
				continue
			}
			bit := len(q.GuardAtoms)
			if bit >= MaxGuards {
				fs.errorf(diagnostics.CodeTooManyGuards, ip.Span,
					"a query may have at most %d guard conditions", MaxGuards)
				return
			}
			seen[g] = bit
			q.GuardAtoms = append(q.GuardAtoms, g)
			p := q.Params[g.Param]
			if p == nil {
				p = &Param{Name: g.Param, GuardBit: -1}
				q.Params[g.Param] = p
				q.ParamOrder = append(q.ParamOrder, g.Param)
			}
			p.GuardBit = bit
		}
	}
}
