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

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// marker opts a const into template extraction. Spelled as a Go
// directive comment (no space after the slashes) so gofmt keeps it
// glued to its declaration.
const marker = "//sqletch:query"

// IsGoSource reports whether path is read as Go source rather than as
// a template file. This is the only dispatch: `queries:` globs carry
// both forms.
func IsGoSource(path string) bool { return filepath.Ext(path) == ".go" }

// Views calls yield once per marked template literal, in source order,
// with an offset-preserving view of the file, and returns diagnostics
// for markers that do not name an extractable template.
//
// The []byte handed to yield is a single backing buffer reused across
// every view — it is only valid until yield returns. This keeps total
// memory O(len(src)) instead of O(literals × len(src)): a file with
// thousands of marked consts would otherwise allocate a full
// file-length prefix copy per const and exhaust memory (and, through
// cli.scanSource, take the LSP down on open). Reuse is sound because
// the scanner copies out everything it retains — skeleton and body
// texts are string copies and spans are byte offsets, never slices
// into the source — so nothing observes the buffer after yield returns.
func Views(path string, src []byte, yield func(view []byte)) []diagnostics.Diagnostic {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return []diagnostics.Diagnostic{parseDiag(path, src, err)}
	}

	e := extractor{path: path, src: src, fset: fset, yield: yield}
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
	return e.diags
}

type extractor struct {
	path  string
	src   []byte
	fset  *token.FileSet
	yield func(view []byte)
	buf   []byte // reused view buffer; blank outside a live literal
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
		e.view(lit)
	}
}

// view hands yield a buffer holding the file with everything before the
// literal's contents blanked (newlines kept, so line numbers survive)
// and the tail cut at the literal's end, so a query's extent cannot run
// past it into the surrounding Go code. The buffer is shared across
// views and re-blanked after each yield (see Views).
func (e *extractor) view(lit *ast.BasicLit) {
	// Bounds come from the FileSet, not len(lit.Value): go/scanner
	// strips carriage returns out of a raw literal's value, so the
	// value can be shorter than the source it came from.
	start := e.offset(lit.Pos()) + 1
	end := max(e.offset(lit.End())-1, start)
	if e.buf == nil {
		// Blank the whole file once: every byte becomes a space except
		// newlines, which survive to keep line numbers. Allocated lazily
		// so a file with no marked consts costs nothing.
		e.buf = make([]byte, len(e.src))
		blank(e.buf, e.src, 0, len(e.src))
	}
	// Splice the literal's real bytes in at their true offsets, hand the
	// truncated view to yield, then restore just that region to blanks
	// so the next view sees a clean prefix. Only O(literal) work per
	// view, so the whole file costs O(len(src)) regardless of count.
	copy(e.buf[start:end], e.src[start:end])
	e.yield(e.buf[:end])
	blank(e.buf, e.src, start, end)
}

// blank sets dst[i] to src[i] where it is a newline and to a space
// otherwise, for i in [lo, hi), so line numbers survive but no Go
// source outside a template literal reaches the scanner.
func blank(dst, src []byte, lo, hi int) {
	for i := lo; i < hi; i++ {
		if src[i] == '\n' {
			dst[i] = '\n'
		} else {
			dst[i] = ' '
		}
	}
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
	end := min(off+1, len(src))
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
