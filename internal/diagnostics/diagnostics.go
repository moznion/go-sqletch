// Package diagnostics defines the user-facing error model shared by
// every compiler phase. All user mistakes surface as Diagnostics with
// stable codes; bare Go errors are reserved for environmental failures.
package diagnostics

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Span is a byte range into an original template file. It lives here
// (not in internal/template) so that every package can attach spans to
// diagnostics without import cycles.
type Span struct {
	File  string
	Start int // byte offset, inclusive
	End   int // byte offset, exclusive
}

func (s Span) IsZero() bool { return s.File == "" && s.Start == 0 && s.End == 0 }

type Severity int

const (
	Error Severity = iota
	Warning
)

func (sv Severity) String() string {
	if sv == Warning {
		return "warning"
	}
	return "error"
}

// Code is a stable diagnostic identifier (e.g. "SQLETCH001").
// Ranges: 0xx lexical/structural (scanner), 1xx rules, 2xx oracle,
// 3xx codegen/config.
type Code string

// Scanner-phase codes (see docs/design/01-template-scanner.md).
const (
	CodeConstructGrammar   Code = "SQLETCH001" // malformed/unterminated construct
	CodeBadIdentifier      Code = "SQLETCH002" // guard/case/param names must be snake_case
	CodeMissingHeader      Code = "SQLETCH003" // statement without -- name: header
	CodeDuplicateQueryName Code = "SQLETCH004"
	CodeMultipleStatements Code = "SQLETCH005"
	CodeConstructNested    Code = "SQLETCH006" // construct inside parens/subquery
	CodeConstructBadSlot   Code = "SQLETCH007" // construct at a non-slot clause
	CodeConjunctNeedsAnd   Code = "SQLETCH008"
	CodeChooseStructure    Code = "SQLETCH009"
	CodeTooManyGuards      Code = "SQLETCH010"
	CodePositionalParam    Code = "SQLETCH011"
	CodeConstructNesting   Code = "SQLETCH012" // guard inside guard (R5)
	CodeTooManyParams      Code = "SQLETCH013" // more parameters than the bind plan's int16 index holds
	CodeWhenIntLiteral     Code = "SQLETCH014" // @when integer literal is ambiguous (leading zero) or out of int64 range
)

// Go-source input codes: templates authored in a `//sqletch:query`
// const inside a .go file (see docs/design/13-go-source-input.md).
// They sit in the scanner band because they report on the same phase —
// getting template bytes out of a file and into the scanner.
const (
	CodeGoParse        Code = "SQLETCH020" // the .go file does not parse
	CodeGoMarkerTarget Code = "SQLETCH021" // //sqletch:query on a non-const declaration
	CodeGoNotRawString Code = "SQLETCH022" // marked const's value is not a raw string literal
	CodeGoBadConstSpec Code = "SQLETCH023" // marked const has no value, or names/values disagree
)

// Rules-phase codes (R1 runs in P2's pipeline position; see
// docs/design/02-rendering.md and 03-structural-rules.md).
const (
	CodeRenderingParse    Code = "SQLETCH100" // a rendering fails to parse
	CodeJoinTypeForbidden Code = "SQLETCH101" // optional join not INNER/LEFT (R2)
	CodeNodeIncomplete    Code = "SQLETCH102" // fragment is not one complete node (R1)
	CodeNotSingleDML      Code = "SQLETCH103" // not exactly one SELECT/UPDATE/INSERT/DELETE

	CodeVacuousGuard     Code = "SQLETCH110" // required param used as guard (R9)
	CodeGuardNeverBinds  Code = "SQLETCH111" // guard param binds nowhere under itself (R9)
	CodeChooseParamBinds Code = "SQLETCH112" // @choose control param used as :name (R9)
	CodeUnanchoredClause Code = "SQLETCH113" // all conjuncts optional, no anchor (R6)
	CodeAmbiguousRef     Code = "SQLETCH114" // unqualified ref matches several relations
	CodeScopeViolation   Code = "SQLETCH115" // reference into optional join w/o guard (R3)
	CodePlannerSensitive Code = "SQLETCH116" // e.g. FOR UPDATE + optional LEFT JOIN
	CodeStarExpansion    Code = "SQLETCH117" // SELECT * would include optional-join columns (R2)
	CodeUnanchoredSet    Code = "SQLETCH118" // every SET/INSERT-list item optional, no anchor (R6)
	CodePairedGuards     Code = "SQLETCH119" // INSERT column/value guard pairing broken (R7)
	CodeOrderByDistinct  Code = "SQLETCH122" // @order-by under DISTINCT ON (prefix-sensitive)
	CodeOrderByNeedsDflt Code = "SQLETCH123" // WITH TIES requires an @order-by @default
	CodePolicyUnscoped   Code = "SQLETCH124" // designated table without the scoping conjunct in every shape
	CodePolicyUnweavable Code = "SQLETCH125" // a policy applies but cannot be woven into this query
	CodePolicyBadOptOut  Code = "SQLETCH126" // @policy-optout names an unknown or inapplicable policy
)

