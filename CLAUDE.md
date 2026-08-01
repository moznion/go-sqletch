# CLAUDE.md

sqletch is a compiler for a restricted conditional-SQL template
language: templates compile to typed Go where every reachable query
shape is statically verified and runtime work is deterministic
composition of pre-verified constant fragments.

## Document authority

1. **`docs/spec.md` is the specification** — the structural
   rules R1–R9, runtime premises P1/P2, the soundness argument, and
   the design boundary. It survived multiple adversarial review
   cycles; do not weaken a rule to make an implementation easier.
2. **`docs/design/00–08` is the implementation design**, phased P1–P7
   (all implemented for v0.1) plus later-phase deltas.
3. On any contradiction or gap between spec, design, and code:
   **ask the user before deviating.** Minor mechanical choices may be
   made directly but must be reflected back into the design docs.

## Commands

```console
go test ./...                              # unit suites (no DB)
go test -tags devdb ./internal/e2e/        # real-DB E2E (needs Docker or SQLETCH_TEST_DSN)
golangci-lint run --build-tags devdb ./... # must be 0 issues before "done"
goimports -w .                             # run after every change
go run ./cmd/sqletch generate --config examples/postgres/sqletch.yaml
go test ./internal/template -run '^$' -fuzz=FuzzScan -fuzztime=15s
go test ./internal/codegen  -run '^$' -fuzz=FuzzComposeConformance -fuzztime=15s
```

Both fuzz targets run in CI for 30s. A crasher is written to the
package's `testdata/fuzz/<target>/`; commit it — that file *is* the
regression test.

## Working conventions (user-mandated)

- **Test-first; tests are the spec.** Every layer gets thorough tests;
  rejected inputs are asserted down to their `SQLETCHnnn` code. Never
  report a task complete with failing or missing tests.
- **Real-DB E2E is first-class.** The devdb suite must stay green: the
  property test (every enumerable shape PREPAREs and EXPLAINs), the
  generated-module run (NULL-heavy seed data scanning into generated
  structs), and the CLI cold→warm offline round-trip. Extend it when
  adding features; seed data should be adversarial (NULLs, empty
  results, multibyte).
- **goimports + golangci-lint on every change.**
- **Commit per completed phase/feature**, message ends with the
  Co-Authored-By trailer.

## Architecture (pipeline order)

```
internal/template   P1  scanner: constructs, spans, params (lexer via
                        dialect.LexerProfile; postgres profile in
                        internal/dialect/postgres/lexer.go)
internal/gosrc      —   templates authored in `//sqletch:query` consts
                        inside .go files (doc 13); go/parser only, and
                        it hands the scanner offset-preserving views so
                        internal/template needs no notion of Go at all
                        (cli.scanSource is the single dispatch)
internal/ast        P2  Render/RenderShape + SourceMap — the reference
                        emission (premise P2)
internal/rules      P2/3/4  CheckR1 (probe-based node completeness),
                        CheckLexical (R6, R9), CheckResolved (R3
                        resolution-based, R2 star, planner table),
                        CheckTypeAgreement/ResolveParamTypes (P1 types)
internal/dialect    —   driver interfaces; postgres/ = pg_query
                        frontend + pgx Describe oracle + TypeMap
internal/cache      P4  committed fingerprint-keyed store (offline)
internal/devdb      P4  DSN or testcontainers; DISPOSABLE by contract
                        (resets public schema when applying schema)
internal/nullability P5 skeleton-only narrowing discipline
internal/codegen    P6  BuildFrags + Go emission
runtime/            P6  PUBLIC package: Compose mirrors ast.Render
internal/config,cli P7  sqletch.yaml + generate/check/explain pipeline;
                        OfflineChecker = the LSP's analysis seam
internal/lsp        —   language server (doc 10): JSON-RPC framing,
                        LSP subset, UTF-16 positions; stdlib only,
                        checker injected via the Workspace interface
