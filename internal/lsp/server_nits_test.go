package lsp

import "testing"

// A request method sent as a NOTIFICATION (no id) with malformed params
// must be dropped: JSON-RPC forbids replying to a notification, and an
// error response with no id member (id is omitempty) is not a valid
// message. The following didOpen's publish must be the next thing on the
// wire -- no stray error for the dropped notification.
func TestServer_DefinitionAsNotificationDropped(t *testing.T) {
	tc := startServer(t, &fakeWS{}, "")
	tc.initialize()

	tc.notify("textDocument/definition", map[string]any{"textDocument": 42})

	uri := pathToURI("/ws/q.sql")
	tc.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": "sql", "version": 1, "text": "xx BAD yy"},
	})
	msg := tc.readNotification()
	if msg.Method != "textDocument/publishDiagnostics" {
		t.Fatalf("definition notification must be dropped; next message method=%q error=%+v", msg.Method, msg.Error)
	}
	tc.shutdownExit(0)
}

// An unsolicited response-shaped message (no method, an id) is not a
// call. The server issues no requests, so it must be dropped, never
// answered with MethodNotFound (which the empty method name would
// otherwise produce).
func TestServer_UnsolicitedResponseDropped(t *testing.T) {
	tc := startServer(t, &fakeWS{}, "")
	tc.initialize()

	tc.writeRawBody(`{"jsonrpc":"2.0","id":99,"result":null}`)

	uri := pathToURI("/ws/q.sql")
	tc.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": "sql", "version": 1, "text": "xx BAD yy"},
	})
	msg := tc.readNotification()
	if msg.Method != "textDocument/publishDiagnostics" {
		t.Fatalf("unsolicited response must be dropped; next message method=%q error=%+v", msg.Method, msg.Error)
	}
	tc.shutdownExit(0)
}
