package lsp

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/moznion/sqletch/internal/diagnostics"
	"github.com/moznion/sqletch/internal/dialect/postgres"
	"github.com/moznion/sqletch/internal/template"
)

// fakeWS flags every occurrence of "BAD" in the checked content — just
// enough behavior to exercise the protocol without the real pipeline
// (the real-checker end-to-end test lives in internal/cli).
type fakeWS struct {
	files   map[string]*template.QueryFile
	sources map[string][]byte
}

func (f *fakeWS) Check(overlay map[string][]byte) (WorkspaceResult, error) {
	res := WorkspaceResult{
		Diags:   map[string][]diagnostics.Diagnostic{},
		Files:   f.files,
		Sources: map[string][]byte{},
	}
	if res.Files == nil {
		res.Files = map[string]*template.QueryFile{}
	}
	for p, src := range f.sources {
		res.Sources[p] = src
	}
	for p, src := range overlay {
		res.Sources[p] = src
		res.Diags[p] = nil
		if i := bytes.Index(src, []byte("BAD")); i >= 0 {
			res.Diags[p] = []diagnostics.Diagnostic{diagnostics.Errorf("SQLETCH999",
				diagnostics.Span{File: p, Start: i, End: i + 3}, "bad marker").WithHint("remove it")}
		}
	}
	return res, nil
}

// testClient drives a Server over in-memory pipes.
type testClient struct {
	t    *testing.T
	c    *conn
	done chan int
	next int
}

func startServer(t *testing.T, ws Workspace, initErr string) *testClient {
	t.Helper()
	clientToServer, serverIn := io.Pipe()
	serverOut, serverToClient := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- Serve(clientToServer, serverToClient, ws, initErr, io.Discard) }()
	tc := &testClient{t: t, c: newConn(serverOut, serverIn), done: done}
	t.Cleanup(func() {
		_ = serverIn.Close()
		_ = serverOut.Close()
	})
	return tc
}

func (tc *testClient) notify(method string, params any) {
	tc.t.Helper()
	if err := tc.c.writeNotification(method, params); err != nil {
		tc.t.Fatal(err)
	}
}

// request sends a request and pumps messages until its response
// arrives; interleaved server notifications are returned too.
func (tc *testClient) request(method string, params any) (json.RawMessage, []*Message) {
	tc.t.Helper()
	tc.next++
	idBytes, _ := json.Marshal(tc.next)
	id := rawID(string(idBytes))
	if err := tc.c.writeRequest(id, method, params); err != nil {
		tc.t.Fatal(err)
	}
	var notes []*Message
	for {
		msg, err := tc.c.read()
		if err != nil {
			tc.t.Fatalf("awaiting %s response: %v", method, err)
		}
		if msg.ID != nil && string(*msg.ID) == string(idBytes) {
			if msg.Error != nil {
				tc.t.Fatalf("%s error: %+v", method, msg.Error)
			}
			return msg.Result, notes
		}
		notes = append(notes, msg)
	}
}

// readNotification reads the next server→client message.
func (tc *testClient) readNotification() *Message {
	tc.t.Helper()
	msg, err := tc.c.read()
	if err != nil {
		tc.t.Fatal(err)
	}
	return msg
}

func decodeParams[T any](t *testing.T, msg *Message) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(msg.Params, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (tc *testClient) initialize() {
	tc.t.Helper()
	result, _ := tc.request("initialize", map[string]any{})
	var init InitializeResult
	if err := json.Unmarshal(result, &init); err != nil {
		tc.t.Fatal(err)
	}
	if init.Capabilities.TextDocumentSync != syncFull {
		tc.t.Errorf("sync = %d, want full", init.Capabilities.TextDocumentSync)
	}
	if !init.Capabilities.DefinitionProvider {
		tc.t.Error("definitionProvider must be on")
	}
	if init.Capabilities.PositionEncoding != "utf-16" {
		tc.t.Errorf("positionEncoding = %q", init.Capabilities.PositionEncoding)
	}
	tc.notify("initialized", map[string]any{})
}

func (tc *testClient) shutdownExit(wantCode int) {
	tc.t.Helper()
	tc.request("shutdown", nil)
	tc.notify("exit", nil)
	if code := <-tc.done; code != wantCode {
		tc.t.Errorf("exit code = %d, want %d", code, wantCode)
	}
}

func TestServer_DiagnosticsLifecycle(t *testing.T) {
	tc := startServer(t, &fakeWS{}, "")
	tc.initialize()

	uri := pathToURI("/ws/queries/q.sql")
	tc.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": "sql", "version": 1, "text": "xx BAD yy"},
	})
	msg := tc.readNotification()
	if msg.Method != "textDocument/publishDiagnostics" {
		t.Fatalf("expected publishDiagnostics, got %q", msg.Method)
	}
	pub := decodeParams[PublishDiagnosticsParams](t, msg)
	if pub.URI != uri || len(pub.Diagnostics) != 1 {
		t.Fatalf("publish = %+v", pub)
	}
	d := pub.Diagnostics[0]
	if d.Code != "SQLETCH999" || d.Source != "sqletch" || d.Severity != severityError {
		t.Errorf("diagnostic = %+v", d)
	}
	if d.Range.Start != (Position{0, 3}) || d.Range.End != (Position{0, 6}) {
		t.Errorf("range = %+v", d.Range)
	}
	if !strings.Contains(d.Message, "bad marker") || !strings.Contains(d.Message, "remove it") {
		t.Errorf("message must carry the hint: %q", d.Message)
	}

	// The fix must clear the published diagnostics.
	tc.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": 2},
		"contentChanges": []map[string]any{{"text": "all good"}},
	})
	pub = decodeParams[PublishDiagnosticsParams](t, tc.readNotification())
	if pub.URI != uri || len(pub.Diagnostics) != 0 {
		t.Fatalf("fix must publish empty diagnostics: %+v", pub)
	}

	tc.shutdownExit(0)
}

