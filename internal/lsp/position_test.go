package lsp

import "testing"

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