// Oracle-phase codes (see docs/design/04-type-oracle.md).
const (
	CodeServerVersionMismatch Code = "SQLETCH200" // pinned version != connected server
	CodeIndeterminateParam    Code = "SQLETCH201" // undetermined parameter type (add a cast)
	CodeOracleFailure         Code = "SQLETCH202" // prepare/describe failed
	CodeCacheServerDrift      Code = "SQLETCH203" // committed cache was generated against a different server version
	CodeDestructiveReset      Code = "SQLETCH204" // refused to reset a user-supplied database's schema (pass --allow-destructive)
	CodeColumnAgreement       Code = "SQLETCH210" // renderings disagree on result columns
	CodeParamAgreement        Code = "SQLETCH211" // renderings disagree on a param's type
	CodeOptionalInsertNotNull Code = "SQLETCH212" // optional NOT NULL column without default (warning)
	CodeParamHintConflict     Code = "SQLETCH213" // `-- @param` hint disagrees with the oracle (Tier 1)
	CodeNativeUnsupported     Code = "SQLETCH214" // native oracle: query construct outside the modeled subset
	CodeNativeDDL             Code = "SQLETCH215" // native oracle: schema DDL outside the catalog builder's subset
	CodeColumnHintConflict    Code = "SQLETCH216" // `-- @column` hint disagrees with the oracle's column type
)

// Codegen/config codes.
const (
	CodeConfigParse     Code = "SQLETCH300" // sqletch.yaml unreadable/unknown keys
	CodeConfigInvalid   Code = "SQLETCH301" // sqletch.yaml field validation
	CodeExpansionLarge  Code = "SQLETCH302" // static expansion exceeds max_shapes
	CodePolicyInvalid   Code = "SQLETCH303" // a policy declaration is malformed
	CodeShapeCapReached Code = "SQLETCH304" // shape enumeration stopped at its cap (explain --max-shapes, verification.max_shapes)
	CodePathEscape      Code = "SQLETCH306" // cache.path/output.path escapes the project directory
	// A result column's name does not form a valid Go identifier once
	// mapped (an oracle column or quoted alias carrying spaces, braces,
	// punctuation, …): emitting it verbatim would either fail gofmt with
	// no span or, worse, splice attacker-influenced text into the
	// generated package. Refuse it and ask for an `AS` alias / `-- @column`.
	CodeInvalidColumnIdentifier Code = "SQLETCH307"
	CodeSourceUnreadable        Code = "SQLETCH308" // a glob-matched template file could not be read (LSP degrades past it)
	CodeNameCollision           Code = "SQLETCH310" // generated Go identifiers collide
	CodeUnsupportedType         Code = "SQLETCH311" // no Go mapping for a database type
	// null_overrides hygiene: overrides are the analyzer's escape
	// hatch and are applied by RESULT-COLUMN NAME, so a key that
	// matches nothing is dead config and a key matching several
	// same-named columns forces all of them at once.
	CodeOverrideUnknownColumn   Code = "SQLETCH312" // null_overrides names no result column of the query
	CodeOverrideAmbiguousColumn Code = "SQLETCH313" // null_overrides matches multiple same-named result columns
)

type Diagnostic struct {
	Code     Code
	Severity Severity
	Span     Span
	Message  string
	Hint     string
}

func Errorf(code Code, span Span, format string, args ...any) Diagnostic {
	return Diagnostic{Code: code, Severity: Error, Span: span, Message: fmt.Sprintf(format, args...)}
}

func Warnf(code Code, span Span, format string, args ...any) Diagnostic {
	return Diagnostic{Code: code, Severity: Warning, Span: span, Message: fmt.Sprintf(format, args...)}
}

func (d Diagnostic) WithHint(format string, args ...any) Diagnostic {
	d.Hint = fmt.Sprintf(format, args...)
	return d
}

