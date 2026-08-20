package cli

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/moznion/go-sqletch/internal/config"
	"github.com/moznion/go-sqletch/internal/template"
)

// Bounding each const's scan to its own literal must not move any span:
// every offset the scanner emits still indexes the original .go file.
// Multibyte content sits before the marked const so a prefix-length or
// rune/byte mix-up would show up as a shifted span.
func TestScanSourceGoOffsetsIndexOriginal(t *testing.T) {
	sc := template.NewScanner(driverFor(config.Config{Dialect: "postgres"}).profile)

	src := []byte(strings.ReplaceAll(`package repo

// 日本語コメント — multibyte bytes before the marked const, so any
// prefix-length confusion shifts the spans below.
const other = "helper"

//sqletch:query
const findSQL = ~
-- name: Find :many
SELECT id FROM t WHERE t.x = :x
~
`, "~", "`"))

	file, diags := scanSource(sc, "repo/users.go", src)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(file.Queries) != 1 {
		t.Fatalf("got %d queries, want 1", len(file.Queries))
	}
	q := file.Queries[0]

	// HeaderSpan must cover the real `-- name: Find :many` bytes.
	if got := string(src[q.HeaderSpan.Start:q.HeaderSpan.End]); got != "-- name: Find :many" {
		t.Errorf("HeaderSpan indexes %q, want the header line", got)
	}
	// The :x occurrence span must land on ':x' in the original file.
	p := q.Params["x"]
	if p == nil || len(p.Occurrences) != 1 {
		t.Fatalf("param x = %+v, want one occurrence", p)
	}
	occ := p.Occurrences[0]
	if got := string(src[occ.Span.Start:occ.Span.End]); got != ":x" {
		t.Errorf("occurrence span indexes %q, want %q", got, ":x")
	}
	// And its offset must be the literal ':x' position in the whole file.
	if want := strings.Index(string(src), ":x"); occ.Span.Start != want {
		t.Errorf("occurrence offset = %d, want %d (byte offset into the original .go file)", occ.Span.Start, want)
	}
}

// buildGoConsts writes a .go file holding k `//sqletch:query` consts,
// each a small independent template. The k-th const sits ~5·k lines
// deep, so the blanked prefix before its literal grows with k — the
// exact shape that turns a per-const re-lex of that prefix into
// O(k·file) quadratic work.
func buildGoConsts(k int) []byte {
	var b strings.Builder
	b.WriteString("package repo\n\n")
	for i := range k {
		fmt.Fprintf(&b, "//sqletch:query\nconst q%d = `\n-- name: Q%d :many\nSELECT %d\n`\n\n", i, i, i)
	}
	return []byte(b.String())
}

// A .go file with many marked consts must scan in ~linear total time.
// Each const's view is the whole file with everything but its own
// literal blanked; handing the scanner the full [0,end) prefix made it
// re-lex (and re-copy, as one giant whitespace token's Text) that
// blank prefix once per const — O(consts × file size). At source scale
// that is seconds of CPU and, through cli.scanSource, hangs the LSP on
// file-open. This asserts the scan cost tracks file size, not its
// square: doubling the const count must not quadruple the time.
//
// On the pre-fix (quadratic) scanner this FAILS — k=4000 takes ~4× the
// k=2000 time; the fix bounds each const's scan to its own literal, so
// the ratio drops to ~2×.
func TestScanSourceGoScalesLinearly(t *testing.T) {
	sc := template.NewScanner(driverFor(config.Config{Dialect: "postgres"}).profile)

	measure := func(k int) time.Duration {
		src := buildGoConsts(k)
		best := time.Duration(1<<62 - 1)
		for range 5 {
			t0 := time.Now()
			file, diags := scanSource(sc, "repo/users.go", src)
			el := time.Since(t0)
			if len(diags) != 0 {
				t.Fatalf("k=%d: unexpected diagnostics: %v", k, diags)
			}
			if len(file.Queries) != k {
				t.Fatalf("k=%d: got %d queries, want %d", k, len(file.Queries), k)
			}
			if el < best {
				best = el
			}
		}
		return best
	}

	t2 := measure(2000)
	t4 := measure(4000)

	// Linear scanning gives t4 ≈ 2·t2; the quadratic prefix re-lex gives
	// t4 ≈ 4·t2. Fail at 3× — comfortably between the two regimes and
	// tolerant of scheduler noise (best-of-5 already damps it).
	if ratio := float64(t4) / float64(t2); ratio > 3.0 {
		t.Fatalf("scan time scales super-linearly with const count: "+
			"k=2000 took %v, k=4000 took %v (%.2f×, want ≈2×); "+
			"the blank prefix before each literal is being re-lexed per const", t2, t4, ratio)
	}
}
