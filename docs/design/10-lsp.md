# sqletch Design — 10: LSP Server (v0.4 editor support)

The editor-support milestone from 08 §"Editor support (LSP)":
a language server that wraps `check` incrementally so template
mistakes surface as you type, with the same `SQLETCHnnn` diagnostics
the CLI prints. (Doc 09 is reserved for the embedded-oracle spike
outcome; this doc takes the next number.)

## 1. Goals and non-goals

Goals:

- **Diagnostics on open/change/save** for template files, produced by
  the same scanner/rules/oracle-cache machinery as `sqletch check` —
  never a reimplementation, so LSP and CLI can not disagree.
- **Strictly offline**: the server NEVER opens a database connection
  or starts a container. Oracle-dependent checks (R3 resolution, type
  agreement, `@param` hint validation) run only when the committed
  cache already holds the catalog and every rendering of the query;
  otherwise they are silently skipped for that query. This is the
  editor-latency version of the cache contract in 04 §4: a cold cache
  degrades coverage, never correctness of what IS reported.
- **Incremental**: per-file scan/lexical/R1 results are memoized by
  content hash; an edit re-analyzes one file, cross-file checks
  (duplicate query names) recompute from memoized scans.
- **Go-to-definition for parameters** (spec: "go-to-definition for
  params/predicates"): a `:param` occurrence jumps to its
  `-- @param name: type` annotation when present, else to the
  parameter's first occurrence; the annotation jumps to the first
  occurrence. Predicates (`@predicate name:`) are themselves the
  definition sites and have no in-template references (they are
  referenced from Go code), so nothing to resolve there; cross-language
  navigation is out of scope.

Non-goals (deliberate, revisit with their own doc sections):

- No completion, hover, rename, formatting-on-save (fmt exists as a
  CLI; wiring it to `textDocument/formatting` is a later mechanical
  addition).
- No codegen-phase diagnostics (SQLETCH310/311 name collisions and
  type-mapping gaps): those need resolved param/column types for every
  query and are inherently generate-time; the CLI reports them.
- No nullability analysis (it feeds codegen, produces no diagnostics).
- No watching of schema/config files (`workspace/didChangeWatchedFiles`);
  the config and schema fingerprint are loaded per check cycle from
  disk, so an external `sqletch generate` run is picked up on the next
  edit without a server restart.
- The tree-sitter/TextMate grammars from 08 are a separate deliverable
  (no Go code); not part of this doc.

## 2. Placement

```
internal/lsp     protocol layer: JSON-RPC 2.0 framing (stdio), LSP
                 types subset, UTF-16 position mapping, server loop,
                 definition provider. Imports diagnostics + template
                 (+ config for workspace paths); stdlib only — no new
                 module dependencies.
internal/cli     the analysis seam: OfflineChecker (this doc §4) —
                 the offline prefix of pipeline.Run, factored for
                 per-file memoization. cli.LSP(...) glues config
                 loading + OfflineChecker + lsp.Serve.
cmd/sqletch      `sqletch lsp` — cobra wiring only, stdio transport.
```

Dependency direction: `cli → lsp` (cli constructs the server and
injects the checker as an `lsp.Workspace` implementation). `lsp` never
imports dialect drivers directly; per-dialect dispatch stays in cli's
`driver` table.

## 3. Protocol subset

JSON-RPC 2.0 over stdio with `Content-Length` framing (the only
mandatory header; unknown headers are skipped). Hand-rolled in
`internal/lsp/jsonrpc.go` — the message vocabulary is small enough
that a dependency is not worth its transitive weight.

Handled methods:

| method | behavior |
| --- | --- |
| `initialize` | capabilities: full-document sync, `definitionProvider`, `positionEncoding: "utf-16"` |
| `initialized` | if config loading failed at startup, send one `window/showMessage` (Error) with the rendered diagnostics (§6) |
| `textDocument/didOpen` | overlay content, run check cycle, publish |
| `textDocument/didChange` | full sync (`TextDocumentSyncKind.Full`); same |
| `textDocument/didSave` | re-run (cache/catalog may have changed on disk) |
| `textDocument/didClose` | drop overlay, re-run, clear diagnostics for the closed doc if it left the workspace |
| `textDocument/definition` | §5 |
| `shutdown` / `exit` | LSP lifecycle; `exit` returns from Serve (exit code 0 after shutdown, 1 otherwise) |
| `$/cancelRequest`, `$/setTrace` | ignored (all responses are fast) |
| other requests | `MethodNotFound` error response |
| other notifications | ignored |

Requests are answered in arrival order on a single goroutine: every
operation is an in-memory recompute over a small workspace, so
concurrency buys nothing and ordering bugs cost correctness
(determinism convention).

Positions: LSP `Position` is (line, UTF-16 code unit); spans are byte
offsets. `internal/lsp/position.go` builds a per-content line index
and converts both directions; multibyte (CJK + surrogate pairs) is
covered by tests per the adversarial-input convention. Span `End` is
exclusive and clamped to the file; a zero-length or out-of-range span
renders as a one-character range (mirrors `RenderExcerpt`'s caret
fallback).

## 4. The offline checker (cli seam)

```go
type OfflineChecker struct { /* cfg, driver, per-path memo */ }
func NewOfflineChecker(cfg config.Config) *OfflineChecker

type WorkspaceCheck struct {
    Diags   map[string][]diagnostics.Diagnostic // abs path → sorted diags
    Files   map[string]*template.QueryFile      // abs path → scan result
    Sources map[string][]byte                   // content actually analyzed
}
func (c *OfflineChecker) Check(overlay map[string][]byte) (WorkspaceCheck, error)
```

One `Check` call is one consistent snapshot; the server runs it per
didOpen/didChange/didSave/didClose and publishes per file. The file
set is `cfg.ExpandGlobs(cfg.Queries)` ∪ overlay keys (an open
unsaved buffer participates even before it matches a glob on disk).
Overlay content wins over disk. The error return is environmental
(unreadable non-overlay file) — the server logs it and keeps the last
published state.

Phases, mirroring pipeline.Run's order:

1. **Per file, memoized by SHA-256 of content**: `ScanFile`, then per
   query `CheckLexical`, `ast.Renderings`, `CheckR1`. The memo also
   keeps the renderings for phase 3. Scan errors that abort rendering
   for a query keep the scan diagnostics only (same short-circuit as
   the pipeline).
2. **Workspace**: duplicate query names (`SQLETCH004`), first
   definition wins in sorted-path order — same ordering the pipeline
   gets from `ExpandGlobs`, so the CLI and the LSP flag the same
   duplicate. Overlay-only paths sort in the same collation.
3. **Oracle-cached, per query, only if phases 1–2 left no errors for
   that file**: recompute the schema fingerprint from disk, load the
   catalog; for a query whose every rendering hits the oracle cache,
   run `CheckResolved` (parsing the maximal rendering through the
   dialect frontend — in-process, offline), Tier 1 type
   agreement/param resolution, `@param` hint validation, Tier 2
   missing-annotation checks. Any cache miss ⇒ skip the query's
   phase-3 checks entirely (partial oracle data could produce
   half-true agreement diagnostics).

Determinism: `Diags` per file are `diagnostics.Sort`ed; map iteration
never leaks into output (the server publishes per-URI, and tests
compare per-file slices).

## 5. Definition provider

Hit-testing uses the memoized scan of the requested file (no extra
parse). Given the byte offset of the cursor:

- inside a bind `Occurrence` of param `p` → `TypeHints[p].Span` if the
  query has an annotation for `p`, else `p`'s first occurrence;
- inside `TypeHints[p].Span` (the `-- @param` line) → `p`'s first
  occurrence;
- anything else → `null` (no error).

Spans of guard-list names inside construct headers
(`@if-present(a, b)`) are not recorded by the scanner (GuardAtom
carries no span); adding them is a scanner-model change and is
deferred until a feature actually needs sub-header navigation.

## 6. Startup and degraded mode

`sqletch lsp` honors the global `--config`. Config load failures do
not kill the server (clients auto-restart in a loop and the user gets
no message): the server starts, answers `initialize`, reports the
config diagnostics once via `window/showMessage` after `initialized`,
and then answers every request with empty results until restarted.
Environmental failures mid-session (unreadable file) are logged to
stderr; the last good diagnostics stay published.

## 7. Testing

- Framing: round-trip, multi-header parse, missing Content-Length,
  EOF mid-message.
- Positions: offset↔UTF-16 both directions over ASCII, CJK, emoji
  (surrogate pair), CRLF; out-of-range clamps.
- Server: in-memory duplex pipe, scripted client. Lifecycle
  (initialize → capabilities; shutdown/exit codes); didOpen with a
  broken template → `publishDiagnostics` carrying the `SQLETCHnnn`
  code and the expected range; didChange fixing it → empty publish;
  definition round-trips (occurrence → hint, hint → first occurrence,
  miss → null). Server tests run against the real OfflineChecker over
  a temp project (postgres profile; scanner/R1 only — no DB).
- OfflineChecker: cold cache reports scanner/lexical/R1 and duplicate
  names but no 1xx resolution/2xx agreement diagnostics; a warm cache
  (catalog + oracle entries written through `internal/cache.Store`)
  enables phase 3 and reports a resolution error; overlay content
  overrides disk; memoization observed via a scan counter hook is NOT
  asserted (implementation detail) — only snapshot equivalence.
