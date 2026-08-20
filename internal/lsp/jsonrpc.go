// Package lsp implements the sqletch language server
// (docs/design/10-lsp.md): JSON-RPC 2.0 over stdio, a subset of the
// Language Server Protocol (diagnostics + go-to-definition), backed by
// an injected Workspace checker so the protocol layer never touches
// dialect drivers or the pipeline directly.
package lsp

import (
	"bufio"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// Message is a JSON-RPC 2.0 request, notification, or response; unset
// members are omitted on the wire.
type Message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *ResponseError   `json:"error,omitempty"`
}

type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC 2.0 error codes (the ones this server emits).
const (
	codeParseError     = -32700
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// errMalformedBody marks a framed body that failed to decode as a
// JSON-RPC message. The Content-Length boundary is still intact, so
// the stream remains usable: the caller may answer with a parse error
// and keep reading instead of tearing the transport down.
var errMalformedBody = errors.New("lsp: malformed message body")

func rawID(s string) *json.RawMessage {
	r := json.RawMessage(s)
	return &r
}

// conn frames JSON-RPC messages with Content-Length headers. Writes
// are mutex-serialized; reads are single-consumer.
type conn struct {
	mu sync.Mutex
	r  *bufio.Reader
	w  io.Writer
}

func newConn(r io.Reader, w io.Writer) *conn {
	return &conn{r: bufio.NewReader(r), w: w}
}

// maxContentLength bounds one inbound JSON-RPC body. LSP payloads are
// documents and edits: 64 MiB is far above any source file an editor
// will send and far below a size that threatens the process.
const maxContentLength = 64 << 20

// maxHeaderBytes bounds the whole header block (all lines up to the
// blank separator) of one inbound message, and maxHeaderLines bounds
// the line count. The body already has maxContentLength; without these
// the header phase does not, so a peer streaming bytes that never
// contain '\n' would grow the read buffer until an out-of-memory abort
// — the exact failure the body ceiling prevents. Real LSP headers are
// two short lines, so 8 KiB / 64 lines is far above any legitimate
// frame and far below a size that threatens the process.
const (
	maxHeaderBytes = 8 << 10
	maxHeaderLines = 64
)

// readHeaderLine reads one header line (through the terminating '\n',
// which it discards) while consuming at most *budget bytes across the
// whole header block. It errors instead of accumulating without bound.
func (c *conn) readHeaderLine(budget *int) (string, error) {
	var sb strings.Builder
	for {
		if *budget <= 0 {
			return "", fmt.Errorf("lsp: header block exceeds the %d-byte limit", maxHeaderBytes)
		}
		b, err := c.r.ReadByte()
		if err != nil {
			return "", err
		}
		*budget--
		if b == '\n' {
			return sb.String(), nil
		}
		sb.WriteByte(b)
	}
}

func (c *conn) read() (*Message, error) {
	length := -1
	budget := maxHeaderBytes
	for lines := 0; ; lines++ {
		if lines >= maxHeaderLines {
			return nil, fmt.Errorf("lsp: header block exceeds the %d-line limit", maxHeaderLines)
		}
		line, err := c.readHeaderLine(&budget)
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("lsp: malformed header %q", line)
		}
		// Header names are case-insensitive; only Content-Length
		// matters, the rest (Content-Type) are skipped.
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("lsp: bad Content-Length: %w", err)
			}
		}
	}
	if length < 0 {
		return nil, errors.New("lsp: missing Content-Length header")
	}
	if length > maxContentLength {
		// The header sizes an allocation from a number the peer chose.
		// Without a ceiling a malformed frame is an out-of-memory abort
		// — which takes the server down with no diagnostic — rather
		// than a framing error it can report and resynchronize from.
		return nil, fmt.Errorf("lsp: Content-Length %d exceeds the %d-byte limit",
			length, maxContentLength)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return nil, err
	}
	// Inbound bodies decode with json/v2: duplicate members and
	// invalid UTF-8 are rejected, and member names match
	// case-sensitively, as JSON-RPC requires. Outbound marshaling
	// stays on v1 so the wire output is unchanged.
	var msg Message
	if err := jsonv2.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("%w: %v", errMalformedBody, err)
	}
	return &msg, nil
}

func (c *conn) write(msg Message) error {
	msg.JSONRPC = "2.0"
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.w.Write(body)
	return err
}

func marshalRaw(v any) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage("null"), nil
	}
	return json.Marshal(v)
}

func (c *conn) writeNotification(method string, params any) error {
	p, err := marshalRaw(params)
	if err != nil {
		return err
	}
	return c.write(Message{Method: method, Params: p})
}

func (c *conn) writeRequest(id *json.RawMessage, method string, params any) error {
	p, err := marshalRaw(params)
	if err != nil {
		return err
	}
	return c.write(Message{ID: id, Method: method, Params: p})
}

// writeResponse always carries an explicit result member — a null
// result is a valid successful response (e.g. a definition miss).
func (c *conn) writeResponse(id *json.RawMessage, result any) error {
	r, err := marshalRaw(result)
	if err != nil {
		return err
	}
	return c.write(Message{ID: id, Result: r})
}

func (c *conn) writeError(id *json.RawMessage, code int, message string) error {
	return c.write(Message{ID: id, Error: &ResponseError{Code: code, Message: message}})
}