// HasErrors reports whether any diagnostic is of Error severity.
func HasErrors(diags []Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == Error {
			return true
		}
	}
	return false
}

// Sort orders diagnostics by (file, offset, code) for stable output.
func Sort(diags []Diagnostic) {
	sort.SliceStable(diags, func(i, j int) bool {
		a, b := diags[i], diags[j]
		if a.Span.File != b.Span.File {
			return a.Span.File < b.Span.File
		}
		if a.Span.Start != b.Span.Start {
			return a.Span.Start < b.Span.Start
		}
		return a.Code < b.Code
	})
}

// Render produces the human-readable one-line form
// "file:line:col: error[CODE]: message". The full excerpt renderer
// arrives in P7; this form is enough for tests and early CLI output.
func (d Diagnostic) Render(src []byte) string {
	return d.RenderWith(src, NewLineMap(src))
}

// RenderWith is Render reusing a precomputed line index. Rendering a
// batch of diagnostics for one file builds the index once (O(n)) and
// then locates each diagnostic in O(log lines) instead of O(offset) —
// see PrintDiags. Output is byte-identical to Render.
func (d Diagnostic) RenderWith(src []byte, lm *LineMap) string {
	line, col := lm.LineCol(d.Span.Start)
	var b strings.Builder
	fmt.Fprintf(&b, "%s:%d:%d: %s[%s]: %s", d.Span.File, line, col, d.Severity, d.Code, d.Message)
	if d.Hint != "" {
		fmt.Fprintf(&b, "\nhelp: %s", d.Hint)
	}
	return b.String()
}

// RenderExcerpt produces the multi-line form with a source excerpt and
// caret underline (design 07 §3):
//
//	file:12:7: error[SQLETCHnnn]: message
//	   |
//	12 |   AND organization_id = :org
//	   |       ^^^^^^^^^^^^^^^
//	help: …
func (d Diagnostic) RenderExcerpt(src []byte) string {
	return d.RenderExcerptWith(src, NewLineMap(src))
}

// RenderExcerptWith is RenderExcerpt reusing a precomputed line index
// (see RenderWith / PrintDiags). Output is byte-identical to
// RenderExcerpt.
func (d Diagnostic) RenderExcerptWith(src []byte, lm *LineMap) string {
	if len(src) == 0 || d.Span.Start >= len(src) {
		return d.RenderWith(src, lm)
	}
	line, _ := lm.LineCol(d.Span.Start)
	lineStart := d.Span.Start
	for lineStart > 0 && src[lineStart-1] != '\n' {
		lineStart--
	}
	lineEnd := d.Span.Start
	for lineEnd < len(src) && src[lineEnd] != '\n' {
		lineEnd++
	}
	spanEnd := d.Span.End
	if spanEnd > lineEnd || spanEnd <= d.Span.Start {
		spanEnd = d.Span.Start + 1
	}
	capEnd := min(lineEnd, spanEnd)

	lineText, prefixRunes, caretRunes := excerptLine(src, lineStart, lineEnd, d.Span.Start, capEnd)

	gutter := fmt.Sprintf("%d", line)
	pad := strings.Repeat(" ", len(gutter))
	var b strings.Builder
	lineNo, col := lm.LineCol(d.Span.Start)
	fmt.Fprintf(&b, "%s:%d:%d: %s[%s]: %s\n", d.Span.File, lineNo, col, d.Severity, d.Code, d.Message)
	fmt.Fprintf(&b, "%s |\n", pad)
	fmt.Fprintf(&b, "%s | %s\n", gutter, lineText)
	fmt.Fprintf(&b, "%s | %s%s", pad, strings.Repeat(" ", prefixRunes), strings.Repeat("^", caretRunes))
	if d.Hint != "" {
		fmt.Fprintf(&b, "\nhelp: %s", d.Hint)
	}
	return b.String()
}

const (
	// A line at or below this many bytes renders in full, exactly as
	// before. Past it the excerpt is windowed around the span: a single
	// pathological line (e.g. a 1 MB one-liner) otherwise made each
	// diagnostic copy the whole line and rune-count the whole prefix —
	// O(line) per diagnostic, gigabytes of output across many.
	excerptFullMaxBytes = 320
	// Context runes shown on each side of the span in a windowed excerpt.
	excerptCtxRunes = 48
	// Longest caret underline in a windowed excerpt; a span covering a
	// huge line is shown truncated rather than underlined end to end.
	excerptMaxCaretRunes = 160
)

