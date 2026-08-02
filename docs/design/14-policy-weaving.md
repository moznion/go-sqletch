# sqletch Design — 14: Cross-query policy weaving and enforcement

**Status: DRAFT — not implemented, not scheduled.** This document
expands the two recorded notes (spec "Future Roadmap"; `08-later-phases.md`
§"Beyond 1.0") into a design. Several points below are marked
**DECISION NEEDED** — they change the template language or the
generated API surface, so per `CLAUDE.md` §"Document authority" they
must be settled with the user before implementation, and the outcome
reflected into `docs/spec.md`.

A *policy* is a boolean predicate declared once in `sqletch.yaml` and
woven, at compile time, into every query that touches a designated
table — plus a check proving that no reachable shape in the codebase
reaches those tables unscoped.

## 1. What this buys — the benefit case

The existing answer to cross-layer filtering is `@filter-tree!`
(spec §"Use Case 5"): a query declares a predicate vocabulary and the
caller passes a typed tree. That is the right tool when the *caller*
decides the filter. Policy weaving addresses the opposite situation:
the filter is not a caller's choice at all, it is an invariant of the
schema, and the compiler should be the one enforcing it.

Concretely, over the status quo:

1. **Default-scoped instead of opt-in-scoped.** `@filter-tree!` only
   protects queries that declare it; a query added next month with no
   filter-tree is silently unscoped, and nothing in the build objects.
   With a policy, a new query touching `orders` is scoped the moment it
   compiles. The failure mode flips: *forgetting* becomes impossible,
   *opting out* becomes a deliberate, reviewable line. This is the same
   inversion `@filter-tree!` performs for one query, lifted to the
   codebase.

2. **A codebase-level invariant, provable.** The enforcement pass
   states a property no per-query construct can state: *no reachable
   shape of any query touches a designated table without the scoping
   conjunct.* Because sqletch already enumerates every reachable shape
   (spec §"Verification Model"), that quantifier is checkable, not
   aspirational — the same machinery that proves every shape parses
   proves every shape is scoped.

3. **The predicate is written once.** Spelled per query, a tenant
   predicate is duplicated across N templates, each with its own alias
   and each independently mis-writable (`o.tenant_id` vs. a copy-paste
   `u.tenant_id` after a rename). Weaving reduces N hand-written,
   individually-auditable copies to one config line whose expansion is
   mechanical.

4. **Schema change propagates by compiler, not by grep.** Adding a
   table to the policy, or renaming the scoping column, is one config
   edit; every query that can no longer satisfy the policy is reported
   with a span. The per-query alternative requires finding all call
   sites by hand and offers no completeness guarantee that you did.

5. **It covers DML, where the stakes are highest.** An unscoped
   `SELECT` leaks; an unscoped `UPDATE`/`DELETE` destroys another
   tenant's data. A policy applies to a table, not to a query kind, so
   the destructive statements are covered by the same declaration —
   and they are exactly the queries where a filter is easiest to forget
   because the developer is thinking about the write, not the scope.

6. **Auditability as an artifact.** `explain` reports per-query policy
   coverage, so "which queries are unscoped, and why" is a command's
   output rather than a code-reading exercise. Opt-outs are greppable
   annotations whose *set* can be diffed in review — adding one is a
   visible event.

7. **A compile-time alternative to RLS that works on every dialect.**
   The database-side answer is PostgreSQL row-level security, which
   costs a per-connection session-variable discipline (a missed `SET`
   is a leak), is a runtime failure mode, and does not exist on MySQL
   or SQLite. Weaving is checked before the program runs and is
   dialect-independent. (It is not a *replacement* for RLS as
   defense-in-depth — see §9.)

8. **Verification still sees the real SQL.** Because expansion happens
   before rendering, the SQL that gets `PREPARE`d, `EXPLAIN`ed, typed,
   and cached *is the scoped SQL*. Unlike a policy layer bolted on at
   runtime (a query-rewriting wrapper, a driver hook), there is no
   window in which the verified statement and the executed statement
   differ.

