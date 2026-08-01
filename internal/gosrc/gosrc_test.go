package gosrc

import (
	"strings"
	"testing"

	"github.com/moznion/sqletch/internal/diagnostics"
)

// bt spells backquotes as '~' so fixtures can nest raw literals.
func bt(s string) []byte { return []byte(strings.ReplaceAll(s, "~", "`")) }

func codes(ds []diagnostics.Diagnostic) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, string(d.Code))
	}
	return out
}

func TestIsGoSource(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"queries/users.sql", false},
		{"repo/users.go", true},
		{"repo/users_test.go", true},
		{"repo/users.GO", false}, // extensions are matched exactly
		{"go", false},
	} {
		if got := IsGoSource(tc.path); got != tc.want {
			t.Errorf("IsGoSource(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

const oneQuery = `package repo

//sqletch:query
const searchUsersSQL = ~
-- name: SearchUsers :many
SELECT id FROM users
~

func unrelated() string { return "SELECT 1" }
`

// The view is the load-bearing property: byte offsets and line numbers
// must be identical to the real .go file, so every span a downstream
// phase produces indexes the original source correctly.
func TestViewsPreserveOffsetsAndLines(t *testing.T) {
	src := bt(oneQuery)
	views, diags := Views("repo/users.go", src)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", codes(diags))
	}
	if len(views) != 1 {
		t.Fatalf("got %d views, want 1", len(views))
	}
	v := views[0]

	body := "\n-- name: SearchUsers :many\nSELECT id FROM users\n"
	start := strings.Index(string(src), body)
	if start < 0 {
		t.Fatal("fixture body not found")
	}
	end := start + len(body)

	if len(v) != end {
		t.Errorf("view length = %d, want %d (truncated at the literal's end)", len(v), end)
	}
	if got := string(v[start:end]); got != body {
		t.Errorf("template bytes = %q, want %q", got, body)
	}
	for i := range start {
		want := byte(' ')
		if src[i] == '\n' {
			want = '\n'
		}
		if v[i] != want {
			t.Fatalf("prefix byte %d = %q, want %q (newlines kept, everything else blanked)", i, v[i], want)
		}
	}

	// Line/col of the header must agree between view and original.
	off := strings.Index(string(src), "-- name:")
	wantLine, wantCol := diagnostics.LineCol(src, off)
	gotLine, gotCol := diagnostics.LineCol(v, off)
	if gotLine != wantLine || gotCol != wantCol {
		t.Errorf("LineCol in view = %d:%d, want %d:%d", gotLine, gotCol, wantLine, wantCol)
	}
}

func TestViewsMultipleLiterals(t *testing.T) {
	src := bt(`package repo

//sqletch:query
const aSQL = ~
-- name: A :one
SELECT 1
~

//sqletch:query
const bSQL = ~
-- name: B :one
SELECT 2
~
`)
	views, diags := Views("repo/users.go", src)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", codes(diags))
	}
	if len(views) != 2 {
		t.Fatalf("got %d views, want 2", len(views))
	}
	// Source order, and neither view absorbs the other's text.
	if !strings.Contains(string(views[0]), "-- name: A") || strings.Contains(string(views[0]), "-- name: B") {
		t.Errorf("view 0 should hold only query A")
	}
	if !strings.Contains(string(views[1]), "-- name: B") || strings.Contains(string(views[1]), "-- name: A") {
		t.Errorf("view 1 should hold only query B")
	}
	if len(views[0]) >= len(views[1]) {
		t.Errorf("views should be truncated at their own literal: %d, %d", len(views[0]), len(views[1]))
	}
}

func TestViewsConstBlockMarker(t *testing.T) {
	src := bt(`package repo

//sqletch:query
const (
	aSQL = ~
-- name: A :one
SELECT 1
~
	bSQL = ~
-- name: B :one
SELECT 2
~
)
`)
	views, diags := Views("repo/users.go", src)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", codes(diags))
	}
	if len(views) != 2 {
		t.Fatalf("got %d views, want 2 (a block marker covers every spec)", len(views))
	}
}

func TestViewsSpecMarkerInsideBlock(t *testing.T) {
	src := bt(`package repo

const (
	plain = "not a template"

	//sqletch:query
	aSQL = ~
-- name: A :one
SELECT 1
~
)
`)
	views, diags := Views("repo/users.go", src)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", codes(diags))
	}
	if len(views) != 1 {
		t.Fatalf("got %d views, want 1", len(views))
	}
}

// Opting a .go file into the queries globs must not turn every
// backquoted string in it into a template.
func TestViewsUnmarkedConstIgnored(t *testing.T) {
	src := bt(`package repo

const notATemplate = ~
-- name: Nope :one
SELECT 1
~
`)
	views, diags := Views("repo/users.go", src)
	if len(views) != 0 || len(diags) != 0 {
		t.Fatalf("got %d views, %v diags; want none", len(views), codes(diags))
	}
}

func TestViewsRejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want diagnostics.Code
	}{
		{
			name: "var declaration",
			src: `package repo

//sqletch:query
var searchSQL = ~
-- name: A :one
SELECT 1
~
`,
			want: diagnostics.CodeGoMarkerTarget,
		},
		{
			name: "func declaration",
			src: `package repo

//sqletch:query
func f() {}
`,
			want: diagnostics.CodeGoMarkerTarget,
		},
		{
			name: "type declaration",
			src: `package repo

//sqletch:query
type T struct{}
`,
			want: diagnostics.CodeGoMarkerTarget,
		},
		{
			name: "interpreted string",
			src: `package repo

//sqletch:query
const aSQL = "-- name: A :one\nSELECT 1\n"
`,
			want: diagnostics.CodeGoNotRawString,
		},
		{
			name: "concatenation",
			src: `package repo

//sqletch:query
const aSQL = ~
-- name: A :one
~ + ~SELECT 1
~
`,
			want: diagnostics.CodeGoNotRawString,
		},
		{
			name: "identifier value",
			src: `package repo

const other = ~SELECT 1~

//sqletch:query
const aSQL = other
`,
			want: diagnostics.CodeGoNotRawString,
		},
		{
			// Parseable but not type-correct — proof that extraction is
			// syntactic and never type-checks the target package.
			name: "more names than values",
			src: `package repo

//sqletch:query
const a, b = ~SELECT 1~
`,
			want: diagnostics.CodeGoBadConstSpec,
		},
		{
			name: "unparseable",
			src: `package repo

func f( {
`,
			want: diagnostics.CodeGoParse,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			views, diags := Views("repo/users.go", bt(tc.src))
			if len(views) != 0 {
				t.Errorf("got %d views, want 0", len(views))
			}
			if len(diags) == 0 {
				t.Fatalf("want %s, got no diagnostics", tc.want)
			}
			if diags[0].Code != tc.want {
				t.Fatalf("got %v, want %s", codes(diags), tc.want)
			}
			if diags[0].Span.File != "repo/users.go" {
				t.Errorf("span file = %q, want the .go path", diags[0].Span.File)
			}
			if diags[0].Hint == "" {
				t.Error("rejection diagnostics must hint the compliant rewrite")
			}
		})
	}
}

// A block marker reaches every spec, so an iota-style block reports
// per spec: `a = iota` is not a raw string, `b` has no value at all.
func TestViewsMarkedIotaBlock(t *testing.T) {
	src := bt(`package repo

//sqletch:query
const (
	a = iota
	b
)
`)
	views, diags := Views("repo/users.go", src)
	if len(views) != 0 {
		t.Fatalf("got %d views, want 0", len(views))
	}
	got := codes(diags)
	want := []string{string(diagnostics.CodeGoNotRawString), string(diagnostics.CodeGoBadConstSpec)}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("codes = %v, want %v", got, want)
	}
}

// A rejection span must point into the offending declaration so the
// excerpt renderer underlines real Go source.
func TestRejectionSpanPointsAtDeclaration(t *testing.T) {
	src := bt(`package repo

//sqletch:query
var searchSQL = ~
-- name: A :one
~
`)
	_, diags := Views("repo/users.go", src)
	if len(diags) != 1 {
		t.Fatalf("got %v, want exactly one diagnostic", codes(diags))
	}
	sp := diags[0].Span
	if sp.Start < 0 || sp.End > len(src) || sp.Start >= sp.End {
		t.Fatalf("span %v is not a valid range into the source", sp)
	}
	if got := string(src[sp.Start:sp.End]); !strings.HasPrefix(got, "var") {
		t.Errorf("span covers %q, want the var declaration", got)
	}
}

// Determinism: identical input, identical output, every time.
func TestViewsDeterministic(t *testing.T) {
	src := bt(oneQuery)
	first, _ := Views("repo/users.go", src)
	for range 3 {
		got, _ := Views("repo/users.go", src)
		if len(got) != len(first) {
			t.Fatalf("view count changed between runs")
		}
		for j := range got {
			if string(got[j]) != string(first[j]) {
				t.Fatalf("view %d changed between runs", j)
			}
		}
	}
}

// Views must not alias the caller's buffer.
func TestViewsDoNotAliasSource(t *testing.T) {
	src := bt(oneQuery)
	views, _ := Views("repo/users.go", src)
	orig := string(src)
	for _, v := range views {
		for i := range v {
			v[i] = 'X'
		}
	}
	if string(src) != orig {
		t.Fatal("Views mutated the caller's source")
	}
}

func TestViewsEmptyLiteral(t *testing.T) {
	src := bt(`package repo

//sqletch:query
const aSQL = ~~
`)
	views, diags := Views("repo/users.go", src)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", codes(diags))
	}
	if len(views) != 1 {
		t.Fatalf("got %d views, want 1", len(views))
	}
	if strings.TrimSpace(string(views[0])) != "" {
		t.Errorf("empty literal should yield an all-blank view")
	}
}
