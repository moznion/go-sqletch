package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// lspClient is a minimal LSP client speaking Content-Length framing;
// deliberately independent of internal/lsp so this test exercises the
// real wire format end to end.
type lspClient struct {
	t *testing.T
	w io.Writer
	r *bufio.Reader
}

func (c *lspClient) send(v map[string]any) {
	c.t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		c.t.Fatal(err)
	}
}

func (c *lspClient) recv() map[string]any {
	c.t.Helper()
	length := -1
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			c.t.Fatal(err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if rest, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			length, err = strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				c.t.Fatal(err)
			}
		}
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(c.r, body); err != nil {
		c.t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		c.t.Fatal(err)
	}
	return v
}

// The full stack — cli.LSP → OfflineChecker → publishDiagnostics —
// over real framing, against a real project on disk, no database.
func TestLSP_EndToEnd(t *testing.T) {
	cfg := writeOfflineProject(t, map[string]string{"queries/a.sql": validQuery})
	configPath := filepath.Join(cfg.Dir, "sqletch.yaml")
	aPath := filepath.Join(cfg.Dir, "queries", "a.sql")
	uri := "file://" + aPath

	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- LSP(configPath, c2sR, s2cW, io.Discard) }()
	t.Cleanup(func() {
		_ = c2sW.Close()
		_ = s2cR.Close()
	})
	client := &lspClient{t: t, w: c2sW, r: bufio.NewReader(s2cR)}

	client.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	resp := client.recv()
	if resp["result"] == nil {
		t.Fatalf("initialize: %v", resp)
	}
	client.send(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})

	// Open the file with a broken buffer: the on-disk content is valid,
	// the overlay must win.
	client.send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didOpen", "params": map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": "sql", "version": 1, "text": unanchoredQuery},
	}})
	pub := client.recv()
	if pub["method"] != "textDocument/publishDiagnostics" {
		t.Fatalf("expected publishDiagnostics, got %v", pub)
	}
	params := pub["params"].(map[string]any)
	if params["uri"] != uri {
		t.Fatalf("uri = %v", params["uri"])
	}
	// The unanchored WHERE yields SQLETCH100 (the rendering does not
	// parse) and SQLETCH113 (R6); the anchor rule must be among them.
	diags := params["diagnostics"].([]any)
	found := false
	for _, d := range diags {
		if d.(map[string]any)["code"] == "SQLETCH113" {
			found = true
		}
	}
	if !found {
		t.Errorf("SQLETCH113 missing from %v", diags)
	}

	// Fixing the buffer clears the diagnostics.
	client.send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didChange", "params": map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": 2},
		"contentChanges": []map[string]any{{"text": validQuery}},
	}})
	params = client.recv()["params"].(map[string]any)
	if got := params["diagnostics"].([]any); len(got) != 0 {
		t.Fatalf("fixed buffer must publish no diagnostics: %v", got)
	}

	client.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "shutdown"})
	client.recv()
	client.send(map[string]any{"jsonrpc": "2.0", "method": "exit"})
	if code := <-done; code != 0 {
		t.Errorf("exit code = %d", code)
	}
}
