package lsp

import (
	"fmt"
	"io"
	"log"
	"sync/atomic"
	"testing"
)

// countingWS counts Check invocations on top of fakeWS's behavior, so a
// test can assert that a refused didOpen does not buy a full-workspace
// re-check. The counter is atomic because the server goroutine writes it
// while the test goroutine reads it (the request round-trips below order
// the accesses, but the race detector wants the atomicity explicit).
type countingWS struct {
	fakeWS
	checks atomic.Int64
}

func (c *countingWS) Check(overlay map[string][]byte) (WorkspaceResult, error) {
	c.checks.Add(1)
	return c.fakeWS.Check(overlay)
}

// startServerWithCap is startServer with a lowered open-document cap
// (the production cap of maxOpenDocuments open documents is too large to
// drive through the protocol in a unit test).
func startServerWithCap(t *testing.T, ws Workspace, maxDocs int) *testClient {
	t.Helper()
	clientToServer, serverIn := io.Pipe()
	serverOut, serverToClient := io.Pipe()
	s := &server{
		conn:        newConn(clientToServer, serverToClient),
		ws:          ws,
		log:         log.New(io.Discard, "", 0),
		overlay:     map[string][]byte{},
		maxOpenDocs: maxDocs,
		published:   map[string]bool{},
	}
	done := make(chan int, 1)
	go func() { done <- s.run() }()
	tc := &testClient{t: t, c: newConn(serverOut, serverIn), done: done}
	t.Cleanup(func() {
		_ = serverIn.Close()
		_ = serverOut.Close()
	})
	return tc
}

// readPublishes reads exactly n notifications and requires each to be a
// publishDiagnostics, returning them decoded. (The transport pipes are
// synchronous, so the client must drain what each notification
// produces before sending the next.)
func (tc *testClient) readPublishes(n int) []PublishDiagnosticsParams {
	tc.t.Helper()
	out := make([]PublishDiagnosticsParams, 0, n)
	for range n {
		m := tc.readNotification()
		if m.Method != "textDocument/publishDiagnostics" {
			tc.t.Fatalf("expected publishDiagnostics, got %q", m.Method)
		}
		out = append(out, decodeParams[PublishDiagnosticsParams](tc.t, m))
	}
	return out
}

// A hostile or buggy client streaming didOpen with ever-distinct URIs
// must not grow the tracked-document set (and the per-file memo behind
// Workspace.Check) without bound, nor buy a full-workspace re-check per
// refused message. Behavior at or under the cap is unchanged, and
// didClose frees a slot.
func TestServer_OpenDocumentCapRefusesAndStaysResponsive(t *testing.T) {
	const docCap = 4
	ws := &countingWS{}
	tc := startServerWithCap(t, ws, docCap)
	tc.initialize()

	open := func(path, text string) {
		t.Helper()
		tc.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{"uri": pathToURI(path), "languageId": "sql", "version": 1, "text": text},
		})
	}

	// Fill to the cap: normal behavior — every open re-checks and
	// publishes once per open document.
	for i := 0; i < docCap; i++ {
		open(fmt.Sprintf("/ws/queries/q%d.sql", i), "ok")
		tc.readPublishes(i + 1)
	}
	checksAtCap := ws.checks.Load()

	// Over the cap: the document is refused — one showMessage warning
	// for the whole session, no publish for the refused URIs, and no
	// workspace re-check (nothing in the snapshot changed).
	open("/ws/queries/over1.sql", "BAD")
	if m := tc.readNotification(); m.Method != "window/showMessage" {
		t.Fatalf("first refusal must showMessage, got %q", m.Method)
	}
	open("/ws/queries/over2.sql", "BAD")
	// A didChange for an untracked path goes through the same cap.
	tc.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": pathToURI("/ws/queries/over3.sql"), "version": 1},
		"contentChanges": []map[string]any{{"text": "BAD"}},
	})
	// Liveness barrier: the response proves all prior notifications were
	// handled (single goroutine, strict order); anything they emitted
	// would arrive before it. Refusals must emit nothing further — no
	// second warning, no publish for a refused URI.
	if _, notes := tc.request("initialize", map[string]any{}); len(notes) != 0 {
		t.Errorf("refused documents must emit nothing, got %d notifications: %+v", len(notes), notes)
	}
	if got := ws.checks.Load(); got != checksAtCap {
		t.Errorf("refused documents must not trigger re-checks: %d -> %d", checksAtCap, got)
	}

	// didClose frees a slot: the same document now tracks, checks, and
	// publishes its diagnostic — the cap never wedges a legit session.
	tc.notify("textDocument/didClose", map[string]any{
		"textDocument": map[string]any{"uri": pathToURI("/ws/queries/q0.sql")},
	})
	tc.readPublishes(docCap - 1) // q1..q3 remain open
	open("/ws/queries/over1.sql", "BAD")
	found := false
	for _, p := range tc.readPublishes(docCap) { // q1..q3 + over1
		if p.URI == pathToURI("/ws/queries/over1.sql") && len(p.Diagnostics) == 1 {
			found = true
		}
	}
	if !found {
		t.Error("after didClose freed a slot, the reopened document must publish its diagnostic")
	}
	tc.shutdownExit(0)
}

// Reopening an ALREADY-tracked document at the cap must not be refused:
// the overlay does not grow, so the cap does not apply.
func TestServer_OpenDocumentCapAllowsRetrackedDoc(t *testing.T) {
	tc := startServerWithCap(t, &countingWS{}, 1)
	tc.initialize()

	uri := pathToURI("/ws/queries/q.sql")
	openText := func(text string) {
		t.Helper()
		tc.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{"uri": uri, "languageId": "sql", "version": 1, "text": text},
		})
	}
	openText("ok")
	tc.readPublishes(1)
	openText("BAD") // same path at the cap: tracked, re-checked
	if p := tc.readPublishes(1)[0]; p.URI != uri || len(p.Diagnostics) != 1 {
		t.Errorf("re-opening a tracked document at the cap must update its content and publish: %+v", p)
	}
	tc.shutdownExit(0)
}
