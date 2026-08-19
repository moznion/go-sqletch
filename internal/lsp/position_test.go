package lsp

import (
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

// linearPosition is an independent O(offset) reference for the position
// conversion, kept deliberately naive so the prebuilt-index posMapper is
// checked against a from-scratch computation rather than against itself.
func linearPosition(src []byte, off int) Position {
	if off < 0 {
		off = 0
	}
	if off > len(src) {
		off = len(src)
	}
	var line uint32
	lineStart := 0
	for i := 0; i < off; i++ {
		if src[i] == '\n' {
			line++
			lineStart = i + 1
		}
	}
	var units uint32
	for i := lineStart; i < off; {
		r, size := utf8.DecodeRune(src[i:])
		i += size
		units += uint32(utf16.RuneLen(r))
	}
	return Position{Line: line, Character: units}
}

// The indexed posMapper must be byte-identical to the linear reference at
// every offset of a large multi-line source with multibyte and
// surrogate-pair content — this is the whole point of PR #55's LineMap
// twin: converting many positions must not change the answer, only the
// cost (one index build instead of a from-offset-0 rescan per call).
func TestPosMapper_MatchesLinearReference(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		b.WriteString("SELECT x FROM t WHERE 名前 = :p -- 😀 comment\n")
	}
	src := []byte(b.String())
	pm := newPosMapper(src)
	for off := -2; off <= len(src)+2; off++ {
		if got, want := pm.position(off), linearPosition(src, off); got != want {
			t.Fatalf("position(%d) = %+v, want %+v", off, got, want)
			break
		}
	}
	// Stability: mapping a position to an offset and back reproduces the
	// position (mid-rune offsets round up, so offset(position(off)) may
	// differ from off, but the position round-trip is a fixed point).
	for off := 0; off <= len(src); off += 7 {
		p := pm.position(off)
		if back := pm.position(pm.offset(p)); back != p {
			t.Fatalf("position round-trip at off %d unstable: %+v -> %+v", off, p, back)
		}
	}
}

// A single mapper reused across many spans (the server.check hot path)
// yields the same ranges as building a fresh mapper per span — a
// regression guard for the "build the index once" refactor.
func TestPosMapper_ReuseIsConsistent(t *testing.T) {
	src := []byte(posSrc)
	pm := newPosMapper(src)
	for start := 0; start < len(src); start++ {
		end := start + 3
		if got, want := pm.spanRange(start, end), spanToRange(src, start, end); got != want {
			t.Errorf("reused spanRange(%d,%d) = %+v, want %+v", start, end, got, want)
		}
	}
}

// Byte-offset ↔ LSP (line, UTF-16 code unit) conversion over
// adversarial content: CJK (3 bytes / 1 unit), an emoji (4 bytes /
// 2 units, i.e. a surrogate pair on the wire), and a trailing newline.
//
//	offset 9        20    26  30    35
//	        WHERE x = :名前 -- 😀\n
const posSrc = "SELECT 1\nWHERE x = :名前 -- 😀\nEND\n"

func TestOffsetToPosition(t *testing.T) {
	src := []byte(posSrc)
	cases := []struct {
		off        int
		line, char uint32
	}{
		{0, 0, 0},
		{8, 0, 8},   // the newline itself
		{9, 1, 0},   // line start
		{20, 1, 11}, // before 名
		{23, 1, 12}, // between 名 and 前
		{26, 1, 13}, // after 前
		{30, 1, 17}, // before 😀
		{34, 1, 19}, // after 😀: the emoji is 2 UTF-16 units
		{35, 2, 0},
		{38, 2, 3},
		{39, 3, 0},  // EOF after final newline
		{999, 3, 0}, // clamps to EOF
		{21, 1, 12}, // mid-rune offsets round up to the rune end
	}
	for _, c := range cases {
		got := offsetToPosition(src, c.off)
		if got.Line != c.line || got.Character != c.char {
			t.Errorf("offsetToPosition(%d) = %d:%d, want %d:%d", c.off, got.Line, got.Character, c.line, c.char)
		}
	}
}

func TestPositionToOffset(t *testing.T) {
	src := []byte(posSrc)
	cases := []struct {
		line, char uint32
		off        int
	}{
		{0, 0, 0},
		{0, 8, 8},
		{1, 0, 9},
		{1, 11, 20},
		{1, 12, 23},
		{1, 13, 26},
		{1, 17, 30},
		{1, 18, 30}, // mid-surrogate clamps to the rune start
		{1, 19, 34},
		{1, 999, 34}, // past line end clamps to it (excl. newline)
		{3, 0, 39},
		{99, 0, 39}, // past EOF clamps
	}
	for _, c := range cases {
		if got := positionToOffset(src, Position{Line: c.line, Character: c.char}); got != c.off {
			t.Errorf("positionToOffset(%d:%d) = %d, want %d", c.line, c.char, got, c.off)
		}
	}
}

func TestSpanToRange(t *testing.T) {
	src := []byte(posSrc)
	r := spanToRange(src, 20, 26) // 名前
	if r.Start != (Position{1, 11}) || r.End != (Position{1, 13}) {
		t.Errorf("range = %+v", r)
	}
	// Zero-length and inverted spans render as one character (the
	// RenderExcerpt caret fallback).
	r = spanToRange(src, 9, 9)
	if r.Start != (Position{1, 0}) || r.End != (Position{1, 1}) {
		t.Errorf("zero-length span = %+v", r)
	}
	// Out-of-range clamps instead of panicking.
	r = spanToRange(src, 999, 1000)
	if r.Start != (Position{3, 0}) {
		t.Errorf("clamped span = %+v", r)
	}
}

func TestPositionCRLF(t *testing.T) {
	src := []byte("a\r\nbc")
	if got := offsetToPosition(src, 3); got != (Position{1, 0}) {
		t.Errorf("after CRLF = %+v", got)
	}
	if got := positionToOffset(src, Position{Line: 1, Character: 1}); got != 4 {
		t.Errorf("CRLF offset = %d", got)
	}
}
