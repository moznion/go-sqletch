# sqletch Design — 14: Cross-query policy weaving and enforcement

**Status: IMPLEMENTED (2026-08-02) — decisions D1–D6 settled with the
user, all per the recommendations recorded below; §10 phases 1–3
shipped (step 4 remains deferred).** This document expands the two
recorded notes
(spec §"Stability and Beyond v1.0"; `08-later-phases.md` §"Beyond
1.0") into a design. The settled outcomes are reflected in
`docs/spec.md` §"Cross-Query Policies". §11 records the mechanical
resolutions made during pre-implementation reconnaissance.

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
  WINDOW / ORDER BY / LIMIT / RETURNING — and before a tail-slot
  construct that *replaces* one of those clauses: `@order-by`, and a
  `@choose` whose cases classify as GROUP BY/ORDER BY clauses, bound
  the WHERE slot exactly like the literal keyword would). This is the
  common case for `DELETE FROM orders` — i.e. the case with the most
  to gain — so it cannot be deferred.

In both sub-cases (and in the `ON`-clause case of the D2 refinement)
every spliced predicate occurrence is wrapped in its own parentheses —
`AND (<predicate>)` / `WHERE (<predicate>)` — see §11.6.

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
recommendation are given. **All six were settled with the user on
2026-08-02, each per its recommendation**; the settled outcome is
restated at the end of each item.

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

**Settled 2026-08-02: as recommended** — one conjunct per top-level
occurrence; subquery occurrences are rejected (D6).

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

**Settled 2026-08-02: (b)** — reject with `SQLETCH125`; `ON`-clause
weaving stays a deferred refinement (§10 step 4). Detection uses
`RelRef.NullableSide`, not `RelRef.Join` — see §11.2.

**Refined 2026-08-02 (second settlement, the §10 step 4 follow-up):
(a), automatic.** A designated table on a null-extended side is woven
into its own join's `ON` clause instead of `WHERE`: the outer join's
row set is preserved and only the joined rows are scoped — the
*correct* scoping for an outer join, so it replaces the rejection
rather than hiding behind a config flag. The rejection remains for
the cases `ON`-weaving cannot express: `USING`/`NATURAL` joins (no
`ON` expression to extend), guarded joins (D5, unchanged), and any
occurrence whose computed insertion point does not land in skeleton
text. Enforcement (§6.1) is extended in kind: for a nullable-side
occurrence the matching conjunct must be unconditional skeleton text
of *that join's* `ON` clause.

**Placeholder-free carve-out.** The `ON`-clause rule exists because a
WHERE conjunct that references the null-extended relation's columns
turns the outer join inner (its NULL-extended rows fail the
predicate). A predicate with no `{}` placeholder references no joined
columns at all, so that hazard does not exist: the weaver deliberately
emits it as a single WHERE conjunct scoping every occurrence — the
null-extended rows evaluate it exactly like every other row.
Enforcement mirrors the emission rule: for a placeholder-free
predicate, presence is checked in the WHERE clause for ALL
occurrences, nullable-side ones included (checking the join's `ON`
there would false-reject the weaver's own output).

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

**Settled 2026-08-02: (a)** — the woven parameter is an ordinary
parameter in the affected queries' `Params` structs, typed by the
oracle (Tier 1) or the policy's `param.type` (Tier 2). (b) remains a
possible later layer (§10 step 4), never a replacement.

**Amended: (a) as an argument, not a params field.** (a) was chosen
partly on the premise, recorded above, that "every call site changes".
It does not. Go has no mandatory struct field: a keyed composite
literal that omits the woven parameter compiles and yields the zero
value, so existing call sites keep building after a policy is added
and send `tenant_id = 0` / `''`. The woven predicate then matches
nothing. The query does not fail — it returns an empty result, which
at a call site like "look the user up by id" is indistinguishable from
"no such row".

Observed by adopting a `users` policy in a downstream service: `go
build ./...` succeeded, every call site compiled unchanged, and
authentication began failing for every user with a 401 that reads
exactly like a wrong password. Only tests running against a real
database caught it.

So the enforcement this document builds — SQLETCH124 proving the
conjunct is present in every reachable shape — was being handed to a
Go boundary that dropped it. The fix keeps (a)'s substance (an
ordinary, oracle-typed value; no ambient state, no context extraction)
and changes only where it sits: the parameter is an argument of the
generated method. Omitting it is then a compile error, which is what
`12-policies.md` already claimed it was.

