# CLAUDE.md

sqletch is a compiler for a restricted conditional-SQL template
language: templates compile to typed Go where every reachable query
shape is statically verified and runtime work is deterministic
composition of pre-verified constant fragments.

## Document authority

1. **`PROJECT_INSTRUCTION.md` is the specification** — the structural
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
go run ./cmd/sqletch generate --config examples/sqletch.yaml
go test ./internal/template -fuzz=FuzzScan -fuzztime=15s
```

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
internal/config,cli P7  sqletch.yaml + generate/check/explain pipeline
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
