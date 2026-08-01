package lsp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestConn_ReadMessage(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	// Extra headers must be skipped; header names are case-insensitive.
	wire := fmt.Sprintf("content-length: %d\r\nContent-Type: application/vscode-jsonrpc\r\n\r\n%s", len(body), body)
	c := newConn(strings.NewReader(wire), io.Discard)
	msg, err := c.read()
	if err != nil {
		t.Fatal(err)
	}
	if msg.Method != "initialize" {
		t.Errorf("method = %q", msg.Method)
	}
	if msg.ID == nil || string(*msg.ID) != "1" {
		t.Errorf("id = %v", msg.ID)
	}
	if _, err := c.read(); !errors.Is(err, io.EOF) {
		t.Errorf("second read must report EOF, got %v", err)
	}
}

func frame(body string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
}

func TestConn_ReadRejectsDuplicateMembers(t *testing.T) {
	for name, body := range map[string]string{
		"top-level": `{"jsonrpc":"2.0","method":"a","method":"b"}`,
		"nested":    `{"jsonrpc":"2.0","method":"a","params":{"x":1,"x":2}}`,
	} {
		c := newConn(strings.NewReader(frame(body)), io.Discard)
		if _, err := c.read(); !errors.Is(err, errMalformedBody) {
			t.Errorf("%s: duplicate members must be errMalformedBody, got %v", name, err)
		}
	}
}

func TestConn_ReadRejectsInvalidUTF8(t *testing.T) {
	body := "{\"jsonrpc\":\"2.0\",\"method\":\"\xff\"}"
	c := newConn(strings.NewReader(frame(body)), io.Discard)
	if _, err := c.read(); !errors.Is(err, errMalformedBody) {
		t.Errorf("invalid UTF-8 must be errMalformedBody, got %v", err)
	}
}

func TestConn_ReadMemberNamesCaseSensitive(t *testing.T) {
	// JSON is case-sensitive: "Method" is an unknown member, not the
	// "method" member, so the message decodes as method-less.
	body := `{"jsonrpc":"2.0","id":1,"Method":"initialize"}`
	c := newConn(strings.NewReader(frame(body)), io.Discard)
	msg, err := c.read()
	if err != nil {
		t.Fatal(err)
	}
	if msg.Method != "" {
		t.Errorf(`"Method" must not populate method, got %q`, msg.Method)
	}
}

func TestConn_WriteErrorNullID(t *testing.T) {
	var buf bytes.Buffer
	c := newConn(strings.NewReader(""), &buf)
	if err := c.writeError(rawID("null"), codeParseError, "bad body"); err != nil {
		t.Fatal(err)
	}
	// JSON-RPC 2.0: a parse-error response carries an explicit null id.
	if !strings.Contains(buf.String(), `"id":null`) {
		t.Errorf("parse error must carry id null: %s", buf.String())
	}
}

func TestConn_ReadMissingContentLength(t *testing.T) {
	c := newConn(strings.NewReader("X-Nope: 1\r\n\r\n{}"), io.Discard)
	if _, err := c.read(); err == nil {
		t.Fatal("missing Content-Length must error")
	}
}

func TestConn_WriteFraming(t *testing.T) {
	var buf bytes.Buffer
	c := newConn(strings.NewReader(""), &buf)
	if err := c.writeNotification("textDocument/publishDiagnostics", map[string]any{"uri": "file:///a"}); err != nil {
		t.Fatal(err)
	}
	wire := buf.Bytes()
	if !bytes.HasPrefix(wire, []byte("Content-Length: ")) {
		t.Fatalf("wire = %q", wire)
	}
	// Round-trip: the written bytes must read back as one message.
	back := newConn(bytes.NewReader(wire), io.Discard)
	msg, err := back.read()
	if err != nil {
		t.Fatal(err)
	}
	if msg.Method != "textDocument/publishDiagnostics" || msg.JSONRPC != "2.0" {
		t.Errorf("round-trip lost fields: %+v", msg)
	}
}

func TestConn_WriteResponseNullResult(t *testing.T) {
	var buf bytes.Buffer
	c := newConn(strings.NewReader(""), &buf)
	id := rawID("7")
	if err := c.writeResponse(id, nil); err != nil {
		t.Fatal(err)
	}
	// A response must carry an explicit result member even when null.
	if !strings.Contains(buf.String(), `"result":null`) {
		t.Errorf("null result must be explicit: %s", buf.String())
	}
}
