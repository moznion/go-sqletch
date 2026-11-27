# sqletch Design — 14: Cross-query policy weaving and enforcement

**Status: IMPLEMENTED (2026-08-02) — decisions D1–D6 settled with the
user, all per the recommendations recorded below; §10 phases 1–3
shipped (step 4 remains deferred).** This document expands the two
recorded notes
(spec §"Stability and Beyond v1.0"; `08-later-phases.md` §"Beyond
1.0") into a design. The settled outcomes are reflected in
`docs/spec.md` §"Cross-Query Policies". §11 records the mechanical
resolutions made during pre-implementation reconnaissance. §13
(2026-09-29) lifts D6's rejection for statement-level set-operation
branches.

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
  no policy-specific *code* — but only because they read a template
  that has already been through the arrow. "No awareness" is not "may
  skip the weave": a phase that re-scans the source and renders it
  itself sits *before* the arrow, whatever its position in the file,
  and emits the unscoped form of a query the compiler scopes.
  `explain --enumerate` and `explain --analyze` are the two commands
  that enumerate shapes outside `pipeline.Run`, so they reach the
  woven template through `cli.wovenTemplates` — the seam is theirs the
  way `cli.scanChecks` is the pipeline's and the LSP's. A defective
  policy set disables every policy (§D4), so they refuse rather than
  print what would then be unscoped SQL. (Fixed 2026-09-16; both had
  rendered the scanned template directly since P7, so `--enumerate`
  showed and `--analyze` planned SQL that is never executed. The
  regression tests are `TestExplainEnumerate_WeavesPolicies` and the
  index-search assertion in `TestSQLitePolicyWeaveCLI`.)

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

**Set-operation refinement (owner decision 2026-09-29, §13).** A
statement-level `UNION`/`INTERSECT`/`EXCEPT` branch is not a nested
scope: each leaf core is woven like a top-level statement. Subqueries
and CTE bodies — including those inside a branch, or in the set
operation's own `WITH`/`ORDER BY` — stay rejected.

**Nested-core refinement (owner decision 2026-11-20, §14).** On
PostgreSQL, a non-recursive CTE body and a FROM-position subquery are
eligible cores, scoped at their own WHERE like a branch. Sublinks,
recursive CTEs, and MySQL/SQLite nested positions stay rejected.

**Sublink refinement (owner decision 2026-11-21, §15).** On PostgreSQL, the body of an `EXISTS`/`IN`/`ANY`/
`ALL`/scalar sublink in the WHERE or HAVING of an eligible core of a
SELECT statement is an eligible core too.
Other sublink positions and kinds, DML, and construct bodies stay
rejected.

**Upsert refinement (owner decision 2026-08-21, audit-12 M10).** The
spec only excludes `INSERT … VALUES` as a policy target ("no rows are
filtered"), but an `INSERT … ON CONFLICT DO UPDATE` (PostgreSQL/SQLite;
MySQL `ON DUPLICATE KEY UPDATE`) *does* modify rows: on a cross-tenant
unique-key collision its `DO UPDATE` arm can overwrite another tenant's
row. That arm cannot carry a woven `WHERE` that scopes the conflict, so
weaving cannot make it safe. Fail-closed: an upsert whose target is a
designated table is **refused** (`SQLETCH125`) when the policy covers
the `update` kind (the `DO UPDATE` arm is an update-shaped
modification); `DO NOTHING` modifies nothing and stays a non-target. A
new `Tree.HasConflictUpdate()` capability reports the row-modifying
conflict arm across the three dialects; enforcement (§6.1) mirrors the
refusal as a `SQLETCH124` weaver-regression backstop.

**Data-modifying CTE refinement (owner decision 2026-10-21, security
finding F9).** Applicability used to key on the *outermost* statement
kind only, so a PostgreSQL data-modifying CTE escaped any policy whose
`applies_to` omitted that outer kind: under `applies_to: [update,
delete]`, `WITH d AS (DELETE FROM posts … RETURNING *) INSERT INTO
posts_archive SELECT * FROM d` was neither woven nor refused. A nested
DML modifies rows under its *own* kind, so a new
`Tree.NestedDML()` capability reports every data-modifying statement
nested inside the top-level one (kind, target, and whether an INSERT's
conflict arm is `DO UPDATE`); the PostgreSQL facade walks the whole
statement (WITH bodies at any depth, nested `ON CONFLICT DO UPDATE`
included), and the MySQL/SQLite facades report nothing (their parsers
admit only SELECT CTE bodies — pinned by tests). When a designated
table is the target of a nested UPDATE/DELETE, or of a nested upsert
under a policy covering `update` (the M10 rule), the policy is
**active regardless of the outer kind** and the statement is refused
(`SQLETCH125`; Enforce never credits it and reports `SQLETCH124` as the
backstop). A plain nested INSERT and `DO NOTHING` stay non-targets. The
non-maximal-rendering scan contributes its renderings' nested DML too,
so a wCTE confined to a non-first `@choose` alternative is refused
likewise. Weave and Enforce share `analyzeApplicability`.
Not changed (follow-up, not settled): hidden reads inside an
UPDATE/DELETE are still gated on the outer kind rather than on
`coversSelect()`.

*MySQL `REPLACE` (security finding F10).* `REPLACE` (any form —
`VALUES`, `SET`, `SELECT`) parses as an `InsertStmt` with `IsReplace`
and resolves a unique-key collision by DELETING the existing row —
whoever owns it — then inserting: with a request-supplied key, tenant
B replaces tenant A's row. It is a row-modifying conflict arm, so the
MySQL facade reports it through `HasConflictUpdate()`. Because the
modification is a deletion, it additionally implements the optional
`dialect.ConflictDeleter` capability (`HasConflictDelete()`), and the
refusal (weave `SQLETCH125`, Enforce `SQLETCH124`) fires when the
policy covers `update` **or** `delete` (`policy.conflictArm.governedBy`,
shared by Weave and Enforce). Each arm answers only to its own kind: a
delete-only policy still leaves `ON DUPLICATE KEY UPDATE` /
`DO UPDATE` alone (they never delete), and a select-only policy governs
neither. `INSERT IGNORE` touches no existing row and stays a
non-target. SQLite's `REPLACE` algorithm is handled the same way
(F17, next paragraph).

*SQLite `REPLACE` algorithm (security finding F17).* `INSERT OR
REPLACE`, `REPLACE INTO` (rqlite `InsertOrReplace` / `Replace`) and
`UPDATE OR REPLACE` (`UpdateOrReplace`) resolve a UNIQUE/PRIMARY KEY
collision by DELETING the existing row — whoever owns it: with a
request-supplied key, one tenant replaces another's row, and an
`UPDATE OR REPLACE` whose `WHERE` is tenant-scoped still deletes the
OTHER tenant's row its new values collide with. The SQLite facade
reports all three through `HasConflictUpdate()` and the optional
`dialect.ConflictDeleter` capability (`HasConflictDelete()`); the
refusal (weave `SQLETCH125`, Enforce `SQLETCH124`) fires when the
policy covers `update` **or** `delete` (`policy.conflictArm.governedBy`,
shared by Weave and Enforce) and the statement's TARGET is designated.
For `UPDATE OR REPLACE` this refusal precedes weaving, and Enforce does
not credit a present scoping conjunct: the conjunct scopes the selected
rows, never the deleted one. Each arm answers only to its own kind: a
delete-only policy still leaves `DO UPDATE` alone (it never deletes),
and a select-only policy governs neither. `OR IGNORE`/`ABORT`/`FAIL`/
`ROLLBACK` touch no existing row: such INSERTs stay non-targets, such
UPDATEs are woven normally. Not covered: a table-level
`… ON CONFLICT REPLACE` constraint clause in the DDL makes a PLAIN
INSERT/UPDATE replace; that is a schema property no statement shows.

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

Attachment (shared by every `-- @…` directive): a directive belongs to
the query whose body continues past it — the next real SQL token **or
construct** (`@if-present`, `@when`, `@choose`, `@order-by`, `@in`,
`@filter-tree`) of the current query claims it. A directive with only
trivia between it and the next `-- name:` header belongs to that
FOLLOWING query; one after the last query's body belongs to the last
query. An opt-out placed before a trailing construct therefore never
drifts onto the next query (it used to, leaving that query unscoped).

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
| `SQLETCH125` | A policy applies to this query but cannot be woven: the designated table is in a position sqletch cannot scope (subquery/CTE per D6, nullable outer-join side per D2, guarded-join relation per D5, a set-operation operand with no WHERE slot or a branch split the parser and the weaver do not agree on per §13), its bound name is not a bare identifier (§11.3), or the query already declares the policy's parameter name with a conflicting *type* (§11.4) or a conflicting *kind* — an optional `@if-present`/`@when` control parameter or a `@filter-tree` `@predicate` argument (§11.4). Opt out explicitly or restructure. |
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
- **Semantic leak fuzz** (added 2026-10 after the audit series, whose
  silent leaks #118–#124 all came from the hand-written clause scanners
  that Enforce shares): `FuzzPolicyWeaveNoLeak_{SQLite,Postgres,MySQL}`
  in `internal/cli` generate templates structure-aware from the fuzz
  bytes, run them through `scanChecks` + `Enforce` exactly as the
  pipeline does, and then EXECUTE every runtime shape of the woven query
  against the two-tenant dataset above with tid = 1. The oracle is the
  engine, never a re-reading of the text: a tenant-2 marker in a
  projected `secret` (or a changed tenant-2 row after rolled-back DML) is
  a leak, and Weave accepting what Enforce rejects is a failure too.
  Re-injecting the historic bugs (dropped OR flag, the audit-16 bare
  keyword column, a skipped ON conjunct, the removed wrong-join refusal)
  is caught within seconds; `TestPolicyWeaveNoLeak_*Live` keeps the
  generator from drifting into vacuity. SQLite runs in the CI fuzz smoke;
  the server dialects need devdb.

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
Statement-level set-operation branches are woven since 2026-09-29
(§13): a branch core is a top-level statement, not a nested scope.

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

**Case folding (security finding F7, owner decision 2026-10-21).**
The bound name is substituted *unquoted*, and PostgreSQL folds an
unquoted identifier to lower case, while its parser reports a quoted
name case-preserved. A quoted mixed-case alias `"A"` therefore became
`A.tenant_id` ≡ `a.tenant_id` — which binds to any *other* relation
aliased `a` (`FROM orders AS "A" JOIN users AS a …` scoped `users`
twice and leaked every tenant's `orders`). Decision: fail closed, keep
"no quoting facility". On a dialect that does not implement
`dialect.CaseInsensitiveIdents` (PostgreSQL), a `{}`-substituted bound
name that is not already in folded form (`name != lower(name)`, ASCII —
the bare-identifier rule already restricts it to ASCII) is
`SQLETCH125` at weave time, and enforcement never credits a conjunct
built from one (`SQLETCH124`, even for a correctly quoted hand-written
`"A".tenant_id` — the weaver can never produce it). A quoted name
already in folded form (`"a"`) is accepted. Placeholder-less predicates
are unaffected (they substitute nothing). One helper,
`policy.unquotedFoldChanges`, serves both passes. MySQL/SQLite resolve
identifiers case-insensitively and are out of this rule's scope.

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

### 11.7 "OR" means every operator looser than AND

The query's OWN clause is spliced into textually too: an existing
WHERE (or D2a `ON`) receives `(<predicate>) AND <clause>` unwrapped when
the clause has no depth-0 OR, and is wrapped as `(<predicate>) AND
(<clause>)` otherwise. "OR" there must mean every boolean operator that
binds looser than AND, or the appended conjunct binds below it. MySQL
has two besides `OR` (TiDB grammar: `pipes`/`or` < `xor` < `and`):

- `XOR` — `WHERE a XOR b` woven unwrapped is `((tenant) AND a) XOR b`,
  which returns every tenant's `b` rows (security finding F2/F3).
- `||` — logical OR under the default `sql_mode` (without
  `PIPES_AS_CONCAT`).

Both set the clause scanners' OR flag (`policy.looseOrOperator`, used by
the one WHERE reader `exprScan` and by `joinOn`), so they wrap, poison
idempotence matching, and are not credited by enforcement — exactly
like OR. `XOR` is recognized on every dialect, regardless of operand
position (a SQLite/PostgreSQL column named `xor` only costs a redundant
wrap), and stays clause CONTENT rather than a segment separator so the
wrap's opening parenthesis covers it even as the clause's first token.
`||` is OR-class only where the lexer profile implements
`dialect.PipesOr` (MySQL): on PostgreSQL and SQLite it is string
concatenation, binding tighter than AND, and treating it as OR would
needlessly re-wrap and defeat idempotence for ordinary `name || 'x'`
predicates. Under MySQL `PIPES_AS_CONCAT` the same treatment costs
only a redundant wrap. The MySQL lexer munches operator runs greedily
(`||-`), so any operator token containing `||` counts. The leak is
pinned by execution in `FuzzPolicyWeaveNoLeak_MySQL`'s seeds
(`mysqlLeakSeeds`) and the generator emits both operators on MySQL.

### 11.8 "Depth 0" excludes CASE … END (security finding F14)

A `CASE` expression is one operand, exactly like a parenthesized
expression. The clause readers (`exprScan` for statement and branch
WHERE clauses, `joinOn` for ON expressions) therefore track CASE
nesting at the clause's paren depth (`caseNest`): inside a CASE no
`AND` splits a segment, no `OR` sets the wrap flag, and no keyword ends
the clause. Before this, `WHERE CASE WHEN :mine THEN o.owner_id = :uid
AND o.tenant_id = :t AND o.active ELSE o.is_public END` yielded the
"top-level" segment `o.tenant_id = :t`: the weaver skipped the splice
and Enforce credited it, so `mine = false` read every tenant's public
rows.

- `CASE`/`WHEN`/`THEN`/`ELSE` are reserved on all three dialects. `END`
  is a legal bare column on MySQL and SQLite, so an `END` closes a CASE
  only in operator position (the closer always follows a complete
  value); in operand position it is a column. Dotted names (`o.end`)
  are never counted. `FROM`/`TO` (`IS DISTINCT FROM x`, `SIMILAR TO x`)
  are operand-introducing so a bare `end` there stays an operand.
- Fail-closed: an operator-position `END` with no open CASE, an arm
  keyword outside a CASE, a construct inside a CASE arm, or a CASE
  still open at clause end marks the clause unreadable
  (`clauseScan.lexOK = false`). The weaver refuses with `SQLETCH125`
  (WHERE and ON alike); Enforce credits nothing (`SQLETCH124`).
- An OR inside a CASE arm no longer triggers the wrap — correct, since
  the CASE binds as one operand; an OR after the CASE still does.
- Pinned by `internal/policy/case_arm_conjunct_test.go` and the
  `FuzzPolicyWeaveNoLeak_*` CASE-arm seed (the generator now emits a
  hand-written scoping conjunct between two ANDs of one CASE arm, and
  `end` is a quirk column on SQLite/MySQL).

### 11.9 Token comparison and identifier case (security finding F13)

Owner decision 2026-10-21: fail closed. "Normalized" used to mean `strings.ToLower`
on every unquoted identifier. That was unsound twice over: MySQL with
`lower_case_table_names=0` (the Linux default) treats table aliases as
case-sensitive, so a hand-written `a.tenant_id` scoping `users a` was
credited to `orders A` (weaver skipped, Enforce passed, every tenant's
orders shipped); and Unicode lowering folds U+212A KELVIN SIGN to `k`,
which no dialect does. The matcher (`conjTokMatch`, shared by the
weaver's idempotence check and Enforce) now:

- folds ASCII only, on every dialect;
- on PostgreSQL (which folds unquoted identifiers itself) compares
  unquoted identifiers ASCII-case-insensitively;
- on the `dialect.CaseInsensitiveIdents` dialects (MySQL, SQLite)
  compares every unquoted identifier EXACTLY, folding only a fixed set
  of MySQL-reserved keywords (`IS`, `NULL`, `AND`, `IN`, …) and never a
  word directly after `.` (a qualified-name part is an identifier
  position even when reserved). A case-variant-only match is therefore
  ABSENT and the weaver weaves anyway — doubling is harmless, skipping
  leaks. On SQLite this costs only a redundant conjunct;
- compares quoted identifiers and every other token verbatim.

The weaver's own output carries the exact bound name, so Enforce still
credits it, and an exact-case hand-written conjunct is still credited
on all three dialects. No identifier quoting is introduced (§11.3).

### 11.10 Designation and keywords use the engine's keys

Owner decision 2026-11-17 ("fail closed for both", doc 01 identifier
comparison keys), applied to policies 2026-11-19. Two Go Unicode folds
in the policy path did not match any engine:

- **Designation** (`Policy.designates`) used `strings.EqualFold`. On
  MySQL under `lower_case_table_names=1/2` the server lowercases table
  names through utf8mb3_general_ci, so `FROM İTEMS` reads table `items`
  (verified on MySQL 8.4), but `EqualFold("İTEMS", "items")` is false:
  the read went undesignated, unwoven, and Enforce passed it — a
  silent all-tenant read on such servers. In the other direction
  EqualFold joined names no engine joins (`ITEMſ`, the Kelvin sign).
  Designation now compares the profile's "may" relation key
  (`dialect.RelationFolds`): any table name SOME configuration could
  resolve to a designated table is designated. SQLite: ASCII-only;
  PostgreSQL: identity over the facade's folded names, so a quoted
  `"Orders"` is a different table from `orders` and is no longer
  designated (previously refused by §11.3's F7 rule). The per-name
  hidden-read counts and the non-maximal overread set key on "may"
  too. The woven-statement re-parse check (§13) compares table names
  exactly — the weaver never changes them.
- **Keywords** were recognized with `strings.ToUpper`, which maps `ı`→`I`
  and `ſ`→`S`; every engine matches keywords ASCII-only (`lımıt` is an
  identifier, even as an alias). The template scanner placed a missing
  WHERE in front of `lımıt` (unparseable woven SQL), `scanSetOp` split
  a branch at a `unıon` column, and `caſe` opened a CASE nest (a false
  SQLETCH125). All keyword recognition now uses `dialect.ASCIIUpper`.
  No silent leak was found through this path: the operand-position and
  CASE tracking fail closed on every misread probed.

- **Bound-name folding** (2026-11-21): `unquotedFoldChanges` keyed on
  `strings.ToLower`, but PostgreSQL downcases unquoted identifiers over
  ASCII only (PG 16: unquoted `É` names the alias `"É"`). It now uses
  `dialect.ASCIILower`. Enforce also refuses (SQLETCH124) a non-bare
  bound name under a `{}` predicate, as the weaver already did
  (SQLETCH125): before, `"É"` on PostgreSQL was refused only by the
  ToLower accident, with the false rationale "folds to `é`", and a
  hand-written `é.tenant_id = …` was CREDITED on MySQL/SQLite while the
  weaver refused the same occurrence — the passes must agree.

Pinned by `designate_fold_test.go` and `bound_name_fold_test.go`.

### 11.11 A join-group alias hides the bound name (PostgreSQL)

Found 2026-11-27 reviewing the §15 implementation. PostgreSQL's alias on a parenthesized
join group, `(orders o JOIN users u ON …) AS j`, hides the member names:
outside the group only `j` is referenceable. The core's WHERE (and any
ON outside the group) is outside it, so the spliced `o.tenant_id = $1`
does not reach the inner `o`:

- at the top level and in a CTE body it fails (`invalid reference to
  FROM-clause entry for table "o"`) — loud, by accident;
- in a LATERAL subquery (§14) or a correlated sublink (§15) it
  resolves to a same-named relation of the ENCLOSING query, the inner
  orders stay unscoped, and Enforce credited the conjunct — a silent
  leak (PG 17: tenant 2's rows decided the tenant-1 answer). The rule
  landed before §15, so sublink cores were covered from the start.

Rule: the PostgreSQL facade marks every relation inside an aliased
join group (`RelRef.JoinAliasHidden`; a USING alias, `JOIN … USING (k)
AS x`, hides nothing and is not marked). A designated occurrence so
marked is refused under a `{}` predicate — SQLETCH125 in Weave,
SQLETCH124 (never credited) in Enforce, through the shared
`joinAliasWhy` — at every level and whatever slot the conjunct would
take, even an ON inside the group where the name is visible: one rule
for both passes, failing closed. The top-level case now gets a
diagnostic instead of SQL that fails at run time. A predicate without
`{}` is unaffected (top level only, §14.1). MySQL and SQLite cannot
alias a join group. The compliant rewrite drops the group's alias.

Pinned by `weave_joinalias_test.go`, `TestFrontend_JoinAliasHidden`,
and the devdb counterexample
`TestPolicyWeave_JoinAliasHidden_Counterexample`.

## 12. Mandatory acknowledgment (`require_annotation`)

**Status: settled 2026-09-16 (D7).** §6.2 made *disabling* a policy
explicit; enabling it stayed implicit. This section makes both sides
explicit, under a per-policy configuration key.

### 12.1 The asymmetry being closed

Today a query that touches a designated table is scoped by writing
nothing at all, and exempted by writing `-- @policy-optout`. That is
the right default — §1.1's inversion depends on it — but it means the
template file is silent about the most security-relevant fact about
the query. A reviewer reading `audit.sql` cannot tell a scoped query
from an unscoped one without holding `sqletch.yaml` in the other hand
and re-deriving the weaver's applicability rule. §1.6 offers `explain`
as the audit surface; this section brings the same information into
the artifact review actually looks at, the diff.

```yaml
policies:
  - name: tenant_scope
    tables: [orders, audit_logs]
    predicate: "{}.tenant_id = :tenant_id"
    require_annotation: true
```

With that key set, every query the policy applies to must carry one of
the two annotations:

```sql
-- name: CountAuditLogs :one
-- @policy-apply: tenant_scope
SELECT count(*) AS total FROM audit_logs;

-- name: AllAuditActions :many
-- @policy-optout: tenant_scope (ops dashboard; aggregates across tenants)
SELECT a.action, count(*) FROM audit_logs AS a GROUP BY a.action;
```

A query with neither is `SQLETCH127`.

### 12.2 The annotation is an acknowledgment, never a switch

`-- @policy-apply` changes **nothing** about what is woven. Weaving is
decided by the policy's table set and statement kinds exactly as
before, whether or not the annotation is present and whether or not
`require_annotation` is set. The annotation's only effect is to
satisfy the requirement.

This is load-bearing and deliberately narrow. Making the annotation
*enable* weaving would invert §1.1: a query added next month would be
unscoped until someone remembered to annotate it, which is precisely
the failure mode this feature exists to remove. The requirement is
therefore expressed as "absence is an error", not as "presence is
required for scoping" — the query is scoped either way, and the
compiler refuses to proceed until the author has said so out loud.

Consequence worth stating: `require_annotation` buys review
legibility, not a soundness property. The soundness argument of §6 is
unchanged and does not depend on this key.

### 12.3 Scope of the requirement

The requirement fires on exactly the predicate that already decides
whether an opt-out is legitimate — `analyzeApplicability(...).active`,
plus the non-maximal overread set (`designatedOverread`). That is not
a convenience: it is what makes the two annotations exhaustive. If the
requirement used a wider predicate than SQLETCH126 uses, a query could
be obliged to annotate and simultaneously forbidden from doing so.

Per policy, not per query: three policies applying to one query need
three annotations, the same granularity opt-out already has. A policy
without `require_annotation` never contributes an obligation.

### 12.4 Symmetry with SQLETCH126

`-- @policy-apply` is subject to the same sanity rules as
`-- @policy-optout`, and shares its code:

- naming an unknown policy is `SQLETCH126`;
- naming a policy that does not apply to the query is `SQLETCH126`;
- carrying both `@policy-apply` and `@policy-optout` for one policy is
  `SQLETCH126` — the query cannot be both acknowledged and exempt, and
  silently preferring one would make the annotation pair unreviewable.

The reason is the one §6.2 gives for opt-outs: renaming a policy must
never silently disarm the annotations that mention it. An
acknowledgment left behind by a rename is exactly as misleading as an
opt-out left behind by one — it tells a reviewer the query is scoped
by a policy that no longer exists.

Like `@policy-optout`, the annotation admits an optional trailing
`(reason)`. It is optional here because an acknowledgment carries no
exemption: "this query is scoped" needs no justification, while "this
query is not" does.

### 12.5 Where the check runs

In the weaver (`policy.Weave`, via `cli.scanChecks`), not in
`policy.Enforce`.

The obligation is a function of the policy set and the statement's
relation list, both of which the weaver already has in hand at the
point where it decides applicability — `apply()` computes
`a.active` and immediately afterwards consults `optOutFor`. No
catalog is involved, so placing it here means the LSP reports a
missing annotation on a cold cache, like every other lexical and
structural diagnostic, instead of only when the committed cache holds
every rendering of the query.

The SQLETCH126 extensions of §12.4 stay in `Enforce`, beside the
opt-out sanity checks they generalize: those exist to survive a weaver
regression, and re-deriving them independently is the whole point.

**Known interaction from that split.** A stale `-- @policy-apply` left
behind by a rename, on a policy that also sets `require_annotation`,
surfaces as SQLETCH127 rather than SQLETCH126: the weave-time error
stops the pipeline before `Enforce` runs. The author is not left
stuck — 127's hint spells the current policy name — but the fix takes
two rounds, the second reporting the dead annotation. Accepted rather
than fixed: validating annotation names in the weaver too would put
the policy-name table in two passes that must then agree, which is the
coupling `Enforce` exists to avoid.

### 12.6 The config-dependence carve-out

CLAUDE.md and §4.2 record a rule this section breaks: *template
validity must not depend on configuration*. R6's anchor check runs on
the unwoven template precisely so that adding a policy cannot change
whether a template compiles.

The rule is kept for its reason rather than its letter. R6's hazard is
a config change **silently making an invalid template valid** — the
author's anchor obligation quietly discharged by a conjunct they did
not write, so a template that reads as broken compiles anyway.
`require_annotation` runs the other way: it makes a valid template
invalid, loudly, with a diagnostic naming the annotation to add and
the policy that demands it. That is the same direction as
`overrides` naming a column the query does not project (SQLETCH312) or
a `queries:` pattern matching nothing (SQLETCH316) — configuration
stating a requirement the sources must then meet.

Stated as the narrower invariant that survives: **no configuration
may make a template compile that would not compile without it.**
Weaving satisfies it because R6 runs unwoven; `require_annotation`
satisfies it because it only ever subtracts.

### 12.7 Rollout

The key is a plain `bool` with no warning tier (owner decision
2026-09-16). Turning it on fails every touching query at once, which
is the intended shape of the change: the annotations are the work, and
a warning tier would leave a codebase indefinitely half-annotated
while reading as compliant. `sqletch explain` already lists, per
query, which policies apply — that is the worklist, and it is
available before the key is set.

### 12.8 Cache impact

The annotation is a comment and stays in the skeleton verbatim, like
every other directive (§11.1), so adding one to a query changes its
rendered SQL and re-keys that query's oracle entries — the ordinary
§7 behavior for any template edit. The config key itself never enters
the fingerprint, consistent with the rest of `policies:`.

## 13. Set-operation branch weaving

**Status: IMPLEMENTED (2026-09-29, owner decision "A" of the D6
follow-up).** D6 rejected every designated read outside the statement's
own FROM, set-operation branches included. This section lifts that
rejection for **statement-level set operations only**; subqueries,
CTE bodies, and `INSERT … SELECT` stay `SQLETCH125` exactly as D6
settled.

### 13.1 Semantics — every branch is a top-level statement

A policy means "every read of a designated table sees only in-scope
rows". A leaf `SELECT` core of `UNION`/`INTERSECT`/`EXCEPT` is an
ordinary statement with its own FROM and WHERE slot, so it is scoped
exactly as §4 scopes a top-level statement: a WHERE conjunct per
designated occurrence (D1), an `ON` conjunct for a null-extended
occurrence (D2a), idempotence and OR-wrapping per branch WHERE, and a
`{}`-less predicate as one conjunct per *branch* that reads a
designated table. Scoping the operands, rather than the set
operation's result, is the only placement that respects the meaning:
an `EXCEPT`'s right operand is read — the rows it removes must be the
tenant's — and filtering the result could not scope it. `INTERSECT`
and `EXCEPT` therefore need no special case.

Set-level clauses (`WITH`, `ORDER BY`, `LIMIT` of the whole operation)
belong to no branch. A designated read there is a subquery or CTE read
(D6, still rejected); the Relations/DeepTables count comparison already
catches it because the branch sub-facades exclude those clauses. An
operand without a WHERE slot (`TABLE t`, `VALUES`) that reads a
designated table is rejected.

### 13.2 Two independent views of the branch structure

Placement needs template offsets, and the parser alone cannot supply
them (PostgreSQL SelectStmt nodes carry no location; TiDB relation
nodes none at all). So the branch structure is derived twice:

1. **AST** — `dialect.Tree.SetOpBranches()` (new facade capability,
   compile-visible across all three frontends): the leaf cores in
   document order, nested/parenthesized operands flattened, each a
   sub-facade whose `Relations()` are that core's own relations with
   locations in the original SQL. PostgreSQL walks `larg`/`rarg`;
   SQLite follows the `Compound` chain and strips the set-level
   clauses rqlite hangs on the first core; MySQL flattens
   `SetOprSelectList` and locates every branch's relations in one
   lexical pass that treats a paren in set-operation operand position
   as transparent (a quantified `> ALL (SELECT …)` is not in operand
   position).
2. **Lexical** — `policy.scanSetOp`: a small recursive descent over
   the template's token stream (`query := [WITH …] operand {setop
   [ALL|DISTINCT] operand} tail`, `operand := '(' query ')' | core`)
   recording, per core, its lead keyword, its WHERE clause (via the
   same `exprScan` state machine `whereClause` uses — one WHERE reader
   for statements and branches), and its insertion points (after
   `WHERE`; before the first post-WHERE clause keyword; at the core's
   end). It also maps every original token offset to its core.
   Set operators are reserved in every dialect, so only a dotted
   `t.union` needs excluding; anything off the modeled shape is
   `ok=false`.

The weaver requires the two to agree before it inserts anything: the
same number of branches, and every designated occurrence's name token
lying in the lexical core of the index the parser assigned it. After
splicing it re-parses the woven maximal rendering and requires the
same branches with the same relations. Any disagreement is
`SQLETCH125` — a mis-split can be loud, never a conjunct in the wrong
branch.

### 13.3 Enforcement

`Enforce` credits a designated occurrence only with a conjunct in its
**own** branch's WHERE (or its own join's ON), found through the
lexical split and attributed through the parser's branch index — the
same two-view agreement, re-derived from the woven template. A
conjunct in one branch never vouches for another.

### 13.4 The pre-§13 SQLite leak this closes

SQLite's whole-statement `Relations()` of a compound SELECT is the
FIRST core's. The weaver therefore treated a designated table in the
first branch as a plain top-level occurrence and spliced its conjunct
at the scanner's `WhereKwEnd` — the statement's first WHERE keyword,
which belongs to a LATER branch when the first has none. With both
branches aliasing `o` and the later table carrying the scoping column,
the result was valid SQL scoping the wrong table, and `Enforce`
(reading the same first WHERE) passed it: a silent all-tenant read,
confirmed against the real engine (tenant 1 received tenant 2's rows).
PostgreSQL and MySQL were unaffected (their whole-statement
`Relations()` is empty for a set operation, so D6 refused). The
branch path replaces the whole-statement path for every set operation;
`TestWeave_SQLite_CompoundBranchLeakRegression` and the devdb
`TestSQLitePolicySetOpWeaving`/`…LeakedShapeRejected` pin it.

### 13.5 Tests

Facade conformance (`dialecttest.CheckSetOpBranches`, all three
dialects); `scanSetOp` unit tests; golden woven renderings incl.
parenthesized operands with their own LIMIT, nested operands, per-branch
OR-wrap and idempotence, ON-weave inside a branch, `{}`-less
predicates; the refusals (subquery in a branch, statement-level CTE,
set-level ORDER BY subquery, `TABLE` operand, guarded join, USING);
per-branch enforcement incl. the wrong-branch alias shape; the offline
pipeline (R1 on the woven statement, PostgreSQL and SQLite); and
row-level devdb proofs on real PostgreSQL (UNION ALL, parenthesized
first/last-row operands, an EXCEPT whose tenant-1 answer is empty) and
real SQLite (the §13.4 shape, every-branch, EXCEPT).

### 13.6 MySQL

MySQL maps a top-level set operation to `StmtOther`, so R1 rejects it
(`SQLETCH103`) before weaving matters, and the nullability soundness
suite relies on that. `SetOpBranches` is implemented and tested for
MySQL, but admitting MySQL set operations is a separate decision
(nullability attribution, the native oracle), pinned by
`TestFrontend_SetOperationIsNotADMLKind`.

## 14. Nested-core weaving (CTE bodies and FROM subqueries)

**Status: IMPLEMENTED (2026-11-20, owner decision on the D6
follow-up: scope "CTE and FROM subqueries", PostgreSQL only).** §13
lifted D6 for statement-level set-operation branches. This section
lifts it for two more positions, on PostgreSQL:

- the body of a **non-recursive** `WITH` list entry whose body is a
  query (`SELECT`, a set operation, a parenthesized query), and
- a **FROM-position subquery** (`FROM (SELECT …) AS d`, `JOIN (…) d
  ON …`, `LATERAL (…)`),

recursively: a CTE body or derived table inside either of those is
itself eligible, and a set operation inside one is split into its
leaf cores exactly as §13 does. Everything else D6 rejected stays
`SQLETCH125`:

- subqueries in the WHERE/HAVING/SELECT/ORDER BY/… of any core
  (`IN (SELECT …)`, `EXISTS (…)`, scalar subqueries) — a sublink is
  not walked, so a designated table there is still a hidden read;
- every body of a `WITH RECURSIVE` list (the flag covers the whole
  list in PostgreSQL; a recursive term reads its own output, and
  scoping it is left to a later decision);
- data-modifying CTE bodies (F9, unchanged);
- `INSERT … SELECT` bodies (unchanged);
- MySQL and SQLite: their facades report no nested cores, so a nested
  designated read stays hidden and refused (the capability exists in
  all three so that enabling them is a compile-visible change).

### 14.1 Semantics — every eligible core is a statement

The §13.1 argument carries over unchanged: a policy means "every read
of a designated table sees only in-scope rows", and a leaf `SELECT`
core with its own FROM and WHERE slot is scoped where it reads. A CTE
body or derived table is such a core. Scoping it at its own WHERE is
the only placement that respects the meaning: the outer statement may
aggregate, join, or anti-join the nested rows, and only a filter at
the read site makes each of those operate on in-scope rows. This is
also what PostgreSQL's row-level security does — it scopes every
reference to the table, whatever its depth.

An occurrence on the null-extended side of an outer join *inside* a
nested core is woven into that join's ON clause (D2a), located by the
same forward scan from the relation's template offset; a derived
table that is itself on a null-extended side of the OUTER statement
needs nothing extra — the conjunct inside it removes out-of-scope rows
before the outer join null-extends the group, exactly the
inner-join-group equivalence §D2 already relies on.

A `{}`-less predicate is refused in a nested core (`SQLETCH125`): it
references no relation, and inside a correlated (LATERAL) derived
table or a CTE body an unqualified column could resolve to an outer
or a different relation than the designated one. The top-level and
set-operation-branch behaviour of such predicates is unchanged.

A CTE whose name shadows a designated table keeps the conservative
treatment of §11.1: references to the CTE name are counted as touching
the designated table and are woven like one. That never leaks — it
filters the CTE's rows by the scoping column, or fails verification
when the CTE has no such column — so it needs no new rule.

### 14.2 The AST view — `Tree.NestedCores`

`dialect.Tree` gains `NestedCores() []Tree`: every eligible leaf core
nested under the statement, as located sub-facades like
`SetOpBranches` returns, in a deterministic walk order (WITH list
before the operand cores, set-operation `larg` before `rarg`, a core's
FROM items in order). The statement's own cores (the statement, or its
`SetOpBranches`) are NOT listed. The PostgreSQL facade walks
`SelectStmt.WithClause` (skipped entirely when `Recursive`) and
`RangeSubselect` FROM items (through `JoinExpr`/`RangeTableSample`);
it never enters a `SubLink`. MySQL and SQLite return nil.

`topRelations` appends every nested core's own relations after the
top-level cores', tagging each with its core index. The
`Relations`/`DeepTables` count comparison (§11.1) therefore turns a
designated read in an eligible nested position into an ordinary
occurrence, and leaves one in any other position hidden.

### 14.3 The lexical view, and how the two are matched

`scanSetOp` (now the core scan for every multi-core statement — a set
operation, or any statement with a nested core) additionally parses,
after the flat pass, every parenthesized region that starts with
`SELECT` or `WITH` — a WITH body, a FROM subquery, a LATERAL subquery,
or a sublink — as a query of its own, recursively. The flat pass is
unchanged, so a core's WHERE still sees every token of its
parenthesized subexpressions; the nested pass only re-attributes the
inner tokens (`branchOf`) to the innermost core. A region that does
not parse as a query keeps its tokens with the enclosing core.

The lexical scan finds cores the AST does not list (sublinks), so the
two lists cannot be matched by index as §13.2 does. They are matched
by **relation location** instead:

1. For every AST core, the set of lexical cores holding its located
   relations' name tokens. The core maps to a lexical core when that
   set has exactly one member.
2. A designated occurrence is woven (or, in Enforce, credited) only
   when its AST core maps to a lexical `SELECT` core, and no OTHER AST
   core has a relation in that same lexical core.

Rule 2 is what makes a mis-split loud: if the lexical scan failed to
open a nested core, its relations land in the enclosing lexical core,
two AST cores collide, and the occurrence is `SQLETCH125` (Enforce:
`SQLETCH124`) — never a conjunct in the enclosing core's WHERE. The
woven statement is re-parsed and must yield the same cores with the
same relations (`verifySetOpWeave`, now run for every multi-core
statement).

Top-level statements without nested cores keep the pre-§14 path
(whole-statement WHERE, or §13's index-matched branches is subsumed
by the location match), so their woven output is byte-identical.

### 14.4 Tests

Golden woven renderings (CTE body, set operation inside a CTE body,
FROM subquery, LATERAL subquery, nested-in-nested, ON-weave inside a
derived table, idempotence inside a CTE, untouched queries); the
refusals (sublink, recursive CTE, `{}`-less predicate in a nested
core, guarded join inside a CTE, MySQL/SQLite CTE); Enforce per core
(a conjunct in the outer WHERE never vouches for a CTE body, and vice
versa); facade conformance for `NestedCores`; the row-level devdb
proof on real PostgreSQL and the Postgres leak fuzzer's nested seeds.

## 15. Sublink weaving, phase 1 (WHERE/HAVING subqueries)

**Status: IMPLEMENTED (2026-11-21, owner decisions §15.9).** §14 left every sublink `SQLETCH125`. This section lifts
that for the most common sublink positions, on PostgreSQL only, and
only in SELECT statements. Later phases are listed in §15.8 so the
boundary is explicit.

### 15.1 Scope

Phase 1 makes a sublink's body an **eligible core** (§14) when:

1. **The sublink kind** (pg_query `SubLink.SubLinkType`, raw parse
   tree) is one of:
   - `EXISTS_SUBLINK` — `EXISTS (…)`, and `NOT EXISTS (…)` (a
     `BoolExpr NOT` around it);
   - `ANY_SUBLINK` — `x IN (…)`, `x NOT IN (…)`, and `x op ANY (…)` /
     `x op SOME (…)` for any operator;
   - `ALL_SUBLINK` — `x op ALL (…)`;
   - `EXPR_SUBLINK` — a scalar subquery `(SELECT …)` used as a value.
     A row comparison `(a, b) = (SELECT …)` is an `EXPR_SUBLINK` in the
     raw parse tree as well (`ROWCOMPARE_SUBLINK` only appears after
     parse analysis), so it is scoped too. Scoping is correct for it in
     the same way as for a scalar subquery: it compares against the
     in-scope row. This is a mechanical refinement made during
     implementation.
2. **The sublink lies in the WHERE or HAVING clause of an eligible
   core.** An eligible core is the statement itself, a set-operation
   branch (§13), a CTE body or FROM subquery (§14), or another
   eligible sublink body (this section). The rule applies recursively.
   Anywhere inside the clause's expression counts, at any depth:
   under `AND`/`OR`/`NOT`, inside `CASE`, function arguments, or the
   left operand of another sublink. The rule is about the clause, not
   the expression's shape.
3. **The top-level statement is a SELECT.** §14 does not walk a DML
   statement's WITH/FROM, and phase 1 does not walk its WHERE either.
4. **The sublink's tokens are template skeleton text**, outside every
   `@if-present`/`@when`/`@choose`/filter-tree body (§15.5).

The body is then woven exactly like a §14 nested core. Each leaf core
gets a WHERE conjunct per designated occurrence, or an ON conjunct
for a null-extended one (D2a). Idempotence and OR-wrapping work per
core. A set operation inside the body is split into its operands
(§13), and CTE bodies and FROM subqueries inside it are eligible
(§14).

Everything else stays `SQLETCH125`. That is the same hidden-read
refusal as today: the `Relations`/`DeepTables` count comparison of
§14.2 reports any designated read that `NestedCores` does not list.
The positions that stay refused are:

- **Sublinks in other clauses:** the SELECT list, `ORDER BY`,
  `GROUP BY`, a join's `ON`, `LIMIT`/`OFFSET`, window definitions,
  `VALUES` rows, and FROM-position function arguments;
- **Other sublink kinds:** `ARRAY(…)` and `MULTIEXPR`
  (`UPDATE … SET (a, b) = (SELECT …)`, which is DML anyway);
- **Any sublink in an UPDATE/DELETE/INSERT statement**;
- **A sublink inside a template construct body** (§15.5);
- **Shapes already refused:** a `WITH RECURSIVE` list inside a
  sublink, a `TABLE t`/`VALUES` operand reading a designated table,
  and a `{}`-less predicate in a sublink core (the §14.1 rule
  carries over);
- **MySQL and SQLite**, which keep returning no nested cores.

### 15.2 Semantics — scoping the read site

The §13.1/§14.1 argument carries over. A policy means "every read of a
designated table sees only in-scope rows". A sublink body is a read
with its own FROM and WHERE slot, so it is scoped where it reads. This
is the placement PostgreSQL row-level security uses. The effect per
kind:

| Form | Woven meaning |
|---|---|
| `EXISTS (…)` | true iff an **in-scope** row satisfies the body |
| `NOT EXISTS (…)` | true iff no **in-scope** row satisfies the body |
| `x IN (…)` / `x op ANY (…)` | true iff `x op v` for some **in-scope** row's `v` (three-valued) |
| `x NOT IN (…)` / `x op ALL (…)` | true iff `x op v` for every **in-scope** row's `v`; vacuously true when there are none (three-valued) |
| scalar `(…)` | the value computed over **in-scope** rows (NULL when none) |

These semantics are the documented behaviour (owner decision Q3). Two
consequences must be stated in the manual (12):

- **Anti-joins can return more outer rows.** A `NOT EXISTS`, `NOT IN`
  or `op ALL` over a designated table sees fewer rows once it is
  scoped, so more rows of the enclosing query qualify. Those rows
  belong to a non-designated table, or to a designated one that is
  scoped in its own core. Out-of-scope rows used to decide the answer,
  which was the leak; now they do not.
- **NULLs come only from in-scope rows.** A NULL in an out-of-scope
  row no longer turns `NOT IN` / `op ALL` into UNKNOWN. RLS gives the
  same answer.

**Correlation and name resolution.** Sublinks are usually correlated.
`{}` is substituted with the bound name of the sublink's own
relation. PostgreSQL resolves a qualified reference to the innermost
scope that has that range-table name, so the woven `o.tenant_id`
binds to the sublink's `o` even when the outer query also has an `o`
or an unaliased table of the same name. That is the binding we want,
and the reason a `{}` predicate is safe here. A `{}`-less predicate
has no such anchor: an unqualified column could resolve outward. It
stays refused, as in §14.1.

### 15.3 The AST view — `NestedCores` enters eligible SubLinks

The PostgreSQL `NestedCores` walk gains one step. For each leaf core
it already visits, after the FROM items, it walks the core's
`WhereClause` and `HavingClause` expression trees:

- On a `SubLink` whose type is `EXISTS`, `ANY`, `ALL` or `EXPR`, it
  recurses into `query(SubLink.Subselect, emit=true)`. A set operation
  there is flattened, and the body's own WITH, FROM subqueries and
  WHERE/HAVING sublinks follow recursively.
- On a `SubLink` of any other type, it does not enter the body. Its
  relations stay hidden and are refused.
- In both cases it walks the sublink's `Testexpr`, because that is an
  outer-scope expression of the same clause
  (`(SELECT …) IN (SELECT …)`). A `Subselect` is never reached through
  an expression walk.
- It never walks the target list, sort, group, window or limit
  expressions.

The walk order extends §14.2's rule deterministically: WITH list,
then operand cores, then FROM subqueries, then WHERE sublinks before
HAVING sublinks, each in document order. A sublink core is a located
sub-facade like any other, so `topRelations`, `coreTrees` and
`verifySetOpWeave` need no change. A facade conformance test pins the
eligible/ineligible split (§15.7).

### 15.4 The lexical view — already in place

`scanSetOp` already opens every parenthesized region that starts with
`SELECT`/`WITH`/`TABLE`/`VALUES`, sublinks included (§14.3), and gives
each region its own insertion points: after `WHERE`, before the first
tail keyword, and at the core's end, which for a sublink is just
before its closing `)`. Matching by relation location, the
"one parser core per lexical core" rule, and the post-weave check
that the parser finds woven conjuncts in exactly the cores that
needed one (`verifySetOpWeave`) all apply unchanged. Before §15 these
lexical sublink cores existed only so that collisions were detected.
Now they are also weaving targets.

The enclosing core's flat WHERE scan still sees the sublink's tokens
at depth > 0, so they never count as top-level conjuncts of the
enclosing core. That keeps idempotence and Enforce credit within one
core: a correlated `o.tenant_id = :tenant_id` written *inside* a
sublink never vouches for the outer `o` (`weave_nested_test` already
pins this shape), and an outer conjunct never vouches for a sublink
read.

### 15.5 Template constructs — refused in phase 1

`setOpElems` turns every non-skeleton item into one opaque element. A
sublink inside an `@if-present` body is therefore invisible to the
lexical view. Its relation's token is in no lexical core, `locate`
fails, and the occurrence is refused. That is fail-closed, and phase 1
keeps it (owner decision Q2: construct bodies are phase 2).
`checkSetOpOccurrences` refuses a nested occurrence whose relation
lies inside any construct's emission with a message that names the
construct body, rather than with the guarded-join or "cannot be
located" diagnostics. The reverse nesting, a construct inside a
subquery, is already an R1 scan error (`SQLETCH006`). The
limitation matters in practice: an optional filter `@if-present(tag)
AND o.id IN (SELECT order_id FROM order_tags WHERE tag = :tag)
@endif` over a designated `order_tags` stays `SQLETCH125`. The author
can opt out and scope the read by hand.

Lifting this means splicing into a construct's `Body`. The woven
conjunct must inherit the construct's guard set: it exists exactly
when the read exists, which differs from D5, where a conjunct is
always unconditional. Enforce would then credit it per shape.

### 15.6 Examples (designated `orders`, `order_items`, `users`; `{}.tenant_id = :tenant_id`)

```sql
-- IN
SELECT u.id FROM users u WHERE u.id IN (SELECT o.user_id FROM orders o)
-- woven
SELECT u.id FROM users u WHERE (u.tenant_id = $1) AND u.id IN (SELECT o.user_id FROM orders o WHERE (o.tenant_id = $1))

-- correlated NOT EXISTS, existing inner WHERE
SELECT o.id FROM orders o
WHERE NOT EXISTS (SELECT 1 FROM order_items i WHERE i.order_id = o.id)
-- woven
SELECT o.id FROM orders o
WHERE (o.tenant_id = $1) AND NOT EXISTS (SELECT 1 FROM order_items i WHERE (i.tenant_id = $1) AND i.order_id = o.id)

-- op ALL
SELECT a.id FROM acc a WHERE a.limit_amount >= ALL (SELECT total FROM orders)
-- woven
SELECT a.id FROM acc a WHERE a.limit_amount >= ALL (SELECT total FROM orders WHERE (orders.tenant_id = $1))

-- scalar subquery in WHERE, inner tail clauses
SELECT o.id FROM orders o WHERE o.total > (SELECT total FROM orders ORDER BY created_at DESC LIMIT 1)
-- woven
SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND o.total > (SELECT total FROM orders WHERE (orders.tenant_id = $1) ORDER BY created_at DESC LIMIT 1)

-- HAVING sublink (HAVING itself is still no policy slot; the sublink body's WHERE is)
SELECT a.kind, count(*) FROM acc a GROUP BY a.kind
HAVING count(*) > (SELECT count(*) FROM orders)
-- woven
SELECT a.kind, count(*) FROM acc a GROUP BY a.kind
HAVING count(*) > (SELECT count(*) FROM orders WHERE (orders.tenant_id = $1))

-- set operation inside IN, inside a CTE body
WITH x AS (SELECT id FROM acc WHERE id IN (SELECT user_id FROM orders UNION SELECT id FROM users))
SELECT id FROM x
-- woven
WITH x AS (SELECT id FROM acc WHERE id IN (SELECT user_id FROM orders WHERE (orders.tenant_id = $1) UNION SELECT id FROM users WHERE (users.tenant_id = $1)))
SELECT id FROM x

-- OR around a sublink: the outer conjunct wraps, the inner one is independent
SELECT o.id FROM orders o WHERE o.flag OR o.id IN (SELECT order_id FROM order_items)
-- woven
SELECT o.id FROM orders o WHERE (o.tenant_id = $1) AND (o.flag OR o.id IN (SELECT order_id FROM order_items WHERE (order_items.tenant_id = $1)))
```

Still refused (`SQLETCH125`):

```sql
SELECT u.id, (SELECT count(*) FROM orders o WHERE o.user_id = u.id) FROM users u   -- SELECT-list sublink (phase 2)
SELECT a.id FROM acc a WHERE a.ids @> ARRAY(SELECT id FROM orders)                 -- ARRAY sublink (phase 2)
SELECT a.id FROM acc a JOIN users u ON u.id IN (SELECT user_id FROM orders)        -- ON sublink (phase 2)
DELETE FROM acc WHERE id IN (SELECT user_id FROM orders)                           -- DML (phase 3)
SELECT u.id FROM users u WHERE EXISTS (SELECT 1 FROM orders WHERE true)            -- with a {}-less predicate configured
SELECT u.id FROM users u WHERE u.id > 0
@if-present(tag) AND u.id IN (SELECT user_id FROM orders WHERE tag = :tag) @endif  -- construct body (phase 2)
```

### 15.7 Tests

Implemented as: `TestFrontend_NestedCores` (facade);
`TestWeave_Sublink_Golden`, `TestWeave_Sublink_Unweavable`,
`TestEnforce_Sublink_PerCore` and `TestWeave_Sublink_SQLiteStillRefused`
(`internal/policy/weave_sublink_test.go`); and, in the leak fuzzer,
`leakGen.sublinked` (raw[0] 0xEA..0xF1), the `tenant2Influence` oracle,
`sublinkLeakSeeds`, and the devdb `TestPolicyWeaveNoLeak_SublinkSeedsAreLive`.
The oracle has two perturbations: deleting tenant 2's rows, and moving
them to values tenant 1 never holds. `leakData` is symmetric, so
deletion alone cannot move IN/EXISTS membership.

- **Facade:** `NestedCores` lists the bodies of EXISTS, NOT EXISTS,
  IN, NOT IN, `op ANY`/`SOME`, `op ALL` and scalar sublinks in WHERE
  and HAVING, including nested sublinks, sublinks in a CTE body or
  FROM subquery, a set operation inside IN, and sublinks inside
  `CASE` or function arguments. It lists nothing for
  SELECT-list/ORDER BY/ON/LIMIT sublinks, `ARRAY` sublinks, or DML. The walk order is pinned.
- **Golden weaves:** the §15.6 shapes, plus idempotence inside a
  sublink, an ON-weave inside a sublink's outer join, a parenthesized
  body `IN ((SELECT …))`, and untouched shapes (a sublink over a
  non-designated table).
- **Refusals:** every §15.1 exclusion, plus a sublink inside each
  construct kind.
- **Enforce:** credit stays per core in both directions, including
  the correlated outer-alias conjunct written inside a sublink.
- **Leak oracle (new — the marker oracle is not enough).** The
  existing fuzzer counts a leak when a tenant-2 marker appears in a
  tenant-1 result. A WHERE sublink leaks through *influence*: tenant-2
  rows change which tenant-1 rows qualify, and no marker is ever
  projected. Phase 1 adds a differential oracle. Inside a rolled-back
  transaction, run the woven tenant-1 query on the seeded data, then
  again after deleting (or mutating) every tenant-2 row. The two
  results must be identical. The seed data is asymmetric enough that
  an unscoped EXISTS, NOT EXISTS, ALL or scalar subquery changes the
  answer, and a pinned test checks that the oracle is live: the
  unwoven query must fail it.
- **Fuzzer:** new selector bytes add the IN, NOT IN, ANY, ALL,
  EXISTS, correlated NOT EXISTS, scalar and HAVING shapes, alone and
  inside a §14 nested core. Existing seeds decode unchanged, as in
  §14.
- **devdb:** a row-level proof on real PostgreSQL for each kind in
  §15.2.

### 15.8 Later phases (not settled)

- **Phase 2:** sublinks inside template construct bodies (§15.5);
  sublinks in the SELECT list, `ORDER BY`, a join's `ON`, and `CASE`
  in those positions; `ARRAY(…)`. A scalar SELECT-list subquery
  projects designated data, so the marker oracle applies there as
  well.
- **Phase 3:** sublinks in UPDATE/DELETE WHERE, and `MULTIEXPR`. This
  overlaps the open D6 follow-up that hidden reads inside an
  UPDATE/DELETE are gated on the outer kind rather than on
  `coversSelect()`.
- **Orthogonal:** `WITH RECURSIVE`, and MySQL/SQLite nested positions.

### 15.9 Owner decisions (2026-11-21)

- **Q1 — `op ANY`/`op ALL` in phase 1: yes.** They read exactly like
  `IN`/`NOT IN`; §15.1 admits `ANY_SUBLINK` whatever its `OperName`,
  and `ALL_SUBLINK`.
- **Q2 — construct-body sublinks: phase 2**, not a phase 1b (§15.5,
  §15.8).
- **Q3 — the §15.2 semantics are adopted** as the documented
  behaviour (RLS-equivalent; anti-joins may return more outer rows).
- **Q4 — the `SQLETCH125` wording changes.** The current text names
  "a WHERE/SELECT-list subquery" as unscopable, and that is no longer
  true once phase 1 lands. The implementation PR rewrites it as
  follows, and keeps the code, manual and spec in step:
  - `internal/policy/weave.go` (the hidden-read refusal): "a
    designated table appears inside a subquery or CTE body sqletch
    cannot scope (a subquery outside a WHERE/HAVING clause, an
    ARRAY subquery, a WITH RECURSIVE body, a
    subquery inside a template construct, or a nested position this
    dialect or statement kind does not support)".
  - `docs/manual/08-diagnostics.md` SQLETCH125: "inside a SELECT-list,
    ORDER BY, ON or other non-WHERE/HAVING subquery, an `ARRAY`/row
    comparison subquery, a subquery inside an `@if-present`/`@when`/
    `@choose` body, any subquery of an UPDATE/DELETE/INSERT, a `WITH
    RECURSIVE` body, or — on MySQL/SQLite — any subquery/CTE".
  - `docs/manual/12-policies.md`: the refused-positions list is
    reworded to match, a sublink bullet with an example joins the
    §14 bullet, and it states the §15.2 consequences.
  - `docs/spec.md` "Cross-Query Policies", the
    `internal/dialect/frontend.go` `NestedCores` doc comment, and
    CLAUDE.md are updated to match.

  These texts describe phase 1 behaviour, so they shipped with the
  implementation and not with the design change. In the final wording,
  `weave.go` names "a subquery outside a WHERE/HAVING clause, an ARRAY
  subquery, a WITH RECURSIVE body, or a nested position this dialect or
  statement kind does not support". A subquery inside a construct body
  has its own diagnostic (§15.5).
