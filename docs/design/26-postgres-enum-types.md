# sqletch Design — 26: PostgreSQL enum types

Status: ACCEPTED — owner decisions D1–D6 settled 2026-10-15 (every
recommendation adopted; D3 = no `Valid()`); implemented. §7 records the
implementation choices made within them. Amends design 04 (type oracle: the Describe answer gains
enum metadata), design 06 (codegen: type resolution beyond the static
TypeMap), and design 21 (oracle entry contents; no layout change).

## 1. Motivation

A parameter or result column whose type is a PostgreSQL enum
(`CREATE TYPE … AS ENUM`) fails code generation with SQLETCH311:

```sql
-- name: InsertAnswerEvent :exec
INSERT INTO questionnaire_answer_events (company_uuid, event_type)
VALUES (:company_uuid, :event_type);
-- SQLETCH311: no Go mapping for type questionnaire_answer_event_type
--             (oid 16412) of parameter "event_type"
```

The workaround authors use today — and that the SQLETCH311 hint steers
them to (design 20 §3.1 names it) — is a round trip through `text`:

```sql
VALUES (:company_uuid, CAST(:event_type AS text)::questionnaire_answer_event_type)
```

Both halves of that cast are needed: `CAST(:event_type AS
questionnaire_answer_event_type)` still makes PostgreSQL infer the
parameter as the enum, so the inner `text` is what pins it. The result
is a `string` field, every enum value is spelled as a string literal at
the call site, and a typo surfaces only at execution time as SQLSTATE
22P02. Result columns need the mirror image (`event_type::text AS
event_type`) in every projection.

The type is not unknown to sqletch. `postgres.Oracle.Describe` already
receives the enum's OID from `PREPARE` and resolves its `typname`
through `pg_type`; the oracle entry records
`{"oid": 16412, "name": "questionnaire_answer_event_type"}`. What is
missing is (a) the fact that the OID *is an enum*, plus its labels, and
(b) a codegen path that maps anything outside the fixed
`postgres.TypeMap` table. This design adds both, inside the oracle — no
catalog change.

## 2. Goal and non-goals

Goal: a bare `:param` bound to an enum position, and an enum-typed
result column, generate a per-enum named Go type with one constant per
label, with no cast in the template.

```go
// QuestionnaireAnswerEventType is the PostgreSQL enum
// public.questionnaire_answer_event_type.
type QuestionnaireAnswerEventType string

const (
	QuestionnaireAnswerEventTypeStarted  QuestionnaireAnswerEventType = "STARTED"
	QuestionnaireAnswerEventTypeAnswered QuestionnaireAnswerEventType = "ANSWERED"
	QuestionnaireAnswerEventTypeSkipped  QuestionnaireAnswerEventType = "SKIPPED"
)
```

Nullability and presence compose unchanged: a nullable enum column is
`optional.Option[QuestionnaireAnswerEventType]`, a guarded parameter
`sqletch.Omittable[…]` (designs 17 and 20).

Non-goals (stay SQLETCH311 in this design; see D5):

- arrays of enums (`_questionnaire_answer_event_type`), including the
  PostgreSQL `@in` rendering `= ANY($n)`;
- domains (over an enum or anything else), composite and range types;
- `-- @param p: <enum name>` hints — Tier 1 infers enum parameters, so
  the hint has nothing to supply; `TypeByName` keeps not knowing enum
  names and an enum hint stays "unknown SQL type" (SQLETCH311);
- MySQL `ENUM`/`SET` and SQLite (no enum types).

## 3. Soundness and premises

- **P1 (pinned parameter types).** The generated code prepares the
  identical SQL text the oracle verified, so the server infers the
  same enum OID at run time; the parameter is bound AT the enum, not
  at text. Nothing re-infers per shape. The Go side holds the label as
  a string, which is the enum's external representation — the mapping
  loses nothing (unlike `numeric → float64`), so it is consistent with
  the TypeMap rule "never a silent guess".
- **Driver boundary (design 17) unchanged.** pgx v5 has no codec for an
  unregistered OID; it falls back to the codec of the Go value's type
  (`Map.TypeForValue`), and named types reach it through
  `TryWrapFindUnderlyingTypeEncodePlan` / `TryFindUnderlyingTypeScanPlan`
  (`type T string` → `string` → text codec, text format). Binds stay
  `*T`, scans stay `*T` temporaries + `FromNillable`. This was read from
  the pgx v5.11 source, then pinned against PostgreSQL 16 by
  `TestPgEnumDriverBoundary` before codegen relied on it.
- **Values are open, not closed.** A Go named string type does not
  restrict its values: `QuestionnaireAnswerEventType("typo")` compiles
  and fails at the server with 22P02, exactly like today's `string`.
  The gain is discoverability and typed signatures, not a closed set.
  The openness is also what keeps scanning safe: a label added by
  `ALTER TYPE … ADD VALUE` after generation still scans (see D3).

## 4. Mechanism

### 4.1 Oracle: describe the type, not just its name

`postgres.Oracle.typeNames` becomes a type-metadata lookup over the
same OID set (parameters ∪ result columns):

