# Cross-query policies

A **policy** is a boolean predicate declared once in `sqletch.yaml`
and woven, at compile time, into every query that touches a designated
table — plus a check proving that no reachable shape in your codebase
touches those tables unscoped. Where `@filter-tree!` protects the
queries that declare it, a policy inverts the default: a query added
next month is scoped the moment it compiles, and opting out is one
explicit, reviewable annotation.

```yaml
policies:
  - name: tenant_scope
    tables: [orders, order_items, invoices]
    predicate: "{}.tenant_id = :tenant_id"
    param:
      name: tenant_id
      type: bigint            # required on MySQL/SQLite; asserted on PostgreSQL
    applies_to: [select, update, delete]   # default: all three
```

`{}` stands for the designated relation *as it is named in each
query*: its alias if it has one, else the table name. With that
declaration:

```sql
-- name: ListOrders :many
SELECT o.id, o.total FROM orders AS o ORDER BY o.id;
```

compiles as if you had written `WHERE o.tenant_id = :tenant_id`, and
`tenant_id` appears in `ListOrdersParams` like any other parameter.
The SQL that is verified against the database **is** the scoped SQL —
there is no runtime rewriting layer, and no window where the verified
statement and the executed statement differ.

## Weaving rules

- The conjunct is unconditional skeleton text, present in **every**
  shape. It is appended right after `WHERE`, or a `WHERE` clause is
  synthesized when the statement has none (`DELETE FROM orders` — the
  case with the most to gain).
- Every top-level occurrence of a designated table gets its own
  conjunct; a self-join is scoped on both sides.
- Multiple policies weave in declaration order.
- A query that already contains a token-identical unconditional
  conjunct is left alone (hand-scoped queries are not double-woven).
  A copy inside `@if-present` deliberately does **not** count — it
  vanishes in guard-off shapes.
- The R6 anchor rule is checked before weaving, so a template's
  validity never depends on your configuration.
- `INSERT … VALUES` filters no rows and is not a policy target; an
  `INSERT … SELECT` that reads a designated table is rejected
  (`SQLETCH125`) — opt out or restructure.

- A designated table on the null-extended side of an outer join
  (e.g. the right side of a `LEFT JOIN`) is woven into **that join's
  `ON` clause** instead: a `WHERE` conjunct would silently turn the
  outer join into an inner join, while the `ON` conjunct preserves
  the outer row set and scopes only the joined rows.

Positions sqletch cannot scope are rejected with `SQLETCH125` rather
than silently skipped: a designated table inside a subquery, CTE, or
set-operation branch; joined with `USING`/`NATURAL` on a
null-extended side (no `ON` expression to extend — rewrite as an
explicit `ON`); introduced by a guarded `@if-present` join; or bound
to a name that is not a bare identifier. Restructure the query, or
opt out.

## Opting out

```sql
-- name: ListAllOrdersForBackfill :many
-- @policy-optout: tenant_scope (batch job; runs outside any tenant)
SELECT ...
```

The reason is mandatory — it documents the exemption in review and in
the `explain` report. An opt-out naming an unknown policy, or one that
does not apply to the query, is `SQLETCH126`: renaming a policy can
never silently disarm its opt-outs.

## Enforcement

Weaving covers what the weaver reaches; a separate enforcement pass
proves the invariant from the compiled result itself: for every
relation whose table a policy designates, a matching conjunct must be
present in the query's WHERE clause **in every reachable shape**
(`SQLETCH124` otherwise). It runs in the same pass the LSP uses, so a
violation appears live in your editor.

## Auditing coverage

"Which queries are unscoped, and why" is a command's output, not a
code-reading exercise:

```console
$ sqletch explain ListOrders AllAuditActions
ListOrders
  ...
  policies:
    tenant_scope: woven (o.tenant_id = :tenant_id)

AllAuditActions
  ...
  policies:
    tenant_scope: opted out (ops dashboard; aggregates across tenants)
```

The same data is a `policies` array in each query's JSON summary
(`.sqletch/explain/<Query>.json`, rewritten by every `generate`), so
CI can watch the opt-out *set* — the thing that should only ever grow
deliberately:

```console
$ jq -r '.name as $q | (.policies // [])[]
         | select(.status == "opted_out") | "\($q): \(.name) (\(.reason))"' \
    .sqletch/explain/*.json
AllAuditActions: tenant_scope (ops dashboard; aggregates across tenants)
```

Diff that output against a committed allowlist and a new opt-out
becomes a reviewable CI failure instead of a silent exemption. The
opt-out rate is also the feature's health metric: if it climbs, the
policy's table set or your query patterns need a conversation, not
more exemptions.

## Compared to the alternatives

- **ORM default scopes** (a base query that appends the filter) are
  runtime behavior: they cover the code paths that use them, can be
  bypassed, and nothing *proves* coverage. A policy is compile-time
  and quantified — the enforcement pass checks every reachable shape
  of every query, and the exceptions are enumerable.
- **Row-level security** is the database-side answer and a good one —
  where it exists (PostgreSQL; MySQL and SQLite have none), it also
  covers non-sqletch clients. Its cost is a per-connection
  session-variable discipline whose failure mode (a missed `SET`) is
  discovered at runtime. Policies move the check to compile time and
  work identically on all three dialects; they are defense at a
  *different layer*, so use RLS as well where you can.
- **`@filter-tree!`** is the per-query tool for filters the *caller*
  chooses; a policy is for invariants the caller may not choose. Its
  required mode protects the queries that declare it — a policy
  protects the query nobody remembered to protect. A query may have
  both.

## The parameter

The policy parameter is an ordinary parameter: it lands in the
affected queries' `Params` structs, typed by the oracle on PostgreSQL
(where `param.type` is asserted like a `-- @param` hint) and by
`param.type` on MySQL/SQLite (where it is mandatory). There is no
ambient state and no context extraction — the generated API contract
is unchanged, and forgetting the value is a compile error in *your*
code, not a runtime leak.

## Boundary

Policies constrain only sqletch-generated queries; hand-written SQL
and other ORMs in the same process are untouched. They express
conjunctive row filters — not column masking, not per-role rules. And
they guarantee the predicate is *present*, not that its argument is
*correct*: a process that passes the wrong tenant ID still reads the
wrong tenant. They complement `@filter-tree!` (caller-chosen filters;
a query may have both) and database row-level security (runtime
defense in depth that also covers non-sqletch clients) — use RLS *as
well*, not instead, where it exists.