What it does **not** buy, stated up front so the doc is not oversold:
it constrains only sqletch-generated queries (hand-written SQL and
other ORMs in the same process are untouched); it expresses conjunctive
row filters, not column masking or per-role policies; and it cannot
protect against a compromised process that sets the scoping parameter
to the wrong value — it guarantees the predicate is *present*, not that
its argument is *correct*.

## 2. Position in the pipeline

The load-bearing decision, already fixed by the recorded notes:
weaving happens **after the P1 scan and before rendering**.

```
scan (P1)  ─→  [WEAVE]  ─→  render (P2)  ─→  rules  ─→  oracle  ─→  codegen
                  ▲
            sqletch.yaml policies
```

Everything downstream of the arrow sees an ordinary `template.Item`
sequence and cannot tell a woven conjunct from a hand-written one.
Therefore:

- R1–R9, `ast.Render`/`RenderShape`, the type oracle, nullability,
  shape keys, `codegen.BuildFrags` and the `runtime.Compose`
  conformance invariant are **unmodified**. The soundness argument of
  the spec applies verbatim to the woven template.
- A woven predicate is checked like any other fragment: if it does not
  parse as one complete node in its slot, that is a normal R1
  diagnostic (with the span question of §6.4).
- Static expansion, the committed cache, and `explain --enumerate` need
  no awareness of policies.

This is the whole reason the feature is cheap in soundness terms, and
it is why no alternative placement (weaving into the rendered SQL
string, or into the generated Go) should be entertained.

## 3. Configuration surface

Proposed addition to `sqletch.yaml` (`internal/config`), following the
existing style of `static_expansion` / `filter_tree_caps`:

```yaml
policies:
  - name: tenant_scope
    tables: [orders, order_items, invoices]
    predicate: "{} .tenant_id = :tenant_id"     # see §4.3 for the alias form
    param:
      name: tenant_id
      type: bigint                              # Tier 2 dialects require it
    applies_to: [select, update, delete]        # default: all four
```

Notes:

- `internal/config` decodes strictly (unknown keys are `SQLETCH300`),
  so adding the key is backward compatible for *reading* existing
  configs, but a config using `policies:` is rejected by older sqletch
  binaries. That is the desired direction of failure.
- `Config` is part of the v1.0 frozen surface (`12-v1.md`), so this key
  is additive-only and must be designed once, not iterated.
- Multiple policies are allowed; they are woven in declaration order,
  each as its own conjunct. Determinism: never iterate a map into the
  woven output.

## 4. The weaving algorithm

### 4.1 Two passes are required

Target selection ("does this query touch `orders`?") and alias binding
("what is `orders` called *here*?") both need the parsed relation set,
which does not exist at scan time. But weaving must happen before
rendering. The resolution is a pre-pass that reuses machinery already
present:

1. Render the **maximal** rendering of the unwoven template
   (`ast.Render`).
2. Parse it with the dialect frontend and take `Tree.Relations()` —
   `[]dialect.RelRef{Table, Alias, Loc, Join}`. This is exactly what
   `rules.newResolver` already does (`internal/rules/resolved.go:164`).
3. Decide targets and aliases from that relation set.
4. Weave into `q.Items`, producing a new `*template.QueryTemplate`.
5. Run the ordinary pipeline on the woven template from `ast.Render`
   onward.

Step 1–2 are pure and offline (no catalog needed for the relation
list), so the LSP's offline checker can run them too. Cost is one extra
render+parse per query, which is small next to the oracle round trip.

An unwoven maximal rendering that fails to parse yields the normal
`SQLETCH100` before any weaving is attempted.

### 4.2 Insertion point

The conjunct is inserted into the WHERE clause (HAVING is not a policy
slot — a policy is a row filter). Finding the position in the item
stream needs the clause context the scanner already tracks
(`clauseCtx` / `ctxWhere`, `internal/template/scanner.go:33`). The
scanner should record, per query, the offset of the WHERE-clause
conjunct insertion point (immediately after `WHERE` and its anchor
token), alongside the spans it already records.

Two sub-cases:

- **The query has a WHERE clause.** Insert an unconditional
  `AND <predicate>` conjunct as a `template.Skeleton` item at the
  recorded point.
