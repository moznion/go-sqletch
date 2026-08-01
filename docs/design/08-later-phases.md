# sqletch Design — 08: Later Phases (v0.2 – v0.4, implementation deltas)

Outline-level design for post-v0.1 work: what each feature touches per
component, plus the decisions already fixed by PROJECT_INSTRUCTION so
future implementers don't re-litigate them. Each item gets a full
design doc (this series' numbering continues) before its milestone
starts; this file records the deltas and the landmines.

## v0.2 — the second half of everyday dynamic SQL

### UPDATE SET slot (`@if-present` on SET items)

- scanner (01): `Set` context accepts `@if-present`; separator lifting
  gains `SepComma` (body must start with `,` unless first item).
- rules (03): R6 anchor check — at least one unconditional SET item;
  minimal-shape validity is *structural* (spec: `SQLETCH113` sibling,
  new code `SQLETCH118`).
- render/runtime (02/06): comma joining for SET lists. Item wrapping:
  **no parentheses** around SET items (unlike conjuncts — a SET item
  `col = expr` is not parenthesizable); node-completeness probe:
  `UPDATE sqletch_probe_t SET <item>` must parse with exactly one
  target.
- nullability (05): untouched (RETURNING is skeleton, R2).

### INSERT column/value pairs (R7)

- scanner: `InsertColumns`/`InsertValues` contexts; pairing by
  *position within guard group* collected lexically.
- rules: R7 full algorithm — for guard set G, the guarded column items
  and guarded value items must be equinumerous and positionally
  aligned (compare ordinal-within-clause); multi-row `VALUES` requires
  the pairing in every row. Diagnostic `SQLETCH119`.
- oracle/codegen: nothing structural — but emit the spec'd warning
  when an optional column is `NOT NULL` without default
  (`SQLETCH212`, warning severity; catalog has `HasDefault`).

### `@choose` in projection and GROUP BY

- scanner: `Projection`/`GroupBy` contexts accept `@choose`; the
  projection form is expression-level (`@choose … @end AS alias`) —
  the **alias stays in the skeleton** (R8; scanner enforces the `AS`
  following `@end`).
- rules: R8 becomes real — name-definition tracking per case; case
  bodies may not define names (`SQLETCH120`).
- oracle (04): cross-rendering agreement per case already exists;
  **nullability union** lands in 05: run `Analyze` per case rendering,
  OR the results per column (the spec's nullable-most rule).
- Landmine (from review): type agreement is by OID; two cases with
  same OID but different nullability is exactly why the union exists.

### Strict static expansion + `explain --enumerate` + `fmt`

- expansion: `shape.Enumerate` × `ast.Render` → `.sql` files +
  dispatch table instead of compose; per-query config
  `static_expansion: true` with a shape-count ceiling
  (`SQLETCH302` if exceeded). Excluded automatically for future
  tree/arity constructs.
- `fmt`: scanner round-trip (01 §9 invariant makes this cheap) +
  canonical layout + `WHERE TRUE` anchor insertion. fmt must be
  fixpoint (fmt∘fmt = fmt; golden-tested).

## v0.3 — expressiveness within the model

### `@when(param op literal)`

- template model: `GuardAtom{Param, Op, Value}` — the v0.1 struct
  already reserves the fields; shape bits allocated per *atom*.
- rules: R3 atom equality is already atom-based (03 §2). New checks:
  all `@when` fragments included in maximal rendering (existing path);
  structural-conflict diagnostic when mutually exclusive joins share
  an alias (`SQLETCH121`, "use distinct aliases").
- codegen: guard evaluation compiles to `arg.Mode == "a"` etc.; typed
  from the literal; if the param also binds, 04's agreement check
  covers literal-type vs SQL-type (`SQLETCH211` reused).

### `@filter-tree`

- runtime: `Tree` value type (And/Or/Leaf), canonical encoding
  (preorder, ordinals + arity) as cache key — full-encoding compare on
  LRU hit; caps (nodes/depth) checked before composition
  (`ErrTreeTooLarge`).
- required mode: `@filter-tree!(param)` — zero-value tree returns
  `ErrFilterRequired` before composition; codegen emits an explicit
  `<Query>Unscoped()` constructor (renders `TRUE`) as the sole opt-out
  path. Scanner: the `!` is part of the construct token
  (`@filter-tree!(`). Motivation and the repository/use-case layering
  pattern are documented in PROJECT_INSTRUCTION Use Case 5; the
  example project gains a multi-tenant repository sample.
- compose: every predicate and subtree parenthesized (P2); placeholder
  numbering per *occurrence* — `Frag.ParamSpans` machinery already
  supports repeated emission, but `Bind` gains per-occurrence value
  slots (leaf carries its own bound values via the typed
  constructors).
- rules: predicates have empty guard sets (03 §2 path); probe each
  predicate body as an expression (02 machinery).
- oracle: maximal rendering conjoins all predicates (existing).
- codegen: per-predicate typed constructors + `gen.And/Or`.

### `@order-by`

- Verified renderings: maximal (all keys, declaration order) **plus
  the `@default` body** (spec fix from review — it is a rendering,
  not a structural freebie).
- rules: reject under `DISTINCT ON` (`SQLETCH122`); require
  `@default` when skeleton has `FETCH FIRST … WITH TIES`
  (`SQLETCH123`) — both detectable from the tree facade
  (`HasDistinctOn`, add `HasFetchWithTies`).
- runtime: key-sequence composition (the one sanctioned reordering,
  P2); shape key gains the key sequence.

### HAVING slot, `explain --analyze`

Mechanical extensions of existing paths (conjunct machinery; the
`--exhaustive` harness with output capture).

## v0.4 — dialect breadth and adoption

### Tier 2 drivers (MySQL, SQLite)

- New lexer profiles (01): MySQL backtick idents, `@var`/`@@sysvar`
  passthrough, `#` comments; SQLite `[ident]`/backtick.
- Frontends: TiDB parser (MySQL), SQLite grammar via
  `sqlite3_prepare` probe-based checks (no Go AST lib needed if
  probes + limited tree facade suffice — decide in that phase's doc).
- Oracles: `COM_STMT_PREPARE` metadata (MySQL; params from
  annotations — new annotation parser in scanner, `-- @param name:
  type` directives feed `TypeRef` directly); `sqlite3_prepare` +
  decltype.
- The conformance suite (`testdata/`) is the gate: same templates,
  per-dialect goldens.

### `@in`

- Postgres: rewrite to `= ANY($n)`, single shape (no runtime delta).
- Expanding dialects: arity in shape key; placeholder-run synthesis is
  a new P2-vocabulary connective (`IN (?, ?, …)`); empty list →
  literal `FALSE`. Compose-conformance test extended over arities.

### Embedded PostgreSQL oracle backend

- New `Oracle` implementation (04 §1 seam): wazero-hosted WASM build
  of PostgreSQL (PGlite-class), or auto-fetched native binaries as
  fallback. `server_version` pins the embedded build. devdb selection
  becomes `database.backend: server | embedded`.
- Explicit spike before committing: WASM PG maturity, extension
  availability, cold-start time budget (< 2s target). Outcome of the
  spike updates this section into doc 09.

### Editor support (LSP)

- The `--format json` diagnostics (07) are the wire format; the LSP
  server wraps `check` incrementally (per-file scan is already
  independent; oracle hits cache). tree-sitter grammar for the
  template constructs layered over SQL highlighting.
- Full design: doc 10 (`sqletch lsp`, `internal/lsp` +
  `cli.OfflineChecker`). Grammars remain unscheduled.

## Beyond 1.0 (recorded, unscheduled)

- Native-inference oracle backend, differential-tested against the
  accumulated `(schema fp, rendered SQL, Desc)` corpus that every
  cache entry and conformance run produces — the corpus format (04 §3)
  is already exactly the required test-case triple; nothing extra to
  collect. First candidate: MySQL (no embeddable real engine).
- Policy weaving/enforcement (tenant scoping): config-declared
  predicate expanded into matching queries **after P1 scan, before
  rendering** (pure pre-verification expansion — every downstream
  phase sees ordinary fragments); enforcement lint = a
  `CheckResolved` pass proving a scoping conjunct exists in every
  shape touching designated tables, with per-query opt-out
  annotations. `explain` reports per-query policy coverage.
