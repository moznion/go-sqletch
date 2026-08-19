package cli

import (
	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/gosrc"
	"github.com/moznion/go-sqletch/internal/template"
)

// scanSource turns one file listed in `queries:` into scanned queries.
//
// A .sql file is the template. A .go file holds its templates in
// `//sqletch:query` consts, and is split into one offset-preserving
// view per const (docs/design/13-go-source-input.md) — the scanner sees
// an ordinary template whose spans happen to index the .go file.
//
// Both the generate/check pipeline and the LSP's OfflineChecker go
// through here so the two can never disagree about what a file
// contains.
func scanSource(sc *template.Scanner, path string, src []byte) (*template.QueryFile, []diagnostics.Diagnostic) {
	if !gosrc.IsGoSource(path) {
		return sc.ScanFile(path, src)
	}
	file := &template.QueryFile{Path: path}
	var diags []diagnostics.Diagnostic
	// The view is only valid inside the callback — gosrc reuses one
	// backing buffer across views to stay O(file size). The scanner
	// keeps only copies, so scanning eagerly here is safe.
	extractDiags := gosrc.Views(path, src, func(v []byte) {
		f, ds := sc.ScanFile(path, v)
		file.Queries = append(file.Queries, f.Queries...)
		diags = append(diags, ds...)
	})
	diags = append(diags, extractDiags...)
	diagnostics.Sort(diags)
	return file, diags
}
