package lsp

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"runtime/debug"
	"sort"

	"github.com/moznion/go-sqletch/internal/diagnostics"
	"github.com/moznion/go-sqletch/internal/template"
)

// Workspace is the analysis seam: internal/cli injects its
// OfflineChecker so the protocol layer stays free of dialect drivers.
// Check runs one consistent offline snapshot with overlay (open
// buffer) contents replacing disk.
type Workspace interface {
	Check(overlay map[string][]byte) (WorkspaceResult, error)
}

// WorkspaceResult mirrors cli.WorkspaceCheck, keyed by absolute path.
type WorkspaceResult struct {
	Diags   map[string][]diagnostics.Diagnostic
	Files   map[string]*template.QueryFile
	Sources map[string][]byte
}

// Serve runs the language server until the client's exit notification
// or a transport failure, returning the process exit code (0 after an
// orderly shutdown, 1 otherwise). ws == nil serves the degraded mode
// of docs/design/10-lsp.md §6: initErr is reported once via
// window/showMessage and every request answers empty.
func Serve(in io.Reader, out io.Writer, ws Workspace, initErr string, logW io.Writer) int {
	s := &server{
		conn:        newConn(in, out),
		ws:          ws,
		initErr:     initErr,
		log:         log.New(logW, "sqletch-lsp: ", 0),
		overlay:     map[string][]byte{},
		maxOpenDocs: maxOpenDocuments,
		published:   map[string]bool{},
	}
	return s.run()
}

type server struct {
	conn    *conn
	ws      Workspace
	initErr string
	log     *log.Logger

	overlay map[string][]byte // open documents, path → content
	// maxOpenDocs is the overlay cap (maxOpenDocuments; a field so
	// tests can lower it).
	maxOpenDocs int
	// overlayCapWarned makes the over-cap refusal a showMessage exactly
	// once per session (the log line still fires per refusal).
	overlayCapWarned bool
	// published tracks paths whose last publish was non-empty, so a
	// fix (or a file leaving the workspace) clears the client's state.
	published map[string]bool
	last      WorkspaceResult
	shutdown  bool
}

// maxOpenDocuments caps the number of documents tracked in the overlay.
// The overlay is fed exclusively by client notifications and evicted
// only on didClose, so a hostile or buggy client streaming didOpen with
// ever-distinct URIs would otherwise grow it — and the per-file memo
// behind Workspace.Check — without bound, with a full-workspace
// re-check per message on top (O(N²) CPU across the stream). The cap is
// far above what a real editor session opens; past it a NEW document is
// refused (logged, one showMessage) rather than silently evicting an
// open one — dropping a tracked buffer would make a legit client's
// diagnostics silently reflect stale disk content, which is worse than
// degrading the excess documents to their on-disk state. A refusal also
// skips the re-check: nothing in the snapshot's inputs changed.
// The server field maxOpenDocs carries it so tests can lower it.
const maxOpenDocuments = 4096

// trackOverlay records an open document's content, refusing paths that
// would grow the overlay past s.maxOpenDocs. Refused documents are
// simply not tracked: checks fall back to their on-disk content (the
// config glob still reaches them), and didClose of a tracked document
// frees its slot.
func (s *server) trackOverlay(path string, content []byte) bool {
	if _, tracked := s.overlay[path]; !tracked && len(s.overlay) >= s.maxOpenDocs {
		s.log.Printf("open-document cap (%d) reached; not tracking %s (its on-disk content is checked instead)",
			s.maxOpenDocs, path)
		if !s.overlayCapWarned {
			s.overlayCapWarned = true
			s.notify("window/showMessage", ShowMessageParams{
				Type: messageWarning,
				Message: fmt.Sprintf(
					"sqletch: more than %d open documents; further documents are checked from disk only",
					s.maxOpenDocs),
			})
		}
		return false
	}
	s.overlay[path] = content
	return true
}

// run processes messages strictly in arrival order on one goroutine:
// every operation is an in-memory recompute, so concurrency would buy
// latency only at the price of ordering bugs.
func (s *server) run() int {
	for {
		msg, err := s.conn.read()
		if err != nil {
			if errors.Is(err, errMalformedBody) {
				// JSON-RPC 2.0: a body that fails to parse gets a
				// -32700 response with a null id; the frame boundary
				// is intact, so keep serving.
				s.log.Printf("read: %v", err)
				s.writeErr(rawID("null"), codeParseError, err.Error())
				continue
			}
			if !errors.Is(err, io.EOF) {
				s.log.Printf("read: %v", err)
			}
			return 1
		}
		if msg.Method == "exit" {
			if s.shutdown {
				return 0
			}
			return 1
		}
		s.dispatch(msg)
	}
}