cmd/sqletch         P7  cobra wiring only
```

Only `internal/dialect/postgres` may import pg_query/pgx (plus
`runtime`/generated code for pgx binding).

## Load-bearing invariants — do not break

- **Compose conformance**: `runtime.Compose` over `codegen.BuildFrags`
  must be byte-identical to `ast.RenderShape` for every shape, with
  identical bind order. `TestComposeConformance` pins it; any emission
  change must touch both sides.
- **Nullability never narrows from guarded fragments.** The analyzer
  intentionally ignores guarded joins/predicates (review
  counterexamples F1a/F1b are permanent must-stay-nullable tests).
  "Guarded INNER join implies non-null FK" is per-shape UNSOUND.
- **Determinism everywhere**: byte-identical outputs for identical
  inputs (renderings, cache JSON, generated Go, composed SQL). Never
  range over a map into output without sorting.
- **Diagnostics carry stable codes** (`SQLETCH0xx` scanner, `1xx`
  rules, `2xx` oracle, `3xx` codegen/config) and template-file spans
  via the source map; messages state the rule *and its rationale*,
  hints show the compliant rewrite.
- **Hashes are an index, never identity**: cache entries store full
  keys and compare on read; the runtime LRU compares full shape keys.

## Known v0.2/v0.3 decisions and limits

- @when literals: string/integer/boolean; `=`/`!=` (`<>` alias);
  modeled as IfPresent items carrying value atoms so all downstream
  machinery is shared.
- @filter-tree: one block per query, WHERE-conjunct slot only (local
  v0.3 restrictions, documented in the spec); caps configurable via
  `filter_tree_caps` and baked into generated code; predicate params
  are constructor arguments, never struct fields; composition caches
  bind PLANS (positions), never values.
- @order-by: verification = maximal + @default renderings only; the
  full permutation space is enumerated for exhaustive/property checks.
- `EXPLAIN (GENERIC_PLAN)` output must go through pgconn's raw simple
  query (pgx's Query/Exec layers reject bare $n placeholders).

## Known v0.4 decisions and limits

- `-- @param name: type` hints: parsed per query (the comment stays in
  the skeleton verbatim); resolved via the dialect's `TypeByName`
  (lowercased, length args stripped); unknown param or type name →
  diagnostic. On Tier 2 they SUPPLY the type (mandatory). On Tier 1
  they ASSERT it: an annotation that disagrees with the oracle's
  inferred OID is SQLETCH213 and the oracle's type wins — never a
  silent override, because the oracle types the rendering and never
  sees the annotation, so a wrong hint is invisible to every other
  phase (it used to reach codegen and fail only at execution time).
  Diagnostics iterate `q.TypeHints` in sorted order (determinism).
  `postgres.TypeMap.WritableName` is TypeByName's inverse, used to
  spell the fix (`_text` → `text[]`).
- `@in(:param)`: v0.4-1 supports depth-0 WHERE/HAVING skeleton
  positions only; inside guarded bodies it is rejected with a
  diagnostic pointing at the PostgreSQL workaround (`= ANY(:param)`).
  PG rendering is `= ANY($n)` — one static shape, no arity dimension.
- MySQL driver (Tier 2): placeholder style is a dialect property
  (`dialect.PlaceholderStyle`); question style emits one '?' per
  occurrence and repeats binds. @in arity is a shape-key dimension
  (`;n=` segment); verification quotients arities to {non-empty≡1, 0}
  because IN-list growth is parse-invariant; arity 0 renders
  `IN (SELECT NULL FROM DUAL WHERE FALSE)` (FALSE even for NULL
  operands, matching PG). Generated MySQL code uses database/sql,
  GetBindsStyle + ResolveArgs (Bind.Elem selects slice elements).
- TiDB parser: expression nodes carry byte offsets, relation nodes do
  NOT — relation locations are recovered lexically (FROM-position
  predecessors: FROM/JOIN/','/'('/'.'/INTO/UPDATE; subqueries skipped
  whole). Parser needs the test_driver blank import.
- MySQL oracle: COM_STMT_PREPARE column metadata is reliable
  (org_table/org_name resolve to synthetic-OID catalog positions);
  parameter slots are untyped — `-- @param` annotations are MANDATORY
  for every bind parameter (control-only params exempt). Plan =
  prepared `EXPLAIN` executed with all-NULL params (plans without
  touching data). TypeRef.OID encodes wire type code + unsigned/binary
  flag bits; TEXT vs BLOB splits on the binary charset (63).
- go-mysql client: ExecuteMultiple hides per-statement errors (only
  the callback sees them) — devdb splits schema SQL on top-level
  semicolons via the lexer profile and executes one at a time.
- SQLite driver (Tier 2): fully in-process (ncruces/go-sqlite3 =
  SQLite-as-WASM under wazero; no Docker). Frontend = rqlite/sql
  (byte offsets everywhere; no RIGHT/FULL JOIN; some non-reserved
  keywords like ACTION need quoting). Prepare IS the plan check;
  oracle errors carry offsets and the engine survives them. Expression
  columns (count(*) etc.) have no decltype — `-- @column name: type`
  annotations are MANDATORY for them (new scanner directive); params
  need `-- @param` like MySQL. Affinity mapping has BOOLEAN/date-time
  carve-outs. @in arity-0 emission is per-dialect via Frag.Text /
  dialect.InEmptySQL (SQLite: `IN (SELECT NULL WHERE 0)`); devdb DSN
  is a file path resolved config-relative via `cli.sqliteDSNPath`
  (`:memory:` and `file:` URIs pass through), reset = drop all
  tables/views, version pin compares dotted prefix ("3.50" vs
  "3.50.x").
- Version-pin mismatch is a DIAGNOSTIC, not an environment error:
  `devdb.VersionMismatchError` (which names its own engine — it is
  shared by all three dialects) maps to SQLETCH200 against
  `config.Config.Path` via `cli.versionPinDiag`, in both pipeline.Run
  and explain --analyze. Exit 1, and it reaches `--format json`/LSP.
- Editor grammars (doc 11, editors/): TextMate INJECTION grammar into
  source.sql (selector excludes string|comment so directive-shaped
  comments still win at line start); tree-sitter grammar keeps SQL as
  opaque sql_token + combined injection; comment-shaped directives
  beat the generic comment token via lexical precedence. Tests run
  via npx (tree-sitter-cli corpus, vscode-tmgrammar-test) in the CI
  grammars job; generated parser src/ is committed and diffed in CI.
- Embedded-PG WASM spike (docs/design/09, harness spike/wasm-oracle):
  feasible — unmodified pgx oracle over libpglite WASI PG 16.6 under
  wazero, warm cold-start 1.7s. NOT shipped: today's build aborts the
  whole instance on any PG error (sjlj off; clear_error is
  emscripten-only), mitigated only by ~0.5s reboot + fresh data dir.
  Transport is the socket-FILE pump (.s.PGSQL.5432.in/.out), not CMA;
  session bootstrap must CREATE SCHEMA public (template1 lacks it).
  Revisit when upstream libpglite ships official bindings.
- LSP (`sqletch lsp`, doc 10): STRICTLY offline — never opens a DB;
  catalog-dependent checks run only when the committed cache holds the
  catalog AND every rendering of the query (any miss skips the query's
  pass wholesale). Per-file scan/lexical/R1 memoized by content hash;
  duplicate-name check uses sorted-path first-wins like the pipeline.
  `cli.resolvedChecks` is the single shared catalog-dependent pass for
  pipeline.Run and OfflineChecker — extend it, don't fork it. Broken
  config ⇒ degraded server (showMessage once), never a crash loop.
  internal/lsp stays stdlib-only; positions are UTF-16 code units.

## Known v0.1 decisions and limits (documented, revisit deliberately)

- `EXPLAIN (GENERIC_PLAN)` requires PostgreSQL 16+.
- R3 skips unqualified column refs inside subquery scopes
  (innermost-first resolution unmodeled); qualified refs are checked
  everywhere. The exhaustive/property tests are the backstop.
- Indeterminate-parameter detection covers SQLSTATE 42P18 and 42725;
  bare `SELECT $1` is NOT an error on modern PostgreSQL.
- The numeric OID maps to float64 (lossy, documented in typemap.go).
- Dev databases are disposable: schema application drops and recreates
  the public schema.
