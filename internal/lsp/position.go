package lsp

import (
	"bytes"
	"unicode/utf16"
	"unicode/utf8"
)

// Diagnostics spans are byte offsets into the template file; the
// protocol wants (line, UTF-16 code unit) pairs. Both conversions
// clamp instead of failing: a span one byte past a just-shortened
// buffer is a race with typing, not an error.

// offsetToPosition converts a byte offset to an LSP position. A
// mid-rune offset rounds up to the end of that rune.
func offsetToPosition(src []byte, off int) Position {
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

// positionToOffset converts an LSP position to a byte offset. A
// character past the line end clamps to the line end (LSP mandate); a
// character landing between the code units of a surrogate pair clamps
// to the rune start; a line past EOF clamps to len(src).
func positionToOffset(src []byte, pos Position) int {
	lineStart := 0
	for line := uint32(0); line < pos.Line; line++ {
		nl := bytes.IndexByte(src[lineStart:], '\n')
		if nl < 0 {
			return len(src)
		}
		lineStart += nl + 1
	}
	i := lineStart
	var units uint32
	for i < len(src) && src[i] != '\n' && units < pos.Character {
		r, size := utf8.DecodeRune(src[i:])
		u := uint32(utf16.RuneLen(r))
		if units+u > pos.Character {
			break
		}
		units += u
		i += size
	}
	return i
}

// spanToRange converts a byte span to an LSP range. Zero-length and
// inverted spans render as one character — the same fallback the CLI's
// caret renderer uses.
func spanToRange(src []byte, start, end int) Range {
	if start < 0 {
		start = 0
	}
	if start > len(src) {
		start = len(src)
	}
	if end <= start {
		end = start + 1
	}
	return Range{Start: offsetToPosition(src, start), End: offsetToPosition(src, end)}
}
