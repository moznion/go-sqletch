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

// Rules-phase codes (R1 runs in P2's pipeline position; see
// docs/design/02-rendering.md and 03-structural-rules.md).
const (
	CodeRenderingParse    Code = "SQLETCH100" // a rendering fails to parse
	CodeJoinTypeForbidden Code = "SQLETCH101" // optional join not INNER/LEFT (R2)
	CodeNodeIncomplete    Code = "SQLETCH102" // fragment is not one complete node (R1)
	CodeNotSingleDML      Code = "SQLETCH103" // not exactly one SELECT/UPDATE/INSERT/DELETE
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