func TestServer_UnknownMethodAndExitWithoutShutdown(t *testing.T) {
	tc := startServer(t, &fakeWS{}, "")
	tc.initialize()
	tc.next++
	idBytes, _ := json.Marshal(tc.next)
	if err := tc.c.writeRequest(rawID(string(idBytes)), "workspace/nope", nil); err != nil {
		t.Fatal(err)
	}
	msg := tc.readNotification()
	if msg.Error == nil || msg.Error.Code != codeMethodNotFound {
		t.Fatalf("want MethodNotFound, got %+v", msg)
	}
	tc.notify("exit", nil)
	if code := <-tc.done; code != 1 {
		t.Errorf("exit without shutdown must return 1, got %d", code)
	}
}

func TestServer_DegradedConfigFailure(t *testing.T) {
	tc := startServer(t, nil, "sqletch.yaml: dialect is required")
	result, _ := tc.request("initialize", map[string]any{})
	if len(result) == 0 {
		t.Fatal("degraded server must still answer initialize")
	}
	tc.notify("initialized", map[string]any{})
	msg := tc.readNotification()
	if msg.Method != "window/showMessage" {
		t.Fatalf("expected showMessage, got %q", msg.Method)
	}
	sm := decodeParams[ShowMessageParams](t, msg)
	if sm.Type != messageError || !strings.Contains(sm.Message, "dialect is required") {
		t.Errorf("showMessage = %+v", sm)
	}
	// Documents are accepted but produce nothing.
	tc.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": pathToURI("/x.sql"), "languageId": "sql", "version": 1, "text": "BAD"},
	})
	tc.shutdownExit(0)
}

const defTemplate = `-- name: FindT :many
-- @param min: int8
SELECT t.id FROM t WHERE TRUE AND t.score > :min AND t.x = :x;
`

func TestServer_Definition(t *testing.T) {
	path := "/ws/queries/def.sql"
	src := []byte(defTemplate)
	file, diags := template.NewScanner(postgres.Profile{}).ScanFile(path, src)
	if diagnostics.HasErrors(diags) {
		t.Fatalf("scan: %v", diags)
	}
	q := file.Queries[0]
	minOcc := q.Params["min"].Occurrences[0].Span
	xOcc := q.Params["x"].Occurrences[0].Span
	hint, ok := q.TypeHints["min"]
	if !ok {
		t.Fatal("scanner must record the @param hint")
	}

	ws := &fakeWS{
		files:   map[string]*template.QueryFile{path: file},
		sources: map[string][]byte{path: src},
	}
	tc := startServer(t, ws, "")
	tc.initialize()
	uri := pathToURI(path)
	tc.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": "sql", "version": 1, "text": defTemplate},
	})
	tc.readNotification() // publish for the opened doc

	def := func(off int) json.RawMessage {
		t.Helper()
		pos := offsetToPosition(src, off)
		result, _ := tc.request("textDocument/definition", map[string]any{
			"textDocument": map[string]any{"uri": uri},
			"position":     map[string]any{"line": pos.Line, "character": pos.Character},
		})
		return result
	}
	decodeLoc := func(raw json.RawMessage) Location {
		t.Helper()
		var loc Location
		if err := json.Unmarshal(raw, &loc); err != nil {
			t.Fatalf("definition result %s: %v", raw, err)
		}
		return loc
	}

	// A :min occurrence resolves to its -- @param annotation.
	loc := decodeLoc(def(minOcc.Start + 1))
	if loc.URI != uri || loc.Range != spanToRange(src, hint.Span.Start, hint.Span.End) {
		t.Errorf("min occurrence → %+v, want the @param hint", loc)
	}
	// The annotation resolves to the first occurrence.
	loc = decodeLoc(def(hint.Span.Start + 1))
	if loc.Range != spanToRange(src, minOcc.Start, minOcc.End) {
		t.Errorf("hint → %+v, want the first occurrence", loc)
	}
	// A param without annotation resolves to its first occurrence.
	loc = decodeLoc(def(xOcc.Start + 1))
	if loc.Range != spanToRange(src, xOcc.Start, xOcc.End) {
		t.Errorf("x occurrence → %+v, want itself", loc)
	}
	// Anywhere else: null.
	if raw := def(0); string(raw) != "null" {
		t.Errorf("miss must be null, got %s", raw)
	}
	tc.shutdownExit(0)
}