```sql
SELECT t.oid, t.typname, n.nspname, t.typtype,
       array(SELECT e.enumlabel FROM pg_enum e
             WHERE e.enumtypid = t.oid ORDER BY e.enumsortorder)
FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE t.oid = ANY($1)
```

`dialect.TypeRef` gains one optional field:

```go
type TypeRef struct {
	OID  uint32
	Name string
	Ext  *TypeExt // nil for built-in types (OID < 16384)
}

type TypeExt struct {
	Schema string   // pg_namespace.nspname
	Kind   string   // pg_type.typtype: "e" enum, "b" base, "d" domain, …
	Labels []string // enum only, in enumsortorder
}
```

`Ext` is recorded for EVERY user-defined type — OID ≥ 16384
(`FirstNormalObjectId`), which includes extension types such as
`citext` — and never for a built-in. The rule is keyed on the OID
range, not on membership in the static TypeMap, so a later growth of
that table does not change what a cold Describe writes (cold/warm byte
identity, determinism invariant). Only `Kind == "e"` is acted on in this
design; recording the kind for the others is what makes D4's staleness
check exact.

### 4.2 Cache: entries carry the metadata

`cache.EntryType` and `cache.EntryColumn` gain the same data as
`omitempty` fields, converted in the single seam
`dialect.EntryFromDesc` / `DescFromEntry` (design 15 §3):

```json
{"oid": 16412, "name": "questionnaire_answer_event_type",
 "schema": "public", "kind": "e", "labels": ["STARTED", "ANSWERED", "SKIPPED"]}
```

(A result column spells them `type_schema` / `type_kind` /
`type_labels`, beside its existing `type_name`.)

An entry whose types are all built-in serializes byte-identically to
today, so no existing PostgreSQL entry changes and no MySQL/SQLite
entry can change (they never set `Ext`; the native/server byte-identity
gates are untouched). No `FormatVersion` bump — see D4 for entries
written before this design.

Labels need no separate invalidation: `ALTER TYPE … ADD VALUE` /
`RENAME VALUE` is a DDL edit, which changes the schema fingerprint,
which every entry carries (design 21 D1) — every entry misses and is
rewritten on the next cold run.

### 4.3 Codegen: resolve through the TypeRef

`dialect.TypeMap.GoType(oid)` cannot see `Ext`. Codegen's type
resolution becomes a small resolver in `internal/codegen`:

1. static `TypeMap.GoType(oid)` hit → that type (unchanged path);
2. else `Ext.Kind == "e"` → the package's named type for that enum
   (§4.4);
3. else SQLETCH311 as today (message unchanged for non-enums).

Every current `GoType` call site that emits SQLETCH311 (parameter
fields, result columns, `@filter-tree` predicate arguments) goes
through the resolver, so the three cannot disagree.

### 4.4 Emission: `enums.gen.go`

Per output package (target, design 19), the set of enums its queries
reference is emitted once, sorted, into `enums.gen.go` — the
`policy.gen.go` pattern (one shared named type, declared once, used by
every query). The file is not written when the set is empty, so
existing projects regenerate byte-identically (`generate`'s stale
`*.gen.go` sweep removes it when the last enum use goes away).

- Type name: `GoName(typname)`; constant name: type name +
  `GoName(label)` (D2) — in both, every rune that cannot occur in a Go
  identifier first becomes `_` (a word break). Doc comment names
  `schema.typname`. A type name that is still not an exported
  identifier (a CJK first letter) is SQLETCH307, hinting the `text`
  cast.
- Both are claimed in the per-package generated-name space: a collision
  with a query's Params/Row/@choose/@order-by names, a policy type, or
  `generatedPkgNames` is SQLETCH310/307 as for every other generated
  name.
- Two distinct enums (different `schema.typname`) yielding the same Go
  name is SQLETCH310 (D6).

## 5. Owner decisions (settled 2026-10-15)

- **D1 — Go representation.** (a) a per-enum named string type with
  label constants (§2), or (b) plain `string`. **Settled: (a).** (b)
  removes the cast but keeps every other cost of today; moving from (b)
  to (a) later would change every generated signature a second time.
- **D2 — labels that do not make a Go identifier.** Enum labels are
  arbitrary text (`in-progress`, `済`, `''`, two labels differing only in
  case). `GoName` splits on `_` only, so `in-progress` is not an
  identifier. Options: (a) sanitize (non-identifier runes act as `_`)
  and, if any label is still empty or two labels collide, emit the
  type WITHOUT constants plus a warning naming the labels; (b) make it
  an error (new SQLETCH3xx) whose hint is the `text` cast; (c) always
  emit no constants. **Settled: (a)** — the type is still useful, the
  warning is loud, and a label set never blocks generation.
- **D3 — `Valid()`.** Generate `func (v T) Valid() bool` over the
  labels? It reflects the labels at GENERATION time, so after
  `ADD VALUE` without regenerating, `Valid` rejects a label the server
  accepts. **Settled: not generated** (the server is the
  authority; 22P02 is the check). Adding it later is additive.
