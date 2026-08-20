package template

import (
	"reflect"
	"testing"

	"github.com/moznion/go-sqletch/internal/dialect/postgres"
)

// blankPrefix builds the kind of buffer gosrc hands the scanner: n bytes
// of scan-inert trivia (spaces, with newlines every `line` columns to
// mirror a real .go file's blanked prefix) followed by the template.
func blankPrefix(n, line int, tmpl string) []byte {
	buf := make([]byte, n+len(tmpl))
	for i := range n {
		if line > 0 && i%line == line-1 {
			buf[i] = '\n'
		} else {
			buf[i] = ' '
		}
	}
	copy(buf[n:], tmpl)
	return buf
}

// ScanFileFrom must be byte-identical to ScanFile whenever the skipped
// prefix is scan-inert trivia — that equality is the whole safety
// argument for the gosrc optimization: starting the scan at the
// literal's offset changes nothing about the emitted spans, offsets,
// params, or diagnostics, it only skips re-lexing the blank prefix.
//
// A regression here (any span shifted by the prefix length, any offset
// left window-relative) means Go-authored templates would point at the
// wrong .go bytes, so this compares the FULL scan result, not a summary.
func TestScanFileFromEqualsScanFile(t *testing.T) {
	sc := NewScanner(postgres.Profile{})

	templates := []string{
		"-- name: A :many\nSELECT id FROM users\n",
		"-- name: B :one\nSELECT u.id FROM users AS u WHERE u.id = :id\n",
		// Guards, @in, ORDER BY, a param hint, a semicolon — exercise the
		// offset-bearing fields (Occurrences, WhereKwEnd/TailStart/StmtEnd,
		// TypeHints, item spans).
		"-- name: C :many\n-- @param org: int8\nSELECT id FROM t\nWHERE TRUE\n" +
			"@if-present(org)\n  AND org_id = :org\n@endif\nORDER BY id\n;\n",
		"-- name: D :many\nSELECT id FROM t WHERE id @in(:ids)\n",
		// Multiple queries in one buffer.
		"-- name: E1 :one\nSELECT 1;\n-- name: E2 :many\nSELECT id FROM t;\n",
		// A malformed one, so diagnostics are compared too.
		"-- name: F :many\nSELECT id FROM t\nWHERE\n@if-present(x)\n  AND x = :x\n@endif\n;\n",
	}

	for _, tmpl := range templates {
		for _, prefix := range []struct {
			n, line int
		}{
			{0, 0},     // start == 0 must behave exactly like ScanFile
			{1, 0},     // single leading space
			{200, 17},  // spaces + periodic newlines, like a blanked prefix
			{5000, 40}, // large prefix — the quadratic case
		} {
			// A start landing on a leading whitespace column of the template
			// itself must also match: gosrc's start is the byte after the
			// backquote, which for a `\n-- name:` literal is that newline.
			buf := blankPrefix(prefix.n, prefix.line, tmpl)

			wantFile, wantDiags := sc.ScanFile("t.go", buf)
			gotFile, gotDiags := sc.ScanFileFrom("t.go", buf, prefix.n)

			if !reflect.DeepEqual(gotFile, wantFile) {
				t.Fatalf("ScanFileFrom(start=%d) file differs from ScanFile for template %q\n got: %+v\nwant: %+v",
					prefix.n, tmpl, gotFile, wantFile)
			}
			if !reflect.DeepEqual(gotDiags, wantDiags) {
				t.Fatalf("ScanFileFrom(start=%d) diags differ from ScanFile for template %q\n got: %v\nwant: %v",
					prefix.n, tmpl, gotDiags, wantDiags)
			}
		}
	}
}

// start is clamped: negative and past-EOF starts must not panic and
// must not invent queries.
func TestScanFileFromClampsStart(t *testing.T) {
	sc := NewScanner(postgres.Profile{})
	buf := []byte("   -- name: A :one\nSELECT 1\n")

	if f, _ := sc.ScanFileFrom("t.go", buf, -5); len(f.Queries) != 1 {
		t.Errorf("negative start: got %d queries, want 1", len(f.Queries))
	}
	if f, _ := sc.ScanFileFrom("t.go", buf, len(buf)+99); len(f.Queries) != 0 {
		t.Errorf("past-EOF start: got %d queries, want 0", len(f.Queries))
	}
}
