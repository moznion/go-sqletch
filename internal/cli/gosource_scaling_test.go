package cli

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

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

// A .go file with many marked consts must scan in ~linear total work.
// Each const's view is the whole file with everything but its own
// literal blanked; handing the scanner the full [0,end) prefix made it
// re-lex (and re-copy, as one giant whitespace token's Text) that
// blank prefix once per const — O(consts × file size). At source scale
// that is seconds of CPU and, through cli.scanSource, hangs the LSP on
// file-open. This asserts the scan cost tracks file size, not its
// square: doubling the const count must not quadruple the work.
//
// Work is measured as bytes allocated, not wall-clock time: the
// re-lex copies the blank prefix into a token per const, so allocation
// carries the same quadratic signal deterministically. A timing ratio
// flaked on loaded CI runners (testing policy: no wall-clock
// dependence). Measured: the fix allocates 2.05× from k=2000 to
// k=4000; scanning each view from 0 instead (the pre-fix behavior)
// allocates 3.92×.
func TestScanSourceGoScalesLinearly(t *testing.T) {
	sc := template.NewScanner(driverFor(config.Config{Dialect: "postgres"}).profile)

	allocated := func(k int) uint64 {
		src := buildGoConsts(k)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		file, diags := scanSource(sc, "repo/users.go", src)
		runtime.ReadMemStats(&after)
		if len(diags) != 0 {
			t.Fatalf("k=%d: unexpected diagnostics: %v", k, diags)
		}
		if len(file.Queries) != k {
			t.Fatalf("k=%d: got %d queries, want %d", k, len(file.Queries), k)
		}
		return after.TotalAlloc - before.TotalAlloc
	}

	b2 := allocated(2000)
	b4 := allocated(4000)

	// Linear scanning gives b4 ≈ 2·b2; the quadratic prefix re-lex gives
	// b4 ≈ 4·b2. Fail at 3× — between the two regimes.
	if ratio := float64(b4) / float64(b2); ratio > 3.0 {
		t.Fatalf("scan allocation scales super-linearly with const count: "+
			"k=2000 allocated %d B, k=4000 allocated %d B (%.2f×, want ≈2×); "+
			"the blank prefix before each literal is being re-lexed per const", b2, b4, ratio)
	}
}
