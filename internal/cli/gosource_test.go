package cli

import (
	"strings"
	"testing"

	"github.com/moznion/sqletch/internal/diagnostics"
)

// goProject spells backquotes as '~' so a Go fixture can carry a raw
// string literal.
func goProject(t *testing.T, gosrc string) (cfgFiles map[string]string) {
	t.Helper()
	return map[string]string{
		"sqletch.yaml": `version: 1
dialect: postgres
server_version: "16"
schema:
  files: [db/schema.sql]
queries: [repo/*.go]
output:
  package: gen
  path: gen
`,
		"repo/users.go": strings.ReplaceAll(gosrc, "~", "`"),
	}
}

const goRepoSource = `package repo

//sqletch:query
const findTSQL = ~
-- name: FindT :many
SELECT t.id FROM t
WHERE TRUE
@if-present(x)
  AND t.x = :x
@endif
;
~

// Ordinary code around the template must not reach the scanner.
func Unrelated() string { return "-- name: NotAQuery :one" }
`

// A template authored in a .go file must scan into exactly the same
// queries as the equivalent .sql file.
func TestOfflineCheckGoSource(t *testing.T) {
	cfg := writeOfflineProject(t, goProject(t, goRepoSource))
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.Abs("repo/users.go")
	if diags := res.Diags[path]; len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	file := res.Files[path]
	if file == nil || len(file.Queries) != 1 {
		t.Fatalf("got %v queries, want exactly FindT", file)
	}
	if got := file.Queries[0].Name; got != "FindT" {
		t.Errorf("query name = %q, want FindT", got)
	}
	// The Go code after the literal is truncated away, so its
	// header-shaped comment is invisible to the scanner.
	if len(file.Queries[0].Params) != 1 {
		t.Errorf("params = %v, want just :x", file.Queries[0].Params)
	}
}

// The point of the offset-preserving view: a template diagnostic must
// name the .go line the template text really occupies.
func TestGoSourceDiagnosticsPointAtGoLines(t *testing.T) {
	// R6: every WHERE conjunct optional, no anchor (SQLETCH113).
	const bad = `package repo

//sqletch:query
const badSQL = ~
-- name: BadAnchor :many
SELECT t.id FROM t
WHERE
@if-present(x)
  AND t.x = :x
@endif
;
~
`
	cfg := writeOfflineProject(t, goProject(t, bad))
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.Abs("repo/users.go")
	diags := res.Diags[path]
	if !hasCode(diags, diagnostics.CodeUnanchoredClause) {
		t.Fatalf("want SQLETCH113, got %v", diags)
	}

	src := res.Sources[path]
	if !strings.HasPrefix(string(src), "package repo") {
		t.Fatal("Sources must hold the original Go file, not a view")
	}
	for _, d := range diags {
		if d.Code != diagnostics.CodeUnanchoredClause {
			continue
		}
		if d.Span.Start >= len(src) {
			t.Fatalf("span %v out of bounds for the .go file", d.Span)
		}
		line, _ := diagnostics.LineCol(src, d.Span.Start)
		// R6 anchors at the offending conjunct, which is line 8 of this
		// Go file (1-indexed): package, blank, marker, const, header,
		// SELECT, WHERE, @if-present.
		wantLine := 8
		if line != wantLine {
			t.Errorf("diagnostic at .go line %d, want %d\n%s", line, wantLine,
				d.RenderExcerpt(src))
		}
		if !strings.Contains(d.RenderExcerpt(src), "@if-present") {
			t.Errorf("excerpt should show the offending Go line:\n%s", d.RenderExcerpt(src))
		}
	}
}

// Extraction rejections surface through the same path as template
// diagnostics, so a malformed marker is reported, not silently skipped.
func TestGoSourceExtractionDiagnostics(t *testing.T) {
	const bad = `package repo

//sqletch:query
var notAConst = ~
-- name: A :one
SELECT 1;
~
`
	cfg := writeOfflineProject(t, goProject(t, bad))
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.Abs("repo/users.go")
	if !hasCode(res.Diags[path], diagnostics.CodeGoMarkerTarget) {
		t.Fatalf("want SQLETCH021, got %v", res.Diags[path])
	}
	if len(res.Files[path].Queries) != 0 {
		t.Error("a rejected marker must contribute no queries")
	}
}

// Two templates in one .go file are independent: the first must not
// absorb the second, and both must be found.
func TestGoSourceMultipleConsts(t *testing.T) {
	const two = `package repo

//sqletch:query
const aSQL = ~
-- name: QA :many
SELECT t.id FROM t;
~

func between() {}

//sqletch:query
const bSQL = ~
-- name: QB :many
SELECT u.id FROM u;
~
`
	cfg := writeOfflineProject(t, goProject(t, two))
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.Abs("repo/users.go")
	if diags := res.Diags[path]; len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	var names []string
	for _, q := range res.Files[path].Queries {
		names = append(names, q.Name)
	}
	if strings.Join(names, ",") != "QA,QB" {
		t.Errorf("queries = %v, want [QA QB] in source order", names)
	}
}

// Duplicate names across the two forms are still caught, and the .sql
// and .go inputs coexist in one project.
func TestGoSourceCoexistsWithSQLFiles(t *testing.T) {
	files := goProject(t, goRepoSource)
	files["sqletch.yaml"] = `version: 1
dialect: postgres
server_version: "16"
schema:
  files: [db/schema.sql]
queries: [queries/*.sql, repo/*.go]
output:
  package: gen
  path: gen
`
	files["queries/other.sql"] = `-- name: FindU :many
SELECT u.id FROM u;
`
	cfg := writeOfflineProject(t, files)
	res, err := NewOfflineChecker(cfg).Check(nil)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, f := range res.Files {
		total += len(f.Queries)
	}
	if total != 2 {
		t.Errorf("got %d queries across both input forms, want 2", total)
	}
	for p, diags := range res.Diags {
		if len(diags) != 0 {
			t.Errorf("%s: unexpected diagnostics: %v", p, diags)
		}
	}
}
