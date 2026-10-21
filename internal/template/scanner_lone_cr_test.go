package template

import (
	"strings"
	"testing"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/dialect"
	"github.com/moznion/go-sqletch/internal/dialect/mysql"
	"github.com/moznion/go-sqletch/internal/dialect/postgres"
	"github.com/moznion/go-sqletch/internal/dialect/sqlite"
)

// Security audit F11/F12 (CWE-436), owner decision 2026-10-21: a lone
// carriage return (CR not followed by LF) outside string literals and
// quoted identifiers is SQLETCH024 on EVERY dialect. Engines disagree on
// whether it ends a line comment (PostgreSQL: yes; MySQL, TiDB, SQLite,
// rqlite: no), and so did sqletch's own lexers with their engines: a
// policy conjunct after `-- note<CR>` was live SQL to the weaver and
// Enforce but comment text to the engine — a silent cross-tenant leak.
// Rejecting the byte removes the disagreement instead of picking a side.

var loneCRProfiles = []struct {
	name    string
	profile dialect.LexerProfile
}{
	{"postgres", postgres.Profile{}},
	{"mysql", mysql.Profile{}},
	{"sqlite", sqlite.Profile{}},
}

// loneCRSpans returns the spans of the SQLETCH024 diagnostics.
func loneCRSpans(diags []diagnostics.Diagnostic) [][2]int {
	var out [][2]int
	for _, d := range diags {
		if d.Code == diagnostics.CodeLoneCR {
			out = append(out, [2]int{d.Span.Start, d.Span.End})
		}
	}
	return out
}

// crOffsets is the hand-computed oracle: the offsets of the '\r' bytes
// in src that the case marks as lone (every '\r' in these fixtures sits
// outside a string, and none is followed by '\n').
func crOffsets(src string) [][2]int {
	var out [][2]int
	for i := 0; i < len(src); i++ {
		if src[i] == '\r' {
			out = append(out, [2]int{i, i + 1})
		}
	}
	return out
}

func TestScan_LoneCRRejected(t *testing.T) {
	cases := []struct{ name, src string }{
		// A classic-Mac (CR-only) file used to be accepted by ending line
		// comments at CR; it is now rejected (convert to LF).
		{"cr-only file", "-- name: A :one\rSELECT 1;\r"},
		// The F11/F12 leak shapes: the conjunct after the comment is
		// comment text on MySQL/SQLite.
		{"conjunct after comment", "-- name: A :many\nSELECT o.id FROM orders AS o\nWHERE o.status = :status -- open only\r AND o.tenant_id = :tenant_id;\n"},
		{"order by after comment", "-- name: A :many\nSELECT id FROM orders -- newest\rORDER BY id;\n"},
		{"in block comment", "-- name: A :one\nSELECT /* a\rb */ 1;\n"},
		{"between tokens", "-- name: A :one\nSELECT\r1;\n"},
		{"at EOF", "-- name: A :one\nSELECT 1;\r"},
		{"in construct whitespace", "-- name: A :many\nSELECT o.id FROM orders AS o WHERE TRUE\n@if-present(s)\r  AND o.s = :s\n@endif;\n"},
		{"two lone CRs", "-- name: A :one\nSELECT\r1\r;\n"},
	}
	for _, p := range loneCRProfiles {
		for _, tc := range cases {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				_, diags := NewScanner(p.profile).ScanFile("q.sql", []byte(tc.src))
				got, want := loneCRSpans(diags), crOffsets(tc.src)
				if len(got) != len(want) {
					t.Fatalf("SQLETCH024 spans = %v, want %v\n%s", got, want, renderAll(diags, tc.src))
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("SQLETCH024 spans = %v, want %v", got, want)
					}
				}
				for _, d := range diags {
					if d.Code == diagnostics.CodeLoneCR && (d.Severity != diagnostics.Error || d.Hint == "") {
						t.Fatalf("SQLETCH024 must be an error with a hint: %+v", d)
					}
				}
			})
		}
	}
}

// Accepted neighbors: CRLF line endings, and a CR inside a string
// literal or quoted identifier (data, not line structure — every engine
// keeps it verbatim).
func TestScan_LoneCRAcceptedNeighbors(t *testing.T) {
	type tc struct{ name, src string }
	common := []tc{
		{"crlf file", "-- name: A :one\r\nSELECT 1;\r\n"},
		{"crlf with directive and construct", "-- name: A :many\r\n-- @param s: text\r\nSELECT o.id FROM orders AS o WHERE TRUE\r\n@if-present(s)\r\n  AND o.s = :s\r\n@endif;\r\n"},
		{"crlf comment mid-statement", "-- name: A :one\r\nSELECT 1 -- note\r\nFROM t;\r\n"},
		{"cr in string", "-- name: A :one\nSELECT 'a\rb';\n"},
	}
	perDialect := map[string][]tc{
		"postgres": {{"cr in quoted ident", "-- name: A :one\nSELECT 1 AS \"a\rb\";\n"}, {"cr in dollar string", "-- name: A :one\nSELECT $$a\rb$$;\n"}},
		"mysql":    {{"cr in backtick ident", "-- name: A :one\nSELECT 1 AS `a\rb`;\n"}, {"cr in double-quoted string", "-- name: A :one\nSELECT \"a\rb\";\n"}},
		"sqlite":   {{"cr in quoted ident", "-- name: A :one\nSELECT 1 AS \"a\rb\";\n"}, {"cr in bracket ident", "-- name: A :one\nSELECT 1 AS [a\rb];\n"}},
	}
	for _, p := range loneCRProfiles {
		for _, c := range append(append([]tc{}, common...), perDialect[p.name]...) {
			t.Run(p.name+"/"+c.name, func(t *testing.T) {
				f, diags := NewScanner(p.profile).ScanFile("q.sql", []byte(c.src))
				if len(diags) != 0 {
					t.Fatalf("unexpected diagnostics:\n%s", renderAll(diags, c.src))
				}
				if len(f.Queries) != 1 || f.Queries[0].Name != "A" {
					t.Fatalf("queries = %d, want exactly A", len(f.Queries))
				}
			})
		}
	}
}

// A Go-source view (gosrc, ScanFileFrom) is scanned from the literal's
// offset; a lone CR inside it is reported at its true file offset.
func TestScan_LoneCRInWindowedView(t *testing.T) {
	prefix := strings.Repeat(" ", 10)
	src := prefix + "-- name: A :one\nSELECT\r1;\n"
	for _, p := range loneCRProfiles {
		_, diags := NewScanner(p.profile).ScanFileFrom("q.go", []byte(src), len(prefix))
		got := loneCRSpans(diags)
		want := [2]int{strings.IndexByte(src, '\r'), strings.IndexByte(src, '\r') + 1}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("%s: SQLETCH024 spans = %v, want [%v]", p.name, got, want)
		}
	}
}
