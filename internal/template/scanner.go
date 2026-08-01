package template

import (
	"regexp"
	"strings"

	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect"
)

// Scanner turns template source into QueryFiles. It is construct-
// generic; dialect lexical structure comes from the LexerProfile.
type Scanner struct {
	profile dialect.LexerProfile
}

func NewScanner(profile dialect.LexerProfile) *Scanner {
	return &Scanner{profile: profile}
}

var headerRe = regexp.MustCompile(`^--\s*name:\s*([A-Za-z][A-Za-z0-9_]*)\s+:(one|many|exec|execrows)\s*$`)
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
	ctxValues
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
	default:
		return "statement"
	}
}

const maxGuards = 64

type fileScan struct {
	path    string
	src     []byte
	profile dialect.LexerProfile
	diags   []diagnostics.Diagnostic

	qb            *queryBuilder
	names         map[string]diagnostics.Span
	strayReported bool
}

type queryBuilder struct {
	q              *QueryTemplate
	skelStart      int // -1 = no open skeleton chunk
	depth          int
	ctx            clauseCtx
	statementEnded bool
	extraReported  bool
}

func (s *Scanner) ScanFile(path string, src []byte) (*QueryFile, []diagnostics.Diagnostic) {
	fs := &fileScan{path: path, src: src, profile: s.profile, names: map[string]diagnostics.Span{}}
	file := &QueryFile{Path: path}

	pos := 0
	for pos <= len(src) {
		if pos < len(src) && src[pos] == '@' {
			if name, nameEnd := matchConstruct(src, pos); name != "" {
				pos = fs.handleConstruct(name, pos, nameEnd)
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
	fs.finalize(file, len(src))
	diagnostics.Sort(fs.diags)
	return file, fs.diags
}

func (fs *fileScan) span(start, end int) diagnostics.Span {
	return diagnostics.Span{File: fs.path, Start: start, End: end}
}

func (fs *fileScan) errorf(code diagnostics.Code, span diagnostics.Span, format string, args ...any) {
	fs.diags = append(fs.diags, diagnostics.Errorf(code, span, format, args...))
}

func (fs *fileScan) handleToken(file *QueryFile, tok dialect.Token) {
	if tok.Kind == dialect.KindLineComment {
		if m := headerRe.FindStringSubmatch(tok.Text); m != nil {
			fs.finalize(file, tok.Start)
			fs.startQuery(m[1], m[2], tok)
			return
		}
	}
	if fs.qb == nil {
		switch tok.Kind {
		case dialect.KindWhitespace, dialect.KindLineComment, dialect.KindBlockComment:
			return
		default:
			if !fs.strayReported {
				fs.errorf(diagnostics.CodeMissingHeader, fs.span(tok.Start, tok.End),
					"statement without a query header; every query starts with `-- name: QueryName :many` (or :one/:exec/:execrows)")
				fs.strayReported = true
			}
			return
		}
	}
	fs.qb.feed(fs, tok)
}

func (fs *fileScan) startQuery(name, ann string, headerTok dialect.Token) {
	if prev, dup := fs.names[name]; dup {
		fs.errorf(diagnostics.CodeDuplicateQueryName, fs.span(headerTok.Start, headerTok.End),
			"duplicate query name %q (previously defined at offset %d)", name, prev.Start)
	}
	fs.names[name] = fs.span(headerTok.Start, headerTok.End)
	annotation := map[string]Annotation{
		"one": AnnotationOne, "many": AnnotationMany,
		"exec": AnnotationExec, "execrows": AnnotationExecRows,
	}[ann]
	fs.qb = &queryBuilder{
		q: &QueryTemplate{
			Name:       name,
			Annotation: annotation,
			HeaderSpan: fs.span(headerTok.Start, headerTok.End),
			Params:     map[string]*Param{},
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
	case dialect.KindLParen:
		qb.depth++
	case dialect.KindRParen:
		if qb.depth > 0 {
			qb.depth--
		}
	case dialect.KindSemicolon:
		if qb.depth == 0 {
			qb.statementEnded = true
		}
	case dialect.KindParamRef:
		fs.recordParam(qb.q, tok.Text[1:], fs.span(tok.Start, tok.End), nil, false)
	case dialect.KindPositionalParam:
		fs.errorf(diagnostics.CodePositionalParam, fs.span(tok.Start, tok.End),
			"positional parameter %q is not allowed; use a named parameter (:name)", tok.Text)
	case dialect.KindIdent:
		if qb.depth == 0 {
			qb.transition(strings.ToUpper(tok.Text))
		}
	}
}

func (qb *queryBuilder) transition(kw string) {
	switch kw {
	case "SELECT":
		qb.ctx = ctxProjection
	case "FROM":
		qb.ctx = ctxFrom
	case "WHERE":
		qb.ctx = ctxWhere
	case "GROUP":
		qb.ctx = ctxGroupBy
	case "HAVING":
		qb.ctx = ctxHaving
	case "ORDER":
		qb.ctx = ctxOrderBy
	case "LIMIT", "OFFSET", "FETCH", "FOR":
		qb.ctx = ctxTail
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
	}
}

func (fs *fileScan) recordParam(q *QueryTemplate, name string, span diagnostics.Span, guards []GuardAtom, inCase bool) {
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
	p.Occurrences = append(p.Occurrences, Occurrence{Span: span, Guards: guards, InChooseCase: inCase})
}

// ---- construct handling ----------------------------------------------------

var constructVocab = map[string]bool{
	"if-present": true, "endif": true,
	"choose": true, "case": true, "default": true, "end": true,
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
	case "choose":
		return fs.parseChoose(pos, nameEnd)
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
	q *QueryTemplate, guards []GuardAtom, inCase bool) (int, string) {

	var stack []string // expected closers of nested constructs
	for {
		if pos < len(fs.src) && fs.src[pos] == '@' {
			if name, nameEnd := matchConstruct(fs.src, pos); name != "" {
				switch name {
				case "if-present", "choose":
					fs.diags = append(fs.diags, diagnostics.Errorf(diagnostics.CodeConstructNesting,
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
				case "case", "default":
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
				fs.recordParam(q, tok.Text[1:], fs.span(tok.Start, tok.End), guards, inCase)
			case dialect.KindPositionalParam:
				fs.errorf(diagnostics.CodePositionalParam, fs.span(tok.Start, tok.End),
					"positional parameter %q is not allowed; use a named parameter (:name)", tok.Text)
			}
		}
		pos = tok.End
	}
}

func (fs *fileScan) parseIfPresent(pos, nameEnd int) int {
	qb := fs.qb
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

	if qb.depth > 0 {
		fs.errorf(diagnostics.CodeConstructNested, fs.span(pos, afterArgs),
			"constructs may not appear inside parentheses, subqueries, or CTEs (R1); they belong at the statement's top level")
	}
	ctx := qb.ctx

	qb.flushSkeleton(fs, pos)

	bodyStart := afterArgs
	bodyEnd, term := fs.consumeBody(bodyStart, map[string]bool{"endif": true}, qb.q, guards, false)
	endPos := bodyEnd
	if term == "" {
		fs.errorf(diagnostics.CodeConstructGrammar, fs.span(pos, nameEnd),
			"unterminated @if-present: missing @endif")
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
	case ctxWhere:
		item.Slot = SlotWhereConjunct
		stripped, off, hadAnd := fs.stripLeadingAnd(body, bodyOff)
		if !hadAnd {
			d := diagnostics.Errorf(diagnostics.CodeConjunctNeedsAnd, item.BodySpan,
				"an optional WHERE conjunct must be written as `AND <predicate>`; sqletch owns the separator")
			fs.diags = append(fs.diags, d)
		} else {
			item.Sep = SepAnd
			item.Body = stripped
			item.BodySpan = fs.span(off, off+len(stripped))
		}
	case ctxFrom:
		item.Slot = SlotJoinItem
		item.Sep = SepNone
	default:
		fs.errorf(diagnostics.CodeConstructBadSlot, fs.span(pos, endPos),
			"@if-present is not allowed in the %s position; allowed slots in v0.1: WHERE conjunct, FROM join item", ctx)
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
	if qb.ctx == ctxProjection || qb.ctx == ctxNone {
		fs.errorf(diagnostics.CodeConstructBadSlot, fs.span(pos, afterArgs),
			"@choose is not allowed in the %s position; the only @choose slot in v0.1 is ORDER BY", qb.ctx)
	}

	qb.flushSkeleton(fs, pos)

	item := &Choose{Param: args[0], Slot: SlotOrderBy}
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
		bodyEnd, term := fs.consumeBody(p, terminators, qb.q, nil, true)
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
			fs.checkOrderByCase(&cs, false)
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
			fs.checkOrderByCase(&cs, true)
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
	qb.q.Items = append(qb.q.Items, item)
	qb.ctx = ctxOrderBy
	qb.skelStart = endPos
	return endPos
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
				case "choose":
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
				case "case", "default":
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
		pos = tok.End
	}
}

func (fs *fileScan) checkOrderByCase(cs *ChooseCase, isDefault bool) {
	if cs.Body == "" {
		if !isDefault {
			fs.errorf(diagnostics.CodeChooseStructure, cs.Span,
				"@case body may not be empty (only @default may be empty)")
		}
		return
	}
	b := []byte(cs.Body)
	t1 := fs.firstTokenOf(b)
	if t1 == nil || t1.Kind != dialect.KindIdent || strings.ToUpper(t1.Text) != "ORDER" {
		fs.errorf(diagnostics.CodeChooseStructure, cs.Span,
			"in the ORDER BY slot, every @case body must start with `ORDER BY`")
		return
	}
	rest := b[t1.End:]
	t2 := fs.firstTokenOf(rest)
	if t2 == nil || t2.Kind != dialect.KindIdent || strings.ToUpper(t2.Text) != "BY" {
		fs.errorf(diagnostics.CodeChooseStructure, cs.Span,
			"in the ORDER BY slot, every @case body must start with `ORDER BY`")
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

// stripLeadingAnd removes a leading AND token from body (which starts
// at absolute offset bodyOff). Returns the stripped body, its new
// absolute offset, and whether AND was present.
func (fs *fileScan) stripLeadingAnd(body string, bodyOff int) (string, int, bool) {
	b := []byte(body)
	t := fs.firstTokenOf(b)
	if t == nil || t.Kind != dialect.KindIdent || strings.ToUpper(t.Text) != "AND" {
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
	fs.assignGuardBits(qb.q)
	file.Queries = append(file.Queries, qb.q)
	fs.qb = nil
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
			if bit >= maxGuards {
				fs.errorf(diagnostics.CodeTooManyGuards, ip.Span,
					"a query may have at most %d guard conditions", maxGuards)
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
