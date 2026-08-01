// Package gosrc extracts template text from Go source files.
//
// A template may be authored in a `//sqletch:query` const instead of a
// .sql file (docs/design/13-go-source-input.md). Extraction is purely
// syntactic — go/parser only, never go/types — so the target package
// need not compile; it may reference generated symbols that do not
// exist yet.
//
// The unit handed back is not a substring but a *view*: a copy of the
// whole file with everything outside one template literal blanked and
// the tail truncated. Byte offsets and line numbers therefore match
// the real file exactly, which is what lets every downstream phase —
// the scanner, the source map, diagnostics, the LSP's UTF-16
// conversion — work unmodified on Go-authored templates.
package gosrc

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"path/filepath"
	"strings"

	"github.com/moznion/sqletch/internal/diagnostics"
)

// marker opts a const into template extraction. Spelled as a Go
// directive comment (no space after the slashes) so gofmt keeps it
// glued to its declaration.
const marker = "//sqletch:query"

// IsGoSource reports whether path is read as Go source rather than as
// a template file. This is the only dispatch: `queries:` globs carry
// both forms.
func IsGoSource(path string) bool { return filepath.Ext(path) == ".go" }

// Views returns one offset-preserving view per marked template
// literal, in source order, plus diagnostics for markers that do not
// name an extractable template.
func Views(path string, src []byte) ([][]byte, []diagnostics.Diagnostic) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, []diagnostics.Diagnostic{parseDiag(path, src, err)}
	}

	e := extractor{path: path, src: src, fset: fset}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if hasMarker(d.Doc) {
				e.rejectTarget(d.Pos(), "func", "a func declaration")
			}
		case *ast.GenDecl:
			e.genDecl(d)
		}
	}
	return e.views, e.diags
}

type extractor struct {
	path  string
	src   []byte
	fset  *token.FileSet
	views [][]byte
	diags []diagnostics.Diagnostic
}

func (e *extractor) genDecl(d *ast.GenDecl) {
	marked := hasMarker(d.Doc)
	if marked && d.Tok != token.CONST {
		e.rejectTarget(d.Pos(), d.Tok.String(), "a "+d.Tok.String()+" declaration")
		return
	}
	if d.Tok != token.CONST {
		return
	}
	for _, spec := range d.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		// A marker on the declaration covers every spec in the block;
		// a marker on the spec covers just that one.
		if !marked && !hasMarker(vs.Doc) {
			continue
		}
		e.valueSpec(vs)
	}
}

func (e *extractor) valueSpec(vs *ast.ValueSpec) {
	// `const ( a = iota; b )` and `const a, b = f()` have no
	// one-name-one-literal reading, so there is nothing to extract.
	if len(vs.Values) == 0 || len(vs.Values) != len(vs.Names) {
		e.diags = append(e.diags, diagnostics.Errorf(diagnostics.CodeGoBadConstSpec,
			e.span(vs.Pos(), vs.End()),
			"%s requires a const that declares exactly one value per name", marker).
			WithHint("write `%s` above a const whose value is a single raw string literal", marker))
		return
	}
	for _, v := range vs.Values {
		lit, ok := v.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || !strings.HasPrefix(lit.Value, "`") {
			e.diags = append(e.diags, diagnostics.Errorf(diagnostics.CodeGoNotRawString,
				e.span(v.Pos(), v.End()),
				"a %s const must be a single raw string literal: interpreted strings process escapes and concatenations have no contiguous source range, so template spans could not point back at this file", marker).
				WithHint("wrap the template in backquotes: const q = `\n-- name: … `"))
			continue
		}
		e.views = append(e.views, e.view(lit))
	}
}

// view copies the file with everything before the literal's contents
// blanked (newlines kept, so line numbers survive) and the tail cut at
// the literal's end, so a query's extent cannot run past it into the
// surrounding Go code.
func (e *extractor) view(lit *ast.BasicLit) []byte {
	// Bounds come from the FileSet, not len(lit.Value): go/scanner
	// strips carriage returns out of a raw literal's value, so the
	// value can be shorter than the source it came from.
	start := e.offset(lit.Pos()) + 1
	end := e.offset(lit.End()) - 1
	if end < start {
		end = start
	}
	buf := make([]byte, end)
	for i := 0; i < start && i < len(buf); i++ {
		if e.src[i] == '\n' {
			buf[i] = '\n'
			continue
		}
		buf[i] = ' '
	}
	copy(buf[start:end], e.src[start:end])
	return buf
}

func (e *extractor) rejectTarget(pos token.Pos, keyword, what string) {
	off := e.offset(pos)
	e.diags = append(e.diags, diagnostics.Errorf(diagnostics.CodeGoMarkerTarget,
		e.spanOff(off, off+len(keyword)),
		"%s applies to const declarations, not %s: a const is what makes the template text immutable, and with it the guarantee that what was verified is what runs", marker, what).
		WithHint("declare the template as `const … = ` + \"`…`\""))
}

func (e *extractor) offset(pos token.Pos) int { return e.fset.Position(pos).Offset }

func (e *extractor) span(start, end token.Pos) diagnostics.Span {
	return e.spanOff(e.offset(start), e.offset(end))
}

// spanOff clamps like the scanner's span constructor: consumers index
// the source with these, so in-bounds is a structural invariant.
func (e *extractor) spanOff(start, end int) diagnostics.Span {
	n := len(e.src)
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
	return diagnostics.Span{File: e.path, Start: start, End: end}
}

func parseDiag(path string, src []byte, err error) diagnostics.Diagnostic {
	off, msg := 0, err.Error()
	var el scanner.ErrorList
	if errors.As(err, &el) && len(el) > 0 {
		off, msg = el[0].Pos.Offset, el[0].Msg
	}
	if off > len(src) {
		off = len(src)
	}
	end := off + 1
	if end > len(src) {
		end = len(src)
	}
	return diagnostics.Errorf(diagnostics.CodeGoParse,
		diagnostics.Span{File: path, Start: off, End: end},
		"cannot read templates from this file: %s", msg).
		WithHint("sqletch parses Go sources listed in `queries:` syntactically; fix the syntax error and re-run")
}

func hasMarker(doc *ast.CommentGroup) bool {
	if doc == nil {
		return false
	}
	for _, c := range doc.List {
		if strings.TrimRight(c.Text, " \t") == marker {
			return true
		}
	}
	return false
}