- **The query has no WHERE clause.** The weaver must synthesize
  `WHERE <predicate>` at the clause position (before GROUP BY /
  ORDER BY / LIMIT / RETURNING). This is the common case for
  `DELETE FROM orders` — i.e. the case with the most to gain — so it
  cannot be deferred.

**Interaction with R6.** A woven conjunct is unconditional, so it
satisfies the R6 anchor requirement (`SQLETCH113`). A query whose
conjuncts are all optional would therefore *stop* being an R6 error
once a policy applies to it. That is a silent change in what the
compiler accepts, and it makes a template's validity depend on config.
**Recommendation:** run the R6 anchor check on the *unwoven* template
(it is a lexical check, `rules.CheckLexical`, and already runs before
resolution), so template validity stays config-independent. Woven
conjuncts then satisfy the SQL grammar without being counted as the
author's anchor.

### 4.3 Alias binding

The policy predicate must reference the designated table as it is
named in the target query. The predicate is written with a placeholder
for the relation reference — `{}` in the §3 sketch — which the weaver
substitutes with the alias, or the table name when there is no alias,
quoting as the dialect requires.

The relation set from §4.1 supplies this directly. A predicate that
references anything *other* than the designated relation and its own
`:param`s is out of scope for v1 of this feature: it would need
resolution against the target query's full scope, and it invites
policies that break under `@if-present` joins.

## 5. Decisions the spec must make

Each item below changes observable behavior. Options and a
recommendation are given; none is settled.

### D1 — The designated table appears more than once

Self-joins, `orders o1 JOIN orders o2`, or the table appearing both in
the outer query and in a subquery.

- (a) Weave one conjunct per occurrence.
- (b) Weave into the outermost occurrence only.
- (c) Reject the query with a diagnostic; the author must opt out
  explicitly or restructure.

**Recommendation: (a) for occurrences at the same query level, (c) for
occurrences inside subqueries** (see D6). A policy means "every row of
this table you read is scoped"; skipping one occurrence would silently
weaken that, and (c) keeps the unmodeled case loud rather than wrong.

### D2 — The designated table is on the nullable side of an outer join

Adding `o.tenant_id = :tenant_id` to WHERE for a `LEFT JOIN orders o`
turns the outer join into an inner join: rows that previously survived
with NULLs disappear. The woven query is *more* scoped but computes a
different result than the author wrote.

- (a) Weave into the join's `ON` clause instead of WHERE when the
  relation is on the nullable side.
- (b) Reject with a diagnostic (the author restructures or opts out).
- (c) Weave into WHERE anyway and document the semantics change.

**Recommendation: (b), with (a) as a later refinement.** (c) is a
correctness trap — a compiler silently changing which rows a query
returns is exactly the class of surprise this project exists to avoid.
Note that `rules.CheckResolved` already has `RelRef.Join` and detects
an analogous planner-sensitive case for `FOR UPDATE`
(`internal/rules/resolved.go:118`), so the detection is cheap.

### D3 — How the woven parameter reaches the call site

The largest open question. `:tenant_id` must get a value at runtime.

- (a) **Ordinary parameter.** It appears in every affected query's
  generated `Params` struct. Simple, consistent with everything else,
  and typed by the oracle. But every call site changes, and the
  ergonomic gain over `@filter-tree!` shrinks to "you cannot forget the
  predicate" (still the main benefit, per §1.1).
- (b) **Ambient value on the Queries handle.** The generated
  constructor takes the policy value once (`gen.New(db,
  gen.WithTenant(id))`) and every woven bind draws from it. Best
  ergonomics; but it introduces hidden state into generated code, and
  the handle becomes request-scoped rather than process-scoped, which
  is a significant change to the generated API's contract.
- (c) **`context.Context` extraction** via a user-supplied function.
  Idiomatic for request scoping, but makes a missing value a runtime
  error and puts an untyped `any` in the middle of a typed pipeline.

**Recommendation: (a) for the first implementation**, with (b)
evaluated afterwards on top of it. (a) keeps the generated API's
current contract intact — which `12-v1.md` freezes — and delivers the
enforcement benefit in full. (b) is a strictly larger change and should
not be a prerequisite.

### D4 — Diagnostic span attribution

