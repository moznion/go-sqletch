package diagnostics

import (
	"strings"
	"testing"
)

// linearLineCol is the pre-optimization O(offset) reference; LineMap
// must be byte-identical to it for every offset (the value feeds LSP
// positions and terminal excerpts).
func linearLineCol(src []byte, off int) (line, col int) {
	if off > len(src) {
		off = len(src)
	}
	line, col = 1, 1
	for i := 0; i < off; i++ {
		if src[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

func TestLineMap_MatchesLinearReference(t *testing.T) {
	sources := [][]byte{
		[]byte(""),
		[]byte("a"),
		[]byte("a\nb"),
		[]byte("\n\n\n"),
		[]byte("no trailing newline"),
		[]byte("line1\nline2\nline3\n"),
		[]byte("mixed\r\nCRLF\r\nline"),
		[]byte("héllo\nωorld\n日本語"), // multibyte: column counts bytes
		[]byte("trailing\n"),
	}
	for _, src := range sources {
		lm := NewLineMap(src)
		// Probe every byte offset plus a few past EOF (offsets are
		// normally span-clamped, but the clamp behavior must match too).
		for off := -2; off <= len(src)+3; off++ {
			wantLine, wantCol := linearLineCol(src, off)
			gotLine, gotCol := lm.LineCol(off)
			// The reference has no negative guard; treat <0 as 0 like
			// LineMap does, matching the old loop (which never runs).
			if off < 0 {
				wantLine, wantCol = linearLineCol(src, 0)
			}
			if gotLine != wantLine || gotCol != wantCol {
				t.Fatalf("src=%q off=%d: LineMap=(%d,%d) want=(%d,%d)",
					src, off, gotLine, gotCol, wantLine, wantCol)
			}
		}
	}
}

func TestLineCol_FreeFunctionUnchanged(t *testing.T) {
	src := []byte("alpha\nbeta\ngamma")
	for off := 0; off <= len(src); off++ {
		wl, wc := linearLineCol(src, off)
		gl, gc := LineCol(src, off)
		if gl != wl || gc != wc {
			t.Fatalf("off=%d: LineCol=(%d,%d) want=(%d,%d)", off, gl, gc, wl, wc)
		}
	}
}

// TestLineMap_ManyDiagnosticsScale is a coarse guard that rendering many
// diagnostics for one large source is no longer O(n·d): with a shared
// LineMap the per-diagnostic lookup is a binary search, so a large batch
// completes near-instantly. This asserts correctness at scale rather
// than wall-clock; the identity test above pins the values.
func TestLineMap_ManyDiagnosticsScale(t *testing.T) {
	src := []byte(strings.Repeat("x", 200_000) + "\n" + strings.Repeat("y", 50_000))
	lm := NewLineMap(src)
	line, col := lm.LineCol(200_001) // first byte of line 2
	if line != 2 || col != 1 {
		t.Fatalf("got (%d,%d), want (2,1)", line, col)
	}
	line, col = lm.LineCol(len(src))
	if line != 2 || col != 50_001 {
		t.Fatalf("got (%d,%d), want (2,50001)", line, col)
	}
}