// excerptLine returns the excerpt line text, the rune count before the
// caret (left padding), and the caret length in runes. For lines up to
// excerptFullMaxBytes it is byte-identical to the original full-line
// rendering; longer lines are windowed around [start,capEnd] with
// ellipses so both the work and the output stay bounded.
func excerptLine(src []byte, lineStart, lineEnd, start, capEnd int) (text string, prefixRunes, caretRunes int) {
	if lineEnd-lineStart <= excerptFullMaxBytes {
		text = string(src[lineStart:lineEnd])
		prefixRunes = utf8.RuneCount(src[lineStart:start])
		caretRunes = max(utf8.RuneCount(src[start:capEnd]), 1)
		return text, prefixRunes, caretRunes
	}

	// Windowed: never rune-count or copy beyond the window, so cost is
	// O(excerptCtxRunes + excerptMaxCaretRunes) regardless of line length.
	caretEnd := fwdRunes(src, start, capEnd, excerptMaxCaretRunes)
	winStart := backRunes(src, start, lineStart, excerptCtxRunes)
	winEnd := fwdRunes(src, caretEnd, lineEnd, excerptCtxRunes)

	var b strings.Builder
	if winStart > lineStart {
		b.WriteRune('…')
		prefixRunes++
	}
	b.Write(src[winStart:winEnd])
	if winEnd < lineEnd {
		b.WriteRune('…')
	}
	text = b.String()
	prefixRunes += utf8.RuneCount(src[winStart:start])
	caretRunes = max(utf8.RuneCount(src[start:caretEnd]), 1)
	return text, prefixRunes, caretRunes
}

// backRunes returns the byte offset n runes before pos, not crossing lo.
// It is bounds-safe on invalid UTF-8 (stray continuation bytes only make
// it stop sooner).
func backRunes(src []byte, pos, lo, n int) int {
	for pos > lo && n > 0 {
		pos--
		for pos > lo && src[pos]&0xC0 == 0x80 {
			pos--
		}
		n--
	}
	return pos
}

// fwdRunes returns the byte offset n runes after pos, not crossing hi.
func fwdRunes(src []byte, pos, hi, n int) int {
	for pos < hi && n > 0 {
		pos++
		for pos < hi && src[pos]&0xC0 == 0x80 {
			pos++
		}
		n--
	}
	return pos
}

// LineCol converts a byte offset into 1-based line and column
// (column counts bytes; rune-aware columns arrive with the P7
// renderer). It builds a one-shot line index; callers rendering many
// diagnostics for the same source should build a LineMap once and reuse
// it (see PrintDiags) to avoid O(offset) per lookup.
func LineCol(src []byte, off int) (line, col int) {
	return NewLineMap(src).LineCol(off)
}

// LineMap is a precomputed index of line-start byte offsets for one
// source, so byte offset → (line, col) is an O(log lines) binary search
// instead of an O(offset) rescan. Rendering d diagnostics for an n-byte
// file drops from O(n·d) to O(n + d·log lines) by building it once.
type LineMap struct {
	// starts[k] is the byte offset at which line k+1 begins; starts[0]
	// is always 0. A '\n' at index j starts a new line at j+1.
	starts []int
	n      int // len(src), for clamping an offset past EOF
}

// NewLineMap builds the line index for src.
func NewLineMap(src []byte) *LineMap {
	starts := []int{0}
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return &LineMap{starts: starts, n: len(src)}
}

// LineCol returns the 1-based line and (byte-)column of off. It is
// byte-identical to the previous linear implementation: an offset past
// EOF clamps to EOF, the column counts bytes since the last '\n', and a
// '\n' byte itself belongs to the line it terminates.
func (m *LineMap) LineCol(off int) (line, col int) {
	if off > m.n {
		off = m.n
	}
	if off < 0 {
		off = 0
	}
	idx := sort.Search(len(m.starts), func(i int) bool { return m.starts[i] > off }) - 1
	return idx + 1, off - m.starts[idx] + 1
}

// LineStartOffset returns the byte offset at which the 0-based line
// begins. A line index at or past the last line clamps to len(src), so
// a position past EOF resolves to EOF — matching the previous linear
// walk that returned len(src) when the requested line ran off the end.
func (m *LineMap) LineStartOffset(line0 int) int {
	if line0 < 0 {
		line0 = 0
	}
	if line0 >= len(m.starts) {
		return m.n
	}
	return m.starts[line0]
}