A woven fragment can fail R1 (not a complete node), fail to type, or
fail the oracle. The span then points at text that exists in no
template file.

- (a) Attribute to `config.Config.Path` (the `sqletch.yaml` line of the
  policy), following `cli.versionPinDiag`, which already emits
  `SQLETCH200` against the config path.
- (b) Attribute to the target query's `HeaderSpan` with the config
  location in the message.

**Recommendation: (a) for defects in the policy itself** (it is the
config that is wrong, and the same defect will fire for every affected
query — reporting it once against the config is right), **(b) for
defects arising from the interaction** with a specific query. This
requires `diagnostics.Span` to carry config-file spans, which it can
already do (`Span.File` is a path), plus a dedup so a broken policy
does not emit one diagnostic per query.

### D5 — Guard set of the woven predicate

**Recommendation, not expected to be controversial:** the woven
conjunct has the **empty guard set** — the same discipline
`@filter-tree` predicates follow (spec: "Predicates have empty guard
sets"). It is skeleton text: present in every shape, referencing only
constant-skeleton scope. Consequently it may not reference a relation
introduced by an `@if-present` join, and if the designated table *is*
such a relation, the query is rejected (it cannot be unconditionally
scoped). Nullability is unaffected, since the analyzer's
"never narrow from guarded fragments" rule concerns guarded fragments
and this one is not guarded.

### D6 — Subqueries, CTEs, and `INSERT … SELECT`

`Relations()` reports relations in subqueries and CTE bodies, but the
weaver has no modeled insertion point inside them, and R3 already
documents that unqualified refs inside subquery scopes are unmodeled
(`internal/rules/resolved.go:18`).

**Recommendation:** v1 of the feature weaves at the top level only, and
a designated table appearing *only* inside a subquery or CTE is a
diagnostic ("this query touches a policy-designated table in a position
sqletch cannot scope; opt out explicitly or restructure"). Loud and
incomplete beats silent and incomplete. `INSERT … SELECT` is covered
insofar as its `SELECT` is a top-level statement body; a plain
`INSERT … VALUES` cannot be row-filtered at all and is simply not a
policy target (see `applies_to`).

## 6. Enforcement

Weaving alone does not prove absence of leaks: a query can name the
table in a position the weaver skipped, or the author can opt out. The
enforcement pass is the second half.

### 6.1 The check

A new `CheckResolved` pass (`internal/rules/resolved.go`), running on
the parsed maximal rendering of the **woven** template with the
catalog available:

> For every relation in `Tree.Relations()` whose table is designated by
> a policy, a conjunct matching that policy must be present in the
> query's WHERE clause in **every** shape.

"In every shape" is what makes this a real check rather than a
formality: a conjunct sitting inside an `@if-present` body satisfies it
in the guard-on shape and not in the guard-off one. Since the woven
conjunct is unguarded by D5, the check reduces to "the conjunct is in
the skeleton", and the interesting cases are hand-written scoping and
opt-outs.

The pass belongs in `cli.resolvedChecks` (`internal/cli/offline.go:230`),
which is the single shared catalog-dependent pass for `pipeline.Run`
and the LSP's `OfflineChecker` — extend it, do not fork it. Consequence:
**policy violations appear live in the editor.**

### 6.2 Opt-out

A per-query annotation, scanned like `-- @param` / `-- @column`:

```sql
-- name: ListAllOrdersForBackfill :many
-- @policy-optout: tenant_scope (batch job; runs outside any tenant)
SELECT ...
```

Requiring a reason after the policy name makes the annotation
self-documenting in review and in the `explain` report. An opt-out
naming a policy that does not exist, or that does not apply to the
query, is itself a diagnostic — otherwise renaming a policy silently
disarms every opt-out that mentioned it.

### 6.3 `explain` coverage report

`sqletch explain` gains a per-query policy section: for each query, the
policies that apply, whether each is woven or opted out (with the
reason), and a summary count. In `--format json` this is an additional
object, so CI can assert on it.

### 6.4 Proposed diagnostic codes

The `1xx` rules band is free at 120–121 and from 124 up
(`internal/diagnostics/diagnostics.go:72-88`); the `3xx` band covers
config. Every new code must also be added to
`docs/manual/08-diagnostics.md` — `diagnostics.manual_test.go` fails
otherwise.

| Code | Meaning |
| --- | --- |
| `SQLETCH124` | A query touches a policy-designated table without the scoping conjunct in every shape, and has no opt-out. |
| `SQLETCH125` | A policy-designated table appears in a position sqletch cannot scope (subquery/CTE per D6, nullable outer-join side per D2). |
| `SQLETCH126` | `-- @policy-optout` names an unknown policy, or one that does not apply to this query. |
| `SQLETCH303` | A policy declaration in `sqletch.yaml` is malformed, or its predicate does not parse as one complete boolean node. |

## 7. Cache impact — none

Verified against the implementation rather than assumed:
`cache.Store` keys oracle entries by `queryHash(fingerprint, renderedSQL)`
(`internal/cache/store.go:75`), and `Fingerprint` is computed from
dialect, server version, and schema files only
(`internal/cache/store.go:35`). Weaving changes the rendered SQL, so
woven queries re-key naturally and a policy edit invalidates exactly
the affected entries. The catalog snapshot is schema-derived and
unaffected.

**No `FormatVersion` bump is required, and the policy config must not
enter the fingerprint** — putting it there would invalidate the whole
catalog on every policy edit for no benefit.

## 8. Testing plan

Per `CLAUDE.md` §"Working conventions" — test-first, every layer, and
rejected inputs asserted down to their `SQLETCHnnn` code.

- **`internal/config`**: policy decoding, validation, strict-unknown-key
  behavior, `SQLETCH303` cases.
- **Weaver unit tests**: golden woven item streams for each insertion
  case (existing WHERE, no WHERE, DELETE, UPDATE, alias vs. bare table,
  multiple policies, multiple occurrences per D1). Because the woven
  template is an ordinary `QueryTemplate`, the golden artifact is the
  maximal rendering — readable, and it doubles as the D2/D6 rejection
  fixture set.
- **Determinism**: identical inputs produce byte-identical woven
  output; multiple policies weave in declaration order.
- **`internal/rules`**: the enforcement pass, including the every-shape
  quantifier (a hand-written scoping conjunct inside `@if-present` must
  *fail*, and that is the test that proves the check is not a
  formality), plus opt-out handling and `SQLETCH126`.
- **Idempotence**: weaving an already-scoped query (hand-written
  conjunct present) must not double-weave.
- **LSP**: a policy violation surfaces through `OfflineChecker` with the
  right span, and a broken policy degrades the server rather than
  crashing it (doc 10's discipline).
- **E2E (devdb, all three dialects)**: a seeded two-tenant dataset where
  the *unwoven* query would return the other tenant's rows and the woven
  one does not — the test that would actually catch a regression in the
  feature's purpose. Plus an opt-out query that legitimately sees both.
- **Conformance**: `TestComposeConformance` must pass unchanged on woven
  templates; that it needs no modification is the evidence for §2's
  claim that the runtime is untouched.

## 9. Relationship to other mechanisms

- **`@filter-tree!`** — complementary, not superseded. Policies express
  invariants the caller may not choose; `@filter-tree!` expresses
  filters the caller *does* choose. A query may have both.
- **Row-level security** — orthogonal defense in depth. Weaving catches
  the error at compile time on every dialect; RLS catches it at runtime
  in the database including for non-sqletch clients. Recommending
  weaving as a *replacement* for RLS would be wrong, and the manual
  should say so.
- **`@if-present` scoping joins** — a policy-designated table reachable
  only through a guarded join cannot be unconditionally scoped (D5) and
  is rejected.

## 10. Phasing

1. Config surface + weaver + golden tests (§3, §4) — no enforcement yet;
   the feature is already useful because woven queries are verified.
2. Enforcement pass + opt-out annotation + diagnostics (§6).
3. `explain` coverage reporting (§6.3) and manual chapter.
4. Deferred: D3(b) ambient parameter binding; D2(a) `ON`-clause weaving;
   subquery/CTE weaving (D6).

Steps 1–2 are the security-relevant core; 3 is what makes it auditable.
