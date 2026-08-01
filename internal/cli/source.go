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
	views, diags := gosrc.Views(path, src)
	file := &template.QueryFile{Path: path}
	for _, v := range views {
		f, ds := sc.ScanFile(path, v)
		file.Queries = append(file.Queries, f.Queries...)
		diags = append(diags, ds...)
	}
	diagnostics.Sort(diags)
	return file, diags
}
