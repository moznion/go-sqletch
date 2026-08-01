// Package lsp implements the sqletch language server
// (docs/design/10-lsp.md): JSON-RPC 2.0 over stdio, a subset of the
// Language Server Protocol (diagnostics + go-to-definition), backed by
// an injected Workspace checker so the protocol layer never touches
// dialect drivers or the pipeline directly.
package lsp

import (
	"bufio"
	"encoding/json"
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
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

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

func (c *conn) read() (*Message, error) {
	length := -1
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
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
	body := make([]byte, length)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return nil, err
	}
	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("lsp: malformed message: %w", err)
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
