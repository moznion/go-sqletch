# sqletch Design — 24: Performance lints

**Status: ACCEPTED — owner decisions D1–D3 settled 2026-10-02;
implemented.** This document adds a warning-only lint lane for query
shapes that defeat indexes or grow without bound. Nothing here touches
the verification model: no rule R1–R9 changes, no rendering changes, no
cache or fingerprint input changes, and no lint can fail a run.

## 1. Why sqletch can do this better than a generic SQL linter

Every reachable fragment of a template appears in one of its
verification renderings (`ast.Renderings`: the maximal rendering
carries every `@if-present` body, and each `@choose` case and
`@order-by` default has its own). A predicate that defeats an index
only in the shape where some optional filter is on is therefore not a
needle in an unbounded space: it is in a finite, already-materialized
set of SQL texts whose source map points back at the template bytes.
The catalog-dependent pass additionally knows every column's catalog
type and every parameter's resolved type — the inputs a type-conversion
lint needs and a syntax-only linter never has.

## 2. Owner decisions (2026-10-02)

- **D1 — Warnings, a separate axis.** Every performance lint is a
  `Warning`. `generate`/`check` exit status never depends on them.
  They are not soundness findings and never weaken or replace one.
- **D2 — Per-query suppression via `-- @allow`.** One or more codes,
  per query, buffered and attached exactly like `-- @param` /
  `-- @policy-optout`. `@allow` may name **only performance-lint
  codes**; any other code, an unknown code, or a malformed directive is
  an ERROR (SQLETCH016).
- **D3 — Initial lint set (all four):** function/cast on the column
  side of a comparison; leading-wildcard `LIKE`; `:many` without
  `LIMIT` (plus OFFSET pagination, separate code); parameter-vs-column
  type mismatch that forces a column-side conversion.

## 3. Codes

| Code | Pass | What |
| --- | --- | --- |
| SQLETCH016 | scanner | malformed `@allow`, or it names a non-performance / unknown code (error) |
| SQLETCH128 | scan (`cli.scanChecks`) | function/cast applied to the column side of a WHERE / JOIN ON comparison |
| SQLETCH129 | scan | `LIKE`/`ILIKE` pattern provably starting with `%` or `_` |
| SQLETCH130 | scan | `:many` SELECT with a reachable rendering that has no `LIMIT` / `FETCH FIRST` |
| SQLETCH131 | scan | OFFSET pagination with a non-constant offset |
| SQLETCH132 | resolved (`cli.resolvedChecks`) | column compared with a parameter whose type converts the column |
| SQLETCH133 | both | an `@allow` that suppressed nothing (warning) |

The proposed codes were verified free on 2026-10-02 (SQLETCH017 and
SQLETCH318 are reserved for the concurrent `@timeout` work).

### 3.1 Shared mechanics

- **Where they run.** 128–131 run in `cli.scanChecks` right after R1,
  over the woven renderings, so the pipeline and the LSP get them from
  the one shared seam (and the LSP memoizes them with the rest of the
  per-file phase). 132 needs the catalog and the final parameter types
  and runs last in `cli.resolvedChecks`, again shared by `pipeline.Run`
  and `OfflineChecker`.
- **Lexical whitelist.** The analysis is token-based over each
  rendering (the dialect `LexerProfile`), not AST-based: three
  frontends with three ASTs would triple the surface for a warning
  lane. The contract that makes this acceptable is the failure
  direction: each lint flags only the unambiguous form and
  **under-reports** everything else. A missed lint costs nothing the
  database was not already charging; a noisy lint trains authors to
  sprinkle `@allow`.
