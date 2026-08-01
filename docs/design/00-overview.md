# sqletch Design — 00: Overview

Status: draft for v0.1 implementation
Companion to: `PROJECT_INSTRUCTION.md` (the concept/spec document; this
series is the implementation design). Where the two disagree,
`PROJECT_INSTRUCTION.md` wins and this series must be updated.

## Reading order

| Doc | Implementation phase | Ships in |
|-----|----------------------|----------|
| 00-overview (this) | — | — |
| 01-template-scanner | P1: construct scanner + lexer profile | v0.1 |
| 02-rendering | P2: renderings, pg_query frontend, source maps, R1 | v0.1 |
| 03-structural-rules | P3: rules R2–R9, guard/scope analysis | v0.1 |
| 04-type-oracle | P4: dev DB, Describe oracle, catalog snapshot, cache | v0.1 |
| 05-nullability | P5: nullability analysis | v0.1 |
| 06-codegen-runtime | P6: code generation + runtime package | v0.1 |
| 07-cli-config | P7: CLI, configuration, diagnostics output | v0.1 |
| 08-later-phases | P8+: v0.2–v0.4 implementation deltas | v0.2+ |

Phases P1–P7 are ordered by dependency and each ends in a testable,
mergeable state. P1–P3 run without any database; P4 introduces the
oracle; P5–P7 complete the v0.1 pipeline.

## Toolchain and dependencies

- Go ≥ 1.24, module `github.com/moznion/sqletch`.
- `github.com/pganalyze/pg_query_go/v6` — PostgreSQL parser (parse +
  scan/lexer APIs). Used by the postgres dialect driver only.
- `github.com/jackc/pgx/v5` — PostgreSQL protocol (Prepare/Describe
  gives `StatementDescription{ParamOIDs, Fields}`), and the driver
  interface generated code binds against.
- `github.com/testcontainers/testcontainers-go` — auto-managed dev DB
  (only when no DSN is configured).
- `github.com/spf13/cobra` — CLI subcommands.
- `gopkg.in/yaml.v3` — configuration.
- No ORM, no code-generation frameworks; generated Go is produced with
  `text/template` + `go/format`.

## Repository layout (implementation view)

```
cmd/sqletch/            main; wires cobra commands to internal/cli
internal/
    cli/                command implementations (generate/check/explain)
    config/             sqletch.yaml schema, loading, validation
    template/           P1: scanner, construct grammar, fragment model
    ast/                P2: rendering, source maps, dialect-AST view
    rules/              P3: R1–R9 checks (R1 lives here but runs in P2's
                        pipeline position; see 02/03)
    shape/              shape keys, guard bit allocation, enumeration
    dialect/            driver interface (LexerProfile, Frontend, Oracle,
                        Placeholder, TypeMap, Binding)
        postgres/       pg_query frontend + pgx Describe oracle
    devdb/              dev-DB lifecycle (DSN passthrough / container)
    cache/              committed oracle+catalog cache
    nullability/        P5: skeleton-based propagation
    codegen/            P6: Go emission
    diagnostics/        Diagnostic type, codes, rendering
runtime/                public package imported by generated code
examples/               end-to-end sample project (also used in CI)
testdata/               conformance corpus shared across phases
docs/design/            this series
```

Package dependency direction (arrows = imports):

```
cli → config, template, ast, rules, cache, devdb, nullability,
      codegen, dialect/postgres
codegen → shape, diagnostics, dialect (interfaces only)
rules → ast, template, shape, diagnostics, cache (catalog model)
ast → template, dialect (interfaces), diagnostics
template → dialect (LexerProfile interface only), diagnostics
runtime → (stdlib only; driver interfaces via type parameters /
           small local interfaces — see 06)
```

`internal/dialect` defines interfaces; `internal/dialect/postgres` is
the only implementation in v0.1. Nothing outside `dialect/postgres`
may import pg_query or pgx directly (enforced by a depguard lint rule),
except `runtime`/generated code which binds pgx per the driver choice.

## Core data model (shared vocabulary)

Defined in `internal/template` and `internal/shape`; used everywhere.

