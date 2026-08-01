// Package diagnostics defines the user-facing error model shared by
// every compiler phase. All user mistakes surface as Diagnostics with
// stable codes; bare Go errors are reserved for environmental failures.
package diagnostics

import (
	"fmt"
	"sort"
	"strings"
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
)

// Oracle-phase codes (see docs/design/04-type-oracle.md).
const (
	CodeServerVersionMismatch Code = "SQLETCH200" // pinned version != connected server
	CodeIndeterminateParam    Code = "SQLETCH201" // undetermined parameter type (add a cast)
	CodeOracleFailure         Code = "SQLETCH202" // prepare/describe failed
	CodeColumnAgreement       Code = "SQLETCH210" // renderings disagree on result columns
	CodeParamAgreement        Code = "SQLETCH211" // renderings disagree on a param's type
	CodeOptionalInsertNotNull Code = "SQLETCH212" // optional NOT NULL column without default (warning)
	CodeParamHintConflict     Code = "SQLETCH213" // `-- @param` hint disagrees with the oracle (Tier 1)
)

// Codegen/config codes.
const (
	CodeConfigParse     Code = "SQLETCH300" // sqletch.yaml unreadable/unknown keys
	CodeConfigInvalid   Code = "SQLETCH301" // sqletch.yaml field validation
	CodeExpansionLarge  Code = "SQLETCH302" // static expansion exceeds max_shapes
	CodeNameCollision   Code = "SQLETCH310" // generated Go identifiers collide
	CodeUnsupportedType Code = "SQLETCH311" // no Go mapping for a database type
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
	line, col := LineCol(src, d.Span.Start)
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
	if len(src) == 0 || d.Span.Start >= len(src) {
		return d.Render(src)
	}
	line, _ := LineCol(src, d.Span.Start)
	lineStart := d.Span.Start
	for lineStart > 0 && src[lineStart-1] != '\n' {
		lineStart--
	}
	lineEnd := d.Span.Start
	for lineEnd < len(src) && src[lineEnd] != '\n' {
		lineEnd++
	}
	lineText := string(src[lineStart:lineEnd])

	// Caret geometry in runes, keeping alignment for multibyte text.
	prefixRunes := len([]rune(string(src[lineStart:d.Span.Start])))
	spanEnd := d.Span.End
	if spanEnd > lineEnd || spanEnd <= d.Span.Start {
		spanEnd = d.Span.Start + 1
	}
	capEnd := min(lineEnd, spanEnd)
	caretRunes := max(len([]rune(string(src[d.Span.Start:capEnd]))), 1)

	gutter := fmt.Sprintf("%d", line)
	pad := strings.Repeat(" ", len(gutter))
	var b strings.Builder
	lineNo, col := LineCol(src, d.Span.Start)
	fmt.Fprintf(&b, "%s:%d:%d: %s[%s]: %s\n", d.Span.File, lineNo, col, d.Severity, d.Code, d.Message)
	fmt.Fprintf(&b, "%s |\n", pad)
	fmt.Fprintf(&b, "%s | %s\n", gutter, lineText)
	fmt.Fprintf(&b, "%s | %s%s", pad, strings.Repeat(" ", prefixRunes), strings.Repeat("^", caretRunes))
	if d.Hint != "" {
		fmt.Fprintf(&b, "\nhelp: %s", d.Hint)
	}
	return b.String()
}

// LineCol converts a byte offset into 1-based line and column
// (column counts bytes; rune-aware columns arrive with the P7
// renderer).
func LineCol(src []byte, off int) (line, col int) {
	if off > len(src) {
		off = len(src)
	}
	line, col = 1, 1
	for i := 0; i < off; i++ {
		if src[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}