- **Predicate positions.** A token is in a predicate position when it
  sits directly in a `WHERE` or `JOIN … ON` clause (any depth, so a
  subquery's own WHERE counts), or inside a parenthesized boolean group
  opened after `WHERE`/`ON`/`AND`/`OR`/`NOT`/`(`. Function-argument
  lists, operand subqueries (`IN (SELECT …)`, `= (SELECT …)`), and
  `CASE` expressions are not predicate positions. `ON CONFLICT` and
  `ON DUPLICATE KEY` open none.
- **HAVING is excluded** (a decision beyond D3's wording, 2026-10-02):
  HAVING filters groups after aggregation, where no index applies, so a
  "function on the column" there is not an index finding.
- **Spans and determinism.** Findings map through `Rendering.Map`
  back to template bytes; a placeholder maps to its `:name`, and any
  other synthesized token (a policy-woven conjunct, a construct
  emission) makes the finding unattributable, so it is dropped — the
  author did not write that text. A finding seen in several renderings
  is reported once per (code, span); output is sorted by span.

### 3.2 SQLETCH128 — function/cast on the column side

Flagged: a comparison (`= <> != < > <= >=`, `LIKE`, `ILIKE`, `IN (…)`,
`BETWEEN`) in a predicate position where one operand is

- `f(…)` whose arguments contain **exactly one** bare column reference
  and are otherwise column-free (`lower(email)`,
  `date_trunc('day', created_at)`, `coalesce(status, 'x')`),
- `col::type` (PostgreSQL, multi-word types included), or
- `CAST(col AS type)`,

and the other operand is **column-free**: placeholders, literals,
operators, calls over those, typed literals, `INTERVAL` units. Only the
wrapped side may be a column for `IN`/`BETWEEN`/`LIKE`. Not flagged:
both sides referencing columns (a join condition's index story is the
planner's), arithmetic on the column, negated forms, anything inside
`CASE`. Expression indexes are deliberately **not modeled**: the hint
says to `@allow` when one exists.

### 3.3 SQLETCH129 — leading-wildcard LIKE

Flagged: `col LIKE p` / `ILIKE` (not `NOT LIKE`), `col` a bare column,
where `p` provably starts with a wildcard: a string literal whose
content starts with `%` or `_` (E-strings included), the first operand
of a concatenation (`'%' || :q`), or `CONCAT`'s first argument. A bare
`:q` pattern is never flagged — its content is unknowable at compile
time.

### 3.4 SQLETCH130 / SQLETCH131 — unbounded results, OFFSET paging

- **130** fires once, at the query header, when the query is `:many`,
  its statement is a SELECT (a depth-0 `SELECT` and no depth-0
  `INSERT`/`UPDATE`/`DELETE`/`REPLACE` — DML `RETURNING` is bounded by
  what it modifies), and **any** verification rendering lacks a depth-0
  `LIMIT` (other than `LIMIT ALL`) or `FETCH FIRST|NEXT`. A LIMIT that
  only some renderings carry still warns: the other shapes are
  reachable. (No slot admits a guarded LIMIT today; checking every
  rendering keeps this true if one ever does.) A subquery's LIMIT does
  not bound the outer result. `:one`/`:maybe-one` never warn.
- **131** fires on a depth-0 `OFFSET <expr>` and on MySQL/SQLite's
  `LIMIT <offset>, <count>` when the offset is not a single numeric
  literal. `offset` is non-reserved on MySQL/SQLite, so a column of
  that name is not the clause: the keyword counts only after a depth-0
  `LIMIT`, or when it does not follow a token that introduces an
  expression (`SELECT`, `BY`, `WHERE`, `AND`, `,`, `(`, an operator, …).

### 3.5 SQLETCH132 — parameter-vs-column type mismatch

Inputs: the maximal tree's top-level relations resolved against the
catalog (the R3 resolver), and the parameter types `resolvedChecks`
settled (oracle-inferred on Tier 1, `-- @param` on Tier 2). Only the
TOP-LEVEL statement's WHERE/ON predicates are inspected (a subquery's
columns would need scope resolution the facade does not model), set
operations are skipped (SQLite's `Relations()` is the first core's),
and the operands must be a bare column vs. a bare placeholder (or an
`IN` list of them). PostgreSQL also accepts one `::type` /
`CAST(… AS type)` around the placeholder — the oracle's inferred
parameter type IS that cast type; a double cast is skipped. MySQL takes
bare placeholders only: there the annotation types the bind, but an
explicit cast would decide the comparison.

The per-dialect whitelist (only pairs known to move the conversion
onto the column):

| Dialect | Column | Parameter | Why |
| --- | --- | --- | --- |
| PostgreSQL | `int2`/`int4`/`int8` | `numeric`, `float4`, `float8` | the integer btree family has no integer-vs-numeric/float operator; the planner casts the column |
| MySQL | string/blob wire types (CHAR, VARCHAR, TEXT/BLOB family) | integer, float/double, decimal | string vs number compares as floating point; the manual states the string column's index cannot be used |
| SQLite | — | — | a comparison with a bound parameter applies the **column's** affinity to the parameter (a parameter has none), so the conversion never lands on the column |

Cross-type pairs inside one btree operator family (int4 vs int8,
float4 vs float8, date vs timestamp, varchar vs text) and the reverse
MySQL direction (numeric column vs string value) are index-safe and
never flagged. The SQLite row is a decision beyond D3's wording
(2026-10-02): the owner's example mentioned "SQLite affinity
mismatch", but no SQLite pair is *known* to defeat the index under the
whitelist rule, so none is flagged; revisit with a counterexample.

The MySQL wire-code flag bits are repeated in `internal/rules` to keep
it free of a driver import; `TestPerfTypes_MySQLFlagsAgree` pins them.

## 4. `-- @allow`

```sql
-- name: FindByEmail :many
-- @allow SQLETCH128, SQLETCH130 (expression index users_lower_email_idx; ≤ 5 rows per address)
SELECT id FROM users WHERE lower(email) = :email;
```

- Form: `-- @allow SQLETCHnnn[, SQLETCHnnn…]` with an optional
  trailing `(reason)` (optional like `@policy-apply`'s: the directive is
  visible in review either way; a reason is encouraged). Any
  `@allow`-shaped comment (`-- @allow`, `-- @allow:`, `-- @allow X`) is
  the directive, so a malformed one is SQLETCH016 rather than a
  silently ignored comment; `-- @allowX` is not the directive.
- All-or-nothing: a directive naming any non-performance or unknown
  code records **none** of its codes.
- Scope: the whole query (every rendering). It stays in the skeleton
  verbatim like every directive (so adding one re-keys that query's
  oracle entries — the rendered SQL changed).
- **SQLETCH133 (unused @allow), decision 2026-10-02:** an `@allow`
  whose code does not fire on the query is a warning at the directive,
  because a stale suppression would hide that lint's next regression.
  Each pass judges only the codes it owns: the scan pass judges
  128–131, the resolved pass judges 132 — so a run that cannot reach
  the catalog-dependent pass (an LSP cache miss) never calls a
  SQLETCH132 allow stale. On SQLite, where SQLETCH132 never fires, an
  `@allow SQLETCH132` is always reported unused. SQLETCH133 itself
  cannot be allowed (a stale suppression must not silence its own
  staleness report).

## 5. Examples

`examples/` stays warning-free: `AllAuditActions` (PostgreSQL) and
`CountByStatus` (SQLite) are GROUP BY results bounded by a vocabulary
and carry a justified `@allow SQLETCH130`; `UserAuditActions` gained
`LIMIT :limit` (its result is genuinely unbounded).

## 6. Known limits / follow-ups

- Lexical keyword handling: a non-reserved keyword used as a column
  name (`group`, `offset`, …) in a predicate can end the predicate
  early — a missed warning, never a wrong one.
- The editor grammars (doc 11) do not highlight `@allow` specially; it
  renders as a comment.
- Expression-index awareness (and index awareness in general, design
  conversation "A1") needs indexes in the catalog — a cache format
  change, deliberately out of scope here.