```go
// Positions are byte offsets into the original template file.
// (Implementation note: Span lives in internal/diagnostics so every
// package can attach spans to diagnostics without import cycles.)
type Span struct{ File string; Start, End int }

type Annotation int // One, Many, Exec, ExecRows

type QueryFile struct {
    Path    string
    Queries []*QueryTemplate
}

type QueryTemplate struct {
    Name       string
    Annotation Annotation
    Items      []Item            // document order
    Params     map[string]*Param // by template name (snake_case)
}

// Item is a sum type: *Skeleton | *IfPresent | *Choose
type Skeleton struct {
    Text string // verbatim bytes, params still :name
    Span Span
}

type IfPresent struct {
    Guards []GuardAtom // v0.1: presence atoms only
    Sep    Sep         // lifted separator: SepNone|SepAnd|SepComma
    Body   string      // verbatim bytes, separator removed
    Slot   Slot        // filled in P2: SlotWhereConjunct|SlotJoinItem|…
    Span   Span
}

type Choose struct {
    Param   string
    Cases   []ChooseCase // declaration order
    Default *ChooseCase  // nil = required parameter
    Slot    Slot         // v0.1: SlotOrderBy only
    Span    Span
}

type ChooseCase struct{ Name, Body string; Span Span }

// GuardAtom: v0.1 has only presence atoms; the type is future-proofed
// for @when value atoms (08-later-phases).
type GuardAtom struct {
    Param string
    // Value/Op empty in v0.1
}

type Param struct {
    Name     string
    Optional bool   // per R9 classification (P3)
    GuardBit int    // -1 unless this param is a guard
    GoName   string // PascalCase (P6)
}
```

```go
// internal/shape
type ShapeKey struct {
    Guards  uint64  // bit i = guard with GuardBit i active
    Choices []uint8 // one ordinal per Choose, document order
}
// Canonical encoding (for cache keys and debugging):
// "g=<hex>;c=<n0>,<n1>,..." — stable, human-readable.
```

Hard limit: ≤ 64 guard atoms per query (`Guards uint64`). Exceeding it
is compile error `SQLETCH010` (nobody sane has 65 independent optional
fragments in one query; revisit if proven wrong).

## Pipeline (v0.1)

```
config load
  → for each query file: P1 scan → QueryTemplate
  → P2 render maximal + per-case renderings; parse via frontend;
       build source maps; R1 slot/node-completeness checks
  → P4 oracle: catalog snapshot + Describe per rendering (cache-aware)
  → P3 rules: catalog-free (R5,R6,R7,R9) then catalog-dependent (R2,R3,R8)
  → P5 nullability
  → P6 codegen → gofmt → write
```

Note the P3/P4 interleave: rule checks that need the catalog (star
expansion, reference resolution) run after the oracle phase even though
they are "phase 3" deliverables. The `rules` package exposes two entry
points, `CheckLexical(...)` and `CheckResolved(...)`, so the pipeline
ordering is explicit in code.

`check` runs the same pipeline minus codegen. Offline operation: if
every oracle lookup hits the cache, P4 never opens a connection.

## Cross-cutting conventions

**Diagnostics.** Every user-facing error is a
`diagnostics.Diagnostic{Code, Span, Message, Hint}`. Codes are stable
(`SQLETCH0xx` lexical/structural, `SQLETCH1xx` rules, `SQLETCH2xx`
oracle, `SQLETCH3xx` codegen/config). Messages must state the rule
*and its rationale*; `Hint` carries the compliant rewrite. Rendering
(source excerpt with caret) lives in `diagnostics` and is used by all
commands. Never return bare `fmt.Errorf` for user mistakes; reserve Go
errors for environmental failures (I/O, connection).

**Determinism.** Byte-identical inputs must produce byte-identical
outputs everywhere: renderings, cache files (canonical JSON: sorted
keys, no timestamps), generated Go (stable iteration order — never
range over maps without sorting), composed SQL. CI has a
double-generate test (`generate` twice, diff must be empty).

**Premises P1/P2 ownership.** P2 (verbatim fragments + fixed
connective vocabulary) is implemented once in `internal/ast/render.go`
and mirrored in `runtime/compose.go`; a shared conformance test
(`testdata/compose`) asserts the two produce identical bytes for
identical shape selections — "what is verified is byte-wise what is
composed" is enforced by test, not by convention. P1 (pinned parameter
types) is owned by codegen + runtime binding (06).

**Testing.** Each phase doc defines acceptance criteria. Shared
corpus: `testdata/` holds template files with expected outcomes
(`*.sql` + `*.golden.json` for scanner output, `*.golden.sql` for
renderings, `*.diag` for expected diagnostics). The property test
(enumerate shapes → PREPARE + EXPLAIN each) lands in P4 and grows with
every construct.

## v0.1 scope guard

In scope: PostgreSQL only; `@if-present` on WHERE conjuncts and
INNER/LEFT filter-only join items; `@choose`/`@default` on statement
ORDER BY; annotations `:one/:many/:exec/:execrows`; oracle cache;
`generate`, `check` (with `--exhaustive`), `explain` (basic listing).

Explicitly out (deferred to 08-later-phases): SET/INSERT slots,
`@choose` projection/GROUP BY, `@when`, `@filter-tree`, `@order-by`,
`@in`, `fmt`, static expansion, Tier 2 dialects, embedded oracle, LSP.
