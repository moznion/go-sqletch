package lsp

import (
	"unicode/utf16"
	"unicode/utf8"

	"github.com/moznion/go-sqletch/internal/diagnostics"
)

// Diagnostics spans are byte offsets into the template file; the
// protocol wants (line, UTF-16 code unit) pairs. Both conversions
// clamp instead of failing: a span one byte past a just-shortened
// buffer is a race with typing, not an error.
//
// A posMapper carries a precomputed line index (diagnostics.LineMap) so
// converting D diagnostics over an N-byte source costs O(N + Σ touched
// line lengths) instead of O(N·D): the previous free functions rescanned
// the whole buffer from offset 0 on every call, which made publishing
// many diagnostics on a large file quadratic. This is the LSP twin of
// the CLI's LineMap adoption (PR #55). Build one mapper per source and
// reuse it for every position on that source.
type posMapper struct {
	src []byte
	lm  *diagnostics.LineMap
}

func newPosMapper(src []byte) *posMapper {
	return &posMapper{src: src, lm: diagnostics.NewLineMap(src)}
}

// position converts a byte offset to an LSP position. A mid-rune offset
// rounds up to the end of that rune. Byte-identical to the previous
// linear implementation for every input, including the past-EOF/negative
// clamps and the surrogate-pair code-unit count.
func (m *posMapper) position(off int) Position {
	if off < 0 {
		off = 0
	}
	if off > len(m.src) {
		off = len(m.src)
	}
	line, col := m.lm.LineCol(off)
	lineStart := off - (col - 1)
	var units uint32
	for i := lineStart; i < off; {
		r, size := utf8.DecodeRune(m.src[i:])
		i += size
		units += uint32(utf16.RuneLen(r))
	}
	return Position{Line: uint32(line - 1), Character: units}
}

// offset converts an LSP position to a byte offset. A character past the
// line end clamps to the line end (LSP mandate); a character landing
// between the code units of a surrogate pair clamps to the rune start; a
// line past EOF clamps to len(src).
func (m *posMapper) offset(pos Position) int {
	lineStart := m.lm.LineStartOffset(int(pos.Line))
	i := lineStart
	var units uint32
	for i < len(m.src) && m.src[i] != '\n' && units < pos.Character {
		r, size := utf8.DecodeRune(m.src[i:])
		u := uint32(utf16.RuneLen(r))
		if units+u > pos.Character {
			break
		}
		units += u
		i += size
	}
	return i
}

// spanRange converts a byte span to an LSP range. Zero-length and
// inverted spans render as one character — the same fallback the CLI's
// caret renderer uses.
func (m *posMapper) spanRange(start, end int) Range {
	if start < 0 {
		start = 0
	}
	if start > len(m.src) {
		start = len(m.src)
	}
	if end <= start {
		end = start + 1
	}
	return Range{Start: m.position(start), End: m.position(end)}
}

// The free functions build a single-use mapper; they are for callers
// doing one conversion. Loops over many diagnostics on the same source
// must build one posMapper and reuse it (see server.check), or the
// O(N·D) rescan the mapper exists to prevent creeps back in.
func offsetToPosition(src []byte, off int) Position { return newPosMapper(src).position(off) }
func positionToOffset(src []byte, pos Position) int { return newPosMapper(src).offset(pos) }
func spanToRange(src []byte, start, end int) Range  { return newPosMapper(src).spanRange(start, end) }