- **D4 — entries written before this design.** An entry already
  committed for a query that hit SQLETCH311 on an enum has the enum OID
  but no `Ext`. Options: (a) the PostgreSQL dialect treats an entry
  containing a user-defined OID (≥ 16384) with no `Ext` as a MISS
  (re-described on the next connected run; the LSP skips the query
  like any miss); (b) bump `cache.FormatVersion`, rewriting every entry
  of every project. **Settled: (a)** — exact (§4.1 records `Ext` for
  every user-defined OID, so "user-defined and no Ext" can only be a
  pre-design entry), and it costs nothing for projects without such
  types. The check lives in the dialect, not in `cache` (a leaf that
  knows no OID ranges).
- **D5 — scope.** Arrays of enums (and therefore PostgreSQL `@in` on an
  enum column), domains over enums, and enum `@param` hints stay
  SQLETCH311 in this design (§2). **Settled: yes**; arrays are the
  natural follow-up (`[]T` needs its own devdb pinning: pgx has no array
  codec for an unregistered element OID).
- **D6 — same-named enums in different schemas.** Name them by
  `typname` alone and refuse a collision with SQLETCH310, or prefix the
  schema always/on collision. **Settled: refuse** — schema-prefixing
  only on collision makes a Go name depend on an unrelated schema.

## 6. Tests

Test-first, per docs/development/testing.md:

- **devdb, before any codegen change** — pin the driver behavior §3
  relies on: binding `string` and `type T string` (and `*T`, nil `*T`)
  to an enum parameter on a prepared statement; scanning an enum column
  into `*string` / `*T`; 22P02 on an unknown label; a label added by
  `ALTER TYPE … ADD VALUE` after preparation scans
  (`TestPgEnumDriverBoundary`; see I4 for the pool).
- **oracle (devdb)** — `Describe` returns `Ext` for enum params and
  columns with labels in `enumsortorder` order (seed a type whose
  `ADD VALUE … BEFORE` makes sort order ≠ creation order), `Ext` with
  `Kind "b"` for an array of the enum, no `Ext` for built-ins
  (`TestPgEnumDescribe`).
- **cache** — round trip through `EntryFromDesc`/`DescFromEntry`; an
  all-built-in entry is byte-identical to a pre-design golden; D4's
  miss rule (unit, against a hand-written pre-design entry).
- **codegen** — golden `enums.gen.go`; Option/Omittable nesting;
  collisions (SQLETCH310/307) and D2 label cases; SQLETCH311 still
  raised for an array of enums, a domain, `interval`; `explain` shows
  the Go type.
- **generated-module E2E (devdb)** — extend the PostgreSQL seed with an
  enum column (nullable AND not null, adversarial labels incl.
  multibyte) and run insert/select round trips through the generated
  code, NULL rows included.
- **examples** — `examples/postgres` gains one enum column so the
  feature is exercised by the committed-cache offline round trip and
  `TestExamplesAreLintClean`.

## 7. Implementation choices

- **I1 — D2's warning is SQLETCH320** (`CodeEnumLabelIdent`). It is a
  package-level diagnostic emitted after every query is generated:
  `Generate` drops a query's file on any per-query diagnostic, so a
  warning raised inside a query would have dropped it. Constant naming
  drops GoName's `X` prefix for a label starting with a digit (the type
  prefix already makes it an identifier; keeping it would collide `1st`
  with `x1st`). Like every codegen diagnostic it is reported by
  `generate`/`check`, not by the offline LSP.
- **I2 — name claims.** `codegen.checkEnumNames` claims each enum type
  and constant against the fixed package names (SQLETCH307), query
  types, `@choose`/`@order-by` constants (now recorded per package in
  `pkgConsts`), policy types, and other enums (SQLETCH310). An enum
  that hits one of these is never also reported as SQLETCH311.
- **I3 — D4's seam.** `postgres.StaleEntry` is the rule;
  `cli.loadOracleEntry` (driver hook `staleEntry`) is the ONE committed
  read of an oracle entry for `pipeline.Run` and the OfflineChecker, so
  both treat a stale entry as a miss. The re-described entry is written
  back like any miss and is live for `generate`'s sweep.
- **I4 — pool coverage.** The devdb driver-boundary test runs on a
  `pgx.Conn` under both cached-statement and describe-exec modes
  (pgxpool would add a dependency to the main module); the
  generated-module run goes through `pgxpool`.
- **Pre-existing, out of scope:** `devdb` resets the schema on every
  connected run, so a run with SOME misses describes them under new
  OIDs while the cached catalog keeps the old ones. Provenance then
  fails to match and those columns widen to `Option` (safe direction).
  D4 re-describes are such partial-miss runs, so a project upgrading
  with stale enum entries may see this; it is not specific to enums.

## 8. Documentation

- Manual: dialects chapter (PostgreSQL enums), diagnostics (SQLETCH311
  note, SQLETCH320), generated-code chapter (`enums.gen.go`).
- `examples/postgres`: `audit_logs.severity` (`audit_severity` enum),
  selected and filterable in `ListAuditLogs`.
- CLAUDE.md: "Known decisions: PostgreSQL enums (doc 26)".
