package lsp

import (
	"io"
	"strings"
	"testing"
)

// countingReader yields an endless stream of the same byte and records
// how much was consumed, so a test can prove a read is bounded.
type countingReader struct {
	b byte
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	r.n += int64(len(p))
	return len(p), nil
}

// A header stream that never contains '\n' must be refused after a
// bounded number of bytes, not accumulated until OOM. Before the fix
// conn.read used bufio.Reader.ReadString('\n'), which grows without
// limit.
func TestConn_ReadBoundsHeaderWithoutNewline(t *testing.T) {
	cr := &countingReader{b: 'A'}
	c := newConn(cr, io.Discard)
	_, err := c.read()
	if err == nil {
		t.Fatal("expected a framing error for an unterminated header, got nil")
	}
	// The read must stop far below anything that threatens the process.
	// bufio fills in fixed chunks, so allow generous slack over the
	// 8 KiB header ceiling but assert it is nowhere near unbounded.
	if cr.n > 1<<20 {
		t.Fatalf("header read consumed %d bytes; expected it bounded near maxHeaderBytes (%d)", cr.n, maxHeaderBytes)
	}
}

// A header block made of many short lines (each ending in \n) but never
// reaching the blank separator is also bounded.
func TestConn_ReadBoundsManyHeaderLines(t *testing.T) {
	// Each "X:1\n" is 4 bytes; far more than maxHeaderBytes worth.
	wire := strings.Repeat("X:1\n", maxHeaderBytes)
	c := newConn(strings.NewReader(wire), io.Discard)
	if _, err := c.read(); err == nil {
		t.Fatal("expected a framing error for an over-long header block, got nil")
	}
}

// A normal two-line header still frames correctly after the cap change.
func TestConn_ReadNormalHeaderStillWorks(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	c := newConn(strings.NewReader(frame(body)), io.Discard)
	msg, err := c.read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msg.Method != "ping" {
		t.Fatalf("method = %q, want ping", msg.Method)
	}
}
