# sqletch Design — 24: Performance lints

**Status: ACCEPTED — owner decisions D1–D3 settled 2026-10-02, D4–D5
on 2026-10-03; implemented.** This document adds an opt-in,
warning-only lint lane for query
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
- **D2 — Per-query suppression via `-- @nolint`.** One or more codes,
  per query, buffered and attached exactly like `-- @param` /
  `-- @policy-optout`. `@nolint` may name **only performance-lint
  codes**; any other code, an unknown code, or a malformed directive is
  an ERROR (SQLETCH016). (Spelled `@allow` until a second owner
  decision the same day renamed it `@nolint`, after golangci-lint's
  `//nolint`, which the Go audience already reads as "suppress a
  lint"; the semantics did not change.)
- **D3 — Initial lint set (all four):** function/cast on the column
  side of a comparison; leading-wildcard `LIKE`; `:many` without
  `LIMIT` (plus OFFSET pagination, separate code); parameter-vs-column
  type mismatch that forces a column-side conversion.
- **D4 — Opt-in (2026-10-03).** Lints run only when enabled:
  `lint: true` in sqletch.yaml, or `--lint` on `generate`/`check`. The
  flag overrides the config either way for one invocation
  (`--lint=false` turns them off; absent = the config decides). The LSP
  has no flags and follows the config. With lints off, `@nolint` is
  still parsed and validated by the scanner — SQLETCH016 is a template
  error and must not depend on config — but no lint, and no
  SQLETCHL006, is reported. The key is not a cache or fingerprint input.
- **D5 — Own code space (2026-10-03).** Lints use `SQLETCHLnnn`
  (L001–L006), not the `SQLETCHnnn` rule space: the prefix says "lint,
  warning, opt-in" at a glance, and the rule numbering stays free of
  them. The first implementation used SQLETCH128–133; `@nolint
  SQLETCH130` is now SQLETCH016 (not a lint code).

## 3. Codes

| Code | Pass | What |
| --- | --- | --- |
| SQLETCH016 | scanner | malformed `@nolint`, or it names a non-performance / unknown code (error) |
| SQLETCHL001 | scan (`cli.scanChecks`) | function/cast applied to the column side of a WHERE / JOIN ON comparison |
| SQLETCHL002 | scan | `LIKE`/`ILIKE` pattern provably starting with `%` or `_` |
| SQLETCHL003 | scan | `:many` SELECT with a reachable rendering that has no `LIMIT` / `FETCH FIRST` |
| SQLETCHL004 | scan | OFFSET pagination with a non-constant offset |
| SQLETCHL005 | resolved (`cli.resolvedChecks`) | column compared with a parameter whose type converts the column |
| SQLETCHL006 | both | an `@nolint` that suppressed nothing (warning) |

SQLETCH016 (verified free 2026-10-02; SQLETCH017/318 belong to the
`@timeout` work) stays in the rule space because it is a template
error, raised with lints on or off.

### 3.1 Shared mechanics

- **Where they run.** When enabled (D4; `scanChecks`/`resolvedChecks`
  take the effective switch), L001–L004 run in `cli.scanChecks` right after R1,
  over the woven renderings, so the pipeline and the LSP get them from
  the one shared seam (and the LSP memoizes them with the rest of the
  per-file phase). L005 needs the catalog and the final parameter types
  and runs last in `cli.resolvedChecks`, again shared by `pipeline.Run`
  and `OfflineChecker`.
- **Lexical whitelist.** The analysis is token-based over each
  rendering (the dialect `LexerProfile`), not AST-based: three
  frontends with three ASTs would triple the surface for a warning
  lane. The contract that makes this acceptable is the failure
  direction: each lint flags only the unambiguous form and
  **under-reports** everything else. A missed lint costs nothing the
  database was not already charging; a noisy lint trains authors to
  sprinkle `@nolint`.
- **Predicate positions.** A token is in a predicate position when it
  sits directly in a `WHERE` or `JOIN … ON` clause (any depth, so a
  subquery's own WHERE counts), or inside a parenthesized boolean group
  opened after `WHERE`/`ON`/`AND`/`OR`/`NOT`/`(`. Function-argument
  lists, operand subqueries (`IN (SELECT …)`, `= (SELECT …)`), and
  `CASE` expressions are not predicate positions. `ON CONFLICT` and
  `ON DUPLICATE KEY` open none, and neither does any `WHERE` of an
  upsert's `ON CONFLICT` clause (the conflict target's partial-index
  predicate, or `DO UPDATE … WHERE` on the one conflicting row): no
  index-served scan is filtered there.
- **HAVING is excluded** (a decision beyond D3's wording, 2026-10-02):
  HAVING filters groups after aggregation, where no index applies, so a
  "function on the column" there is not an index finding. An
  aggregate's `FILTER (WHERE …)` (PostgreSQL/SQLite) is excluded for
  the same reason — it filters rows already fetched — for both L001 and
  L002.
- **Spans and determinism.** Findings map through `Rendering.Map`
  back to template bytes; a placeholder maps to its `:name`, and any
  other synthesized token (a policy-woven conjunct, a construct
  emission) makes the finding unattributable, so it is dropped — the
  author did not write that text. A finding seen in several renderings
  is reported once per (code, span); output is sorted by span.

### 3.2 SQLETCHL001 — function/cast on the column side

Flagged: a comparison (`= <> != < > <= >=`, `LIKE`, `ILIKE`, `IN (…)`,
`BETWEEN`) in a predicate position where one operand is

- `f(…)` whose arguments contain **exactly one** bare column reference
  and are otherwise column-free (`lower(email)`,
  `date_trunc('day', created_at)`, `coalesce(status, 'x')`),
- `col::type` (PostgreSQL, multi-word types included), or
- `CAST(col AS type)`,

and the other operand is **column-free**: placeholders, literals,
operators, calls over those, typed literals, `INTERVAL` units (only
the word right after the interval's value — a later `day` is a
column), casts
(a type name ends where the type grammar does: only the words of a
multi-word type continue it, so the zone column in `:t::timestamp AT
TIME ZONE tz` is still a column). Only the
wrapped side may be a column for `IN`/`BETWEEN`/`LIKE`. Not flagged:
both sides referencing columns (a join condition's index story is the
planner's), arithmetic on the column, negated forms, anything inside
`CASE`. Expression indexes are deliberately **not modeled**: the hint
says to `@nolint` when one exists.

### 3.3 SQLETCHL002 — leading-wildcard LIKE

Flagged: `col LIKE p` / `ILIKE` (not `NOT LIKE`), `col` a bare column,
where `p` provably starts with a wildcard: a string literal whose
content starts with `%` or `_` (E-strings included), the first operand
of a concatenation (`'%' || :q`), or `CONCAT`'s first argument. A bare
`:q` pattern is never flagged — its content is unknowable at compile
time. Only quote-delimited literals are read (`'…'`, MySQL `"…"`,
`E'…'`): a dollar-quoted `$$…$$` / `$tag$…$tag$` pattern is not judged.

### 3.4 SQLETCHL003 / SQLETCHL004 — unbounded results, OFFSET paging

- **L003** fires once, at the query header, when the query is `:many`,
  its statement is a SELECT (the statement's own verb — the first
  depth-0 `SELECT`/`INSERT`/`UPDATE`/`DELETE`/`REPLACE`/`VALUES`/`TABLE`,
  past any `WITH` list — is `SELECT`; DML `RETURNING` is bounded by
  what it modifies; a depth-0 `UPDATE` in `FOR [NO KEY] UPDATE` or a
  `replace(…)` call does not make a SELECT DML, and an unbounded
  row-locking SELECT is the costliest case), and **any** verification rendering lacks a depth-0
  `LIMIT` (other than `LIMIT ALL`) or `FETCH FIRST|NEXT`. A LIMIT that
  only some renderings carry still warns: the other shapes are
  reachable. (No slot admits a guarded LIMIT today; checking every
  rendering keeps this true if one ever does.) A subquery's LIMIT does
  not bound the outer result. `:one`/`:maybe-one` never warn.
- **L004** fires on a depth-0 `OFFSET <expr>` and on MySQL/SQLite's
  `LIMIT <offset>, <count>` when the offset is a caller-driven paging
  offset: an operand built ONLY from placeholders, numbers,
  arithmetic, parentheses and casts, holding at least one placeholder
  (`(:page - 1) * :size`, `:o::int`). A constant, a subquery, or any
  other token is silence. `offset` is non-reserved on MySQL/SQLite, so
  a column, table, or alias of that name is not the clause: the
  keyword counts only after a depth-0 `LIMIT`, or when it does not
  follow a token that introduces an operand or relation (`SELECT`,
  `BY`, `WHERE`, `BETWEEN`, `FROM`, `,`, `(`, an operator, …), and the
  operand whitelist closes the rest of the class (a table alias before
  `LEFT JOIN` has the "operand" `LEFT`). Over-skipping only ever costs
  a missed warning.

### 3.5 SQLETCHL005 — parameter-vs-column type mismatch

Inputs: the maximal tree's top-level relations resolved against the
catalog (the R3 resolver), and the parameter types `resolvedChecks`
settled (oracle-inferred on Tier 1, `-- @param` on Tier 2). Only the
TOP-LEVEL statement's WHERE/ON predicates are inspected (a subquery's
columns would need scope resolution the facade does not model), a
relation named like a statement-level CTE is skipped (the CTE shadows
the base table, so the catalog's types are not the compared column's;
the R3 resolver alone would hand back the base table's), set
operations are skipped (SQLite's `Relations()` is the first core's),
and the operands must be a bare column vs. a bare placeholder (or an
`IN` list of them). PostgreSQL also accepts one `::type` /
`CAST(… AS type)` around the placeholder, and then judges the pair at
the CAST's type (resolved like a `-- @param` type name; unresolvable ⇒
silent), not the parameter's inferred type: PostgreSQL infers one type
per parameter from its first use, so in `price = :v AND id = :v::int4`
`$1` is numeric yet `id = $1::int4` is index-safe. A double cast is
skipped. MySQL takes
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

The MySQL wire-code flag bits are repeated in `internal/lint` to keep
it free of a dialect-implementation import; `TestPerfTypes_MySQLFlagsAgree` pins them.

## 4. `-- @nolint`

```sql
-- name: FindByEmail :many
-- @nolint SQLETCHL001, SQLETCHL003 (expression index users_lower_email_idx; ≤ 5 rows per address)
SELECT id FROM users WHERE lower(email) = :email;
```

- Form: `-- @nolint SQLETCHnnn[, SQLETCHnnn…]` with an optional
  trailing `(reason)` (optional like `@policy-apply`'s: the directive is
  visible in review either way; a reason is encouraged). Any
  `@nolint`-shaped comment (`-- @nolint`, `-- @nolint:`, `-- @nolint(…`,
  `-- @nolint X`) is
  the directive, so a malformed one is SQLETCH016 rather than a
  silently ignored comment; `-- @nolintX` is not the directive.
- Deliberately NOT golangci-lint's grammar: there is no bare
  suppress-all form (it would also hide every lint added later), and
  codes follow a space, not `:`. Both habits are SQLETCH016 with a
  message spelling the sqletch form.
- All-or-nothing: a directive naming any non-performance or unknown
  code records **none** of its codes.
- Scope: the whole query (every rendering). It stays in the skeleton
  verbatim like every directive (so adding one re-keys that query's
  oracle entries — the rendered SQL changed).
- **SQLETCHL006 (unused @nolint), decision 2026-10-02:** an `@nolint`
  whose code does not fire on the query is a warning at the directive,
  because a stale suppression would hide that lint's next regression.
  Each pass judges only the codes it owns: the scan pass judges
  L001–L004, the resolved pass judges L005 — so a run that cannot reach
  the catalog-dependent pass (an LSP cache miss) never calls a
  SQLETCHL005 nolint stale. On SQLite, where SQLETCHL005 never fires, an
  `@nolint SQLETCHL005` is always reported unused. SQLETCHL006 itself
  cannot be allowed (a stale suppression must not silence its own
  staleness report).

## 5. Examples

`examples/` enables lints (`lint: true`) and stays warning-free: `AllAuditActions` (PostgreSQL) and
`CountByStatus` (SQLite) are GROUP BY results bounded by a vocabulary
and carry a justified `@nolint SQLETCHL003`; `UserAuditActions` gained
`LIMIT :limit` (its result is genuinely unbounded).

## 5a. Evidence

- The L005 whitelist is pinned against the real planners
  (`internal/e2e/perf_devdb_test.go`).
- The rewrites the hints and manual 14 recommend for L001/L002 are
  pinned the same way, per dialect (`perf_rewrite_devdb_test.go`). This
  is what showed the original L002 hint ("anchor the pattern, `:q ||
  '%'`") to be wrong on PostgreSQL (a B-tree never serves a
  parameterized LIKE in the generic plan, `text_pattern_ops` included)
  and SQLite (the `||` expression is never optimized); the hint now
  recommends the range form every dialect indexes.
- Editor/CLI parity after a real `generate`, L005 included
  (`perf_lsp_parity_devdb_test.go`), and an offline examples gate
  (`cli.TestExamplesAreLintClean`).

## 6. Known limits / follow-ups

- Lexical keyword handling: a non-reserved keyword used as a column
  name (`group`, `offset`, …) in a predicate can end the predicate
  early — a missed warning, never a wrong one.
- The editor grammars (doc 11) do not highlight `@nolint` specially; it
  renders as a comment.
- Expression-index awareness (and index awareness in general, design
  conversation "A1") needs indexes in the catalog — a cache format
  change, deliberately out of scope here.