func (s *server) dispatch(msg *Message) {
	// Per-message panic isolation. Handling a message runs the whole
	// analysis stack (scanner, policy weaver, rules, pg_query cgo
	// bindings) on attacker-controlled buffer contents; a panic anywhere
	// in it must not kill the process. Editors auto-restart their LSP
	// server, so a fatal panic replaying the same hostile buffer degrades
	// into a persistent restart loop (a weaponizable editor hang). Catch
	// the panic here, log it, and — for a request — answer a JSON-RPC
	// internal error (-32603) so the client is not left waiting; a
	// notification has no reply, so it is swallowed. The message frame is
	// already fully read, so the connection framing is intact and the run
	// loop keeps serving subsequent messages. This never fires on a
	// normal error return: analysis reports those as diagnostics or via
	// the degraded-config path, both of which return cleanly.
	defer func() {
		if r := recover(); r != nil {
			s.log.Printf("recovered panic handling %q: %v\n%s", msg.Method, r, debug.Stack())
			if msg.ID != nil {
				s.writeErr(msg.ID, codeInternalError, "internal error")
			}
		}
	}()

	// A message with no method is a response (result/error to a request)
	// or otherwise not a call. This server issues no requests, and
	// JSON-RPC has no reply to a response — so drop it rather than
	// answering MethodNotFound to an empty method name.
	if msg.Method == "" {
		return
	}
	switch msg.Method {
	case "initialize":
		s.respond(msg.ID, InitializeResult{
			Capabilities: ServerCapabilities{
				PositionEncoding:   "utf-16",
				TextDocumentSync:   syncFull,
				DefinitionProvider: true,
			},
			ServerInfo: ServerInfo{Name: "sqletch"},
		})
	case "initialized":
		if s.initErr != "" {
			s.notify("window/showMessage", ShowMessageParams{Type: messageError, Message: s.initErr})
		}
	case "shutdown":
		s.shutdown = true
		s.respond(msg.ID, nil)
	case "textDocument/didOpen":
		var p DidOpenParams
		if path, ok := s.docParams(msg, &p, func() string { return p.TextDocument.URI }); ok {
			if s.trackOverlay(path, []byte(p.TextDocument.Text)) {
				s.check()
			}
		}
	case "textDocument/didChange":
		var p DidChangeParams
		if path, ok := s.docParams(msg, &p, func() string { return p.TextDocument.URI }); ok {
			if n := len(p.ContentChanges); n > 0 {
				// A didChange for an untracked path (a client that skips
				// didOpen) tracks it here, so it goes through the same cap.
				if !s.trackOverlay(path, []byte(p.ContentChanges[n-1].Text)) {
					break
				}
			}
			s.check()
		}
	case "textDocument/didSave":
		// Content is already in the overlay; re-run because the cache
		// or catalog on disk may have moved under us.
		s.check()
	case "textDocument/didClose":
		var p DidCloseParams
		if path, ok := s.docParams(msg, &p, func() string { return p.TextDocument.URI }); ok {
			delete(s.overlay, path)
			s.check()
		}
	case "textDocument/definition":
		s.definition(msg)
	case "$/cancelRequest", "$/setTrace":
		// Every response is fast; nothing to cancel.
	default:
		if msg.ID != nil {
			s.writeErr(msg.ID, codeMethodNotFound, "unsupported method "+msg.Method)
		}
	}
}

// docParams decodes a document notification's params and resolves its
// URI; protocol-level garbage is logged and dropped, never fatal.
func (s *server) docParams(msg *Message, into any, uri func() string) (string, bool) {
	if err := jsonv2.Unmarshal(msg.Params, into); err != nil {
		s.log.Printf("%s: %v", msg.Method, err)
		return "", false
	}
	path, err := uriToPath(uri())
	if err != nil {
		s.log.Printf("%s: %v", msg.Method, err)
		return "", false
	}
	return path, true
}

