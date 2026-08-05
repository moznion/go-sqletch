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
`tenant_id` becomes a required argument of `ListOrders` — see
[The parameter](#the-parameter).
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

Positions sqletch cannot scope are rejected with `SQLETCH125` rather
than silently skipped: a designated table inside a subquery, CTE, or
set-operation branch; on the null-extended side of an outer join
(weaving `WHERE` there would silently turn your `LEFT JOIN` into an
inner join); introduced by a guarded `@if-present` join; or bound to
a name that is not a bare identifier. Restructure the query, or opt
out.

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

`sqletch explain` reports per-query coverage — which policies apply,
whether each is woven or opted out (with the reason) — and carries the
same data as a `policies` array under `--format json`, so CI can
assert on the opt-out set.

## The parameter

The policy parameter is typed by the oracle on PostgreSQL (where
`param.type` is asserted like a `-- @param` hint) and by `param.type`
on MySQL/SQLite (where it is mandatory). There is no ambient state and
no context extraction.

It is **an argument of the generated method**, not a field of the
params struct:

```go
func (q *Queries) ListOrders(
	ctx context.Context,
	tenantID int64,               // policy tenant_scope
	arg ListOrdersParams,
) ([]ListOrdersRow, error)
```

The reason is that Go cannot make a struct field mandatory. A keyed
composite literal that omits one compiles and yields the zero value,
so a policy parameter kept as a field would let

```go
q.ListOrders(ctx, gen.ListOrdersParams{Limit: 20})   // tenant_id == 0
```

compile, run, and return the rows of no tenant at all — the woven
predicate matching nothing rather than scoping the read. That failure
is silent: no error, just an empty result that looks like "nothing
matched". Only as an argument does forgetting the value fail to
compile, which is what makes "you cannot forget it" true of *your*
code and not only of the SQL.

The residual is an argument given an explicit zero (`0`, `""`). That is
a deliberate act rather than an oversight; policies still guarantee the
predicate's presence, never its argument's correctness (see
[Boundary](#boundary)).

Adding a policy therefore breaks every call site of every query it
touches, on purpose: a security control that could be adopted without
anyone revisiting the callers would be one whose absence nobody
notices either.

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