The cost is that adding a policy breaks every call site of every query
it touches. That is the intended shape of the change: a security
control adoptable without revisiting callers is one whose absence
nobody notices either.

The same reasoning applies to `@filter-tree!`, whose `Scope` field had
the identical hazard — omitted from a keyed literal it was nil, and
only `ErrFilterRequired` at runtime stood between that and an unscoped
read. It is now an argument too.

For the tree the guarantee goes one step further than it can for a
policy parameter. `Tree` became a value type, so `nil` is not a Tree:

    q.FilterUsers(ctx, nil, gen.FilterUsersParams{Limit: 20})
    → cannot use nil as runtime.Tree value in argument to q.FilterUsers

Both shapes a forgotten scope takes — omitting the argument and passing
`nil` — are now compile errors. The one zero left is `runtime.Tree{}`,
written out in full, which is not something a caller produces by
accident; `ErrFilterRequired` refuses it, and `Tree.IsZero` keeps it
distinct from `Unscoped()` so that "did not decide" and "decided not to
scope" cannot collapse into each other.

A policy parameter cannot be hardened this way: its underlying type is
an ordinary `int64` or `string`, and Go has no type whose zero is
unrepresentable. "Cannot omit" is the whole of what is achievable
against forgetting.

**Amended: a distinct named type per policy parameter.** What CAN be
hardened is the next failure mode over: two policy arguments of the
same underlying type swapped at a call site — `(orgID, tenantID)` for
`(tenantID, orgID)` compiles with plain `int64`s and scopes each
predicate by the other policy's value. Codegen therefore declares one
named type per woven parameter name in `policy.gen.go`
(`type TenantID int64`, name = the parameter's Go name), used in every
generated signature; the wrong order is then a type mismatch. The
decisions:

- **One type per parameter name, package-wide** — shared by every
  query, and every policy, that binds that name, so the value is
  passable across call sites. Parameters that agree on the name but
  resolve to different Go types are `SQLETCH310`, never a silently
  different second type; likewise a policy type name colliding with a
  query-generated type (the check runs after all queries have claimed
  their names, in sorted order for determinism).
- **The driver never sees the named type**: the bind site converts
  back to the underlying type (`int64(tenantID)`), so wire behavior,
  renderings, and the cache fingerprint are untouched.
- **Untyped constants still convert** (`q.AllAudit(ctx, 1, …)`) — Go
  semantics; the protection is against swapping *variables*, which is
  where real values live.
- The zero (`TenantID(0)`) stays representable; presence, not
  correctness, remains the guarantee.

**(b)'s design was settled 2026-08-02 for when it is picked up**
(implementation deliberately not scheduled): a compile-time switch —
`param.binding: ambient` in the policy declaration — excludes the
parameter from the affected queries' `Params` structs; the generated
constructor gains variadic options (`gen.New(db, gen.WithTenantID(v))`,
source-compatible with existing `New(DBTX)` callers) and the option is
mandatory: a handle constructed without it errors on first use of any
affected query, before any SQL is composed. Ambient-as-overridable-
default was rejected: a zero tenant ID and "unset" would be
indistinguishable, which is exactly the failure mode a security
feature must not have.

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

**Settled 2026-08-02: the hybrid as recommended** — policy-intrinsic
defects (`SQLETCH303`) attribute to `config.Config.Path` once;
per-query interaction defects (`SQLETCH124`/`125`) attribute to the
query's `HeaderSpan` and name the policy and config path in the
message.

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

**Settled 2026-08-02: as recommended** — empty guard set. (This falls
out of the implementation for free: a woven `template.Skeleton`
produces no `FragRange`, so `rules.resolver.fragAt` finds no guards.)

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

**Settled 2026-08-02: as recommended** — top-level weaving only; a
designated table visible only inside a subquery or CTE body is
`SQLETCH125`. Note the premise correction in §11.1: `Relations()`
does *not* report subquery/CTE relations, so this detection needs a
new `Tree` capability. That correction also narrows the
`INSERT … SELECT` claim above: `Relations()` reports only the INSERT
target, never the select body's tables, so v1 *rejects* (`SQLETCH125`)
an `INSERT … SELECT` reading a designated table rather than weaving
its body — the spec states this.

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
| `SQLETCH125` | A policy applies to this query but cannot be woven: the designated table is in a position sqletch cannot scope (subquery/CTE per D6, nullable outer-join side per D2, guarded-join relation per D5), its bound name is not a bare identifier (§11.3), or the query already declares the policy's parameter name with a conflicting *type* (§11.4) or a conflicting *kind* — an optional `@if-present`/`@when` control parameter or a `@filter-tree` `@predicate` argument (§11.4). Opt out explicitly or restructure. |
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

Step 4 status (2026-08-02): D2(a) `ON`-clause weaving is
**implemented** (see the D2 refinement note); D3(b) is designed but
unscheduled (see the D3 note); subquery/CTE weaving stays rejected —
its scope-resolution model is the same unmodeled territory R3
deliberately skips, and loud rejection remains the design.

## 11. Mechanical resolutions (pre-implementation reconnaissance)

Recorded per `CLAUDE.md` §"Document authority": minor mechanical
choices made directly, reflected here. Each was forced by a gap
between this doc's assumptions and the code as it exists.

### 11.1 `Relations()` does not see subqueries — new `Tree` capability

§D6 assumed `Tree.Relations()` reports relations inside subqueries and
CTE bodies. It does not: the PostgreSQL frontend walks only the
statement's own FROM/USING/target relations and emits a placeholder
`RelRef` with empty `Table` for a `RangeSubselect`
(`postgres/frontend.go:292`). A designated table hidden in a subquery
would be *invisible*, so D6's loud rejection needs a new capability:

`dialect.Tree` gains `DeepTables() []TableRef` with
`TableRef{Name string; Loc int}` — every base-table name referenced
anywhere in the statement, including subquery and CTE bodies
(`Loc` is -1 where the parser exposes no offset, e.g. TiDB relation
nodes; the diagnostic then falls back to the query's `HeaderSpan`).
It deliberately includes references to CTE *names*: a CTE shadowing a
designated table is conservatively treated as touching it — a false
positive is a loud diagnostic with an opt-out, never a silent leak.
Extending `Tree` is compile-visible across all three frontends, which
is exactly the property we want for a soundness-relevant capability.

Each frontend's `DeepTables()`/`Relations()` must cover **every**
position a designated table can appear, including on data-modifying
statements. The SQLite facade originally read `CTEs()` from the SELECT
statement only and never walked a DML `WITH` clause or an
`UPDATE … FROM` source, so a designated table inside
`WITH x AS (SELECT … FROM t) UPDATE …` or `UPDATE … FROM t …` appeared
in neither method and was silently un-woven and un-rejected — the exact
leak D6 forbids. The facade now returns DML `WITH` CTEs and walks the
`UPDATE … FROM` source in `Relations()`, `DeepTables()`, `DerivedRels()`,
and `ColumnRefs()`, matching the PostgreSQL (protoreflect) and MySQL
(TiDB `Accept`) walks. A hidden-in-`WITH` occurrence now surfaces in
`DeepTables()` but not `Relations()`, so the per-name count goes
negative and the weaver raises `SQLETCH125` as intended; an
`UPDATE … FROM t` occurrence is a top-level relation and is scoped
normally.

### 11.2 D2 detection uses `RelRef.NullableSide`

`RelRef.Join` describes only how the relation itself was introduced;
`RelRef.NullableSide` is the propagated answer through nested joins
(right of LEFT, left of RIGHT, either side of FULL — the nullability
analysis input). The FOR UPDATE precedent uses `Join` because it cares
about one directly-guarded LEFT JOIN; the policy check needs the
general property, so D2 rejects when the designated relation has
`NullableSide == true`.

### 11.3 `{}` substitution is restricted to bare identifiers

No identifier-quoting facility exists in `internal/dialect`, and
adding one for this feature alone is surface for little gain: aliases
and table names in real templates are bare `snake_case` identifiers.
v1 rule: the name bound to `{}` (alias if present, else table name —
`relInfo.name()`'s exact rule) must match `[A-Za-z_][A-Za-z0-9_]*`;
otherwise the query is rejected with `SQLETCH125`. Likewise a policy's
`tables:` entries must be bare lowercase identifiers (`SQLETCH303`
otherwise). Revisit only if a real schema demands quoted names.

### 11.4 The policy parameter's type hint

The woven `:param` flows through the ordinary machinery (it lands in
`ParamsSeq` via `emitVerbatim`, and into the generated `Params` struct
via D3(a)). Its `param.type` from the config is injected into the
woven template's `TypeHints` — exactly as if the author had written
`-- @param` — but only when the query does not already hint that name.
An existing hint with the *same* type is fine (idempotent); a
*different* type is `SQLETCH125` (the query and the policy disagree
about one parameter — loud, per-query, opt-out-able). Tier 2 dialects
get their mandatory annotation this way; Tier 1 gets the normal
assert-against-oracle behavior (`SQLETCH213`).

The parameter's *kind* must agree too. D3(a) makes the woven parameter
a **required** argument precisely so a caller cannot omit it and
silently send the zero value; that guarantee is lost if the query
already declares the same name as a parameter of an incompatible kind,
because the weaver reuses the author's declaration rather than
overwriting it. So the weaver rejects (`SQLETCH125`) when the name is
already:

- an **optional** `@if-present` parameter — the woven conjunct is
  unconditional, so a `None` caller would bind `NULL` in every shape
  (`tenant_id = NULL`) and silently empty the result set: the exact
  "401 that reads like a wrong password" failure D3 exists to prevent;
- a **control** parameter (`@when` value, presence guard, or a
  `@filter-tree` `@predicate` argument) — binding it as an
  unconditional SQL value is what R9 forbids, and the enforcement pass
  (`SQLETCH124`) would wrongly accept the woven unconditional copy.

Only a plain, always-**required** value parameter of the same name is
sound to share (the D3(a) case): the policy binds the same required
value the caller already had to supply. The check keys on
scanner-populated fields (`Param.GuardBit`, per-occurrence
`InFilterTree`), so it does not depend on the R9 `Optional`
classification having run.

### 11.5 Wiring: one shared weave point, not two

`pipeline.Run` and `lsp`'s `OfflineChecker.analyzeFile` today
hand-duplicate the per-query catalog-free sequence
(`CheckLexical` → `ast.Renderings` → `CheckR1`). Weaving slots between
`CheckLexical` (which must see the **unwoven** template, §4.2's R6
rule) and `ast.Renderings` (which must see the **woven** one). Rather
than duplicating the weave call too, the sequence is factored into one
shared helper in `internal/cli`, mirroring the `resolvedChecks`
discipline — extend it, don't fork it. Everything downstream of the
helper reads the woven template, so codegen picks up the woven
parameter with no changes.

### 11.6 Every spliced occurrence is parenthesized

The predicate text is trusted config (§threat model) but it is not
precedence-neutral, and the splice site is textual. Two failure modes
of splicing it bare:

- A predicate with a top-level `OR` (`{}.tenant_id = :tid OR :tid IS
  NULL`) spliced as `WHERE p OR q AND <rest>` captures the query's own
  conjuncts under the OR's right arm — leak-shaped SQL, caught only by
  the enforcement pass's depth-0-OR poison (with a misleading
  "missing conjunct" message).
- A predicate carrying a depth-0 `AND` of its own (`{}.a = :x AND
  {}.b`, or `BETWEEN :lo AND :hi`) AND-splits into several segments in
  the woven clause, while the enforcement matcher's wanted token
  sequence keeps its `and` — a guaranteed `SQLETCH124` on the weaver's
  own output.

Both are solved uniformly by wrapping each spliced predicate
occurrence in its own parentheses: `AND (<predicate>)` in an existing
WHERE or `ON`, `WHERE (<predicate>)` when synthesized. `(p OR q)` and
`(a AND b)` are each exactly ONE depth-0 conjunct segment with fixed
precedence. The idempotence pre-check and the enforcement matcher are
symmetric: a segment matches the predicate's normalized token sequence
bare *or* wrapped in one pair of parentheses. Consequences:

- A hand-written parenthesized copy of the predicate, and a bare
  hand-written copy of a single-conjunct predicate, still match — no
  doubling.
- A bare hand-written copy of a predicate that itself carries a
  depth-0 `AND`/`OR` can never equal one AND-split segment; the weaver
  then weaves anyway and the query carries the conjunct twice —
  documented idempotence semantics (doubling is harmless, skipping
  leaks), pinned by test.
- Woven output bytes change for every woven query; oracle entries
  re-key by rendered SQL exactly as §7 describes, and the policy
  config still never enters the cache fingerprint.