func (s *server) check() {
	if s.ws == nil {
		return
	}
	res, err := s.ws.Check(s.overlay)
	if err != nil {
		// Environmental: keep the last published state rather than
		// flickering diagnostics away.
		s.log.Printf("check: %v", err)
		return
	}
	s.last = res

	// Publish for every open document (even when clean, to clear the
	// client's state), every file with diagnostics, and every file
	// whose previous publish was non-empty.
	targets := map[string]bool{}
	for p := range s.overlay {
		targets[p] = true
	}
	for p, ds := range res.Diags {
		if len(ds) > 0 {
			targets[p] = true
		}
	}
	for p := range s.published {
		targets[p] = true
	}
	paths := make([]string, 0, len(targets))
	for p := range targets {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	published := map[string]bool{}
	for _, p := range paths {
		ds := res.Diags[p]
		src := res.Sources[p]
		// One line index per file, shared across all its diagnostics:
		// converting each span rescanned the whole buffer from offset 0
		// before, making a large file with many diagnostics quadratic.
		pm := newPosMapper(src)
		truncated := 0
		if len(ds) > maxPublishedDiagnostics {
			truncated = len(ds) - maxPublishedDiagnostics
			ds = ds[:maxPublishedDiagnostics]
		}
		out := make([]Diagnostic, 0, len(ds))
		for _, d := range ds {
			out = append(out, toLSPDiagnostic(pm, d))
		}
		// Defense in depth: an adversarial or pathological file must not
		// flood the client with an unbounded frame. A trailing summary
		// keeps the truncation visible rather than silently complete.
		if truncated > 0 {
			out = append(out, Diagnostic{
				Range:    pm.spanRange(0, 0),
				Severity: severityWarning,
				Source:   "sqletch",
				Message: fmt.Sprintf(
					"%d further diagnostics suppressed (over the %d-per-file publish cap)",
					truncated, maxPublishedDiagnostics),
			})
		}
		s.notify("textDocument/publishDiagnostics", PublishDiagnosticsParams{
			URI: pathToURI(p), Diagnostics: out,
		})
		if len(out) > 0 {
			published[p] = true
		}
	}
	s.published = published
}

// maxPublishedDiagnostics bounds the diagnostics converted and sent for
// one file in a single publish, so a pathological file cannot produce an
// unbounded outbound frame. The scanner-side cap on diagnostic
// generation is a separate concern; this is the transport backstop.
const maxPublishedDiagnostics = 2000

func toLSPDiagnostic(pm *posMapper, d diagnostics.Diagnostic) Diagnostic {
	sev := severityError
	if d.Severity == diagnostics.Warning {
		sev = severityWarning
	}
	msg := d.Message
	if d.Hint != "" {
		msg += "\nhelp: " + d.Hint
	}
	return Diagnostic{
		Range:    pm.spanRange(d.Span.Start, d.Span.End),
		Severity: sev,
		Code:     string(d.Code),
		Source:   "sqletch",
		Message:  msg,
	}
}

func (s *server) definition(msg *Message) {
	// definition is a request; a notification-shaped one (no id) is
	// malformed. Replying is wrong twice over — JSON-RPC forbids a reply
	// to a notification, and an error response with no id member (id is
	// omitempty) is not a valid message — so drop it.
	if msg.ID == nil {
		return
	}
	var p DefinitionParams
	if err := jsonv2.Unmarshal(msg.Params, &p); err != nil {
		s.writeErr(msg.ID, codeInvalidParams, err.Error())
		return
	}
	path, err := uriToPath(p.TextDocument.URI)
	if err != nil {
		s.respond(msg.ID, nil)
		return
	}
	file, src := s.last.Files[path], s.last.Sources[path]
	if file == nil || src == nil {
		s.respond(msg.ID, nil)
		return
	}
	pm := newPosMapper(src)
	span, ok := definitionAt(file, pm.offset(p.Position))
	if !ok {
		s.respond(msg.ID, nil)
		return
	}
	s.respond(msg.ID, Location{URI: pathToURI(path), Range: pm.spanRange(span.Start, span.End)})
}

// definitionAt implements docs/design/10-lsp.md §5: a parameter
// occurrence resolves to its `-- @param` annotation when the query has
// one, else to the parameter's first occurrence; the annotation
// resolves to the first occurrence. Spans within one file are
// disjoint, so at most one target matches.
func definitionAt(file *template.QueryFile, off int) (diagnostics.Span, bool) {
	contains := func(sp diagnostics.Span, off int) bool {
		return off >= sp.Start && off < sp.End
	}
	for _, q := range file.Queries {
		for name, hint := range q.TypeHints {
			if !contains(hint.Span, off) {
				continue
			}
			if p := q.Params[name]; p != nil && len(p.Occurrences) > 0 {
				return p.Occurrences[0].Span, true
			}
			return diagnostics.Span{}, false
		}
		for _, name := range q.ParamOrder {
			p := q.Params[name]
			if p == nil {
				continue
			}
			for _, occ := range p.Occurrences {
				if !contains(occ.Span, off) {
					continue
				}
				if hint, ok := q.TypeHints[name]; ok {
					return hint.Span, true
				}
				return p.Occurrences[0].Span, true
			}
		}
	}
	return diagnostics.Span{}, false
}

func (s *server) respond(id *json.RawMessage, result any) {
	if id == nil {
		return
	}
	if err := s.conn.writeResponse(id, result); err != nil {
		s.log.Printf("write response: %v", err)
	}
}

func (s *server) writeErr(id *json.RawMessage, code int, message string) {
	if err := s.conn.writeError(id, code, message); err != nil {
		s.log.Printf("write error: %v", err)
	}
}

func (s *server) notify(method string, params any) {
	if err := s.conn.writeNotification(method, params); err != nil {
		s.log.Printf("write %s: %v", method, err)
	}
}
