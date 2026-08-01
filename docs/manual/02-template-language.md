# The template language

A sqletch template is a plain SQL file. Each query is one SQL
statement with a header, `:name` parameters, and a small closed set of
`@constructs` marking the parts that may vary. Everything outside a
construct is the **skeleton** — emitted byte-for-byte, never
reformatted.

The design bargain: constructs are only allowed where deleting or
inserting a fragment provably cannot change the meaning of anything
around it. In exchange, sqletch verifies *every reachable shape* of
the query against a real database at compile time. When a template
won't compile, the diagnostic names the rule and the rewrite
([diagnostics reference](08-diagnostics.md)).

## Queries

```sql
-- name: SearchUsers :many
SELECT ... ;
```

`:one` (exactly one row; `ErrNoRows` otherwise), `:many` (slice),
`:exec` (no result), `:execrows` (affected count). One statement per
query; names are global across files.

Parameters are `:snake_case` tokens. Never write `$1` or `?` —
placeholder emission belongs to the compiler.

## `@if-present` — optional fragments

```sql
WHERE TRUE
@if-present(status)
  AND u.status = :status
@endif
```

The fragment exists exactly when the caller provides every listed
parameter (`@if-present(a, b)` requires both — flat conjunction is the
only combinator; guards never nest). In Go, guarded parameters are
pointers and `nil` omits the fragment.

Fragments must occupy one of the **slots**, and each fragment must be
exactly one complete item of its slot:

| Slot | Form |
| --- | --- |
| WHERE conjunct | body starts with `AND`; parenthesized on emission |
| HAVING conjunct | same |
| JOIN item | a whole `JOIN … ON …` (INNER or LEFT only) |
| UPDATE SET item | `, col = :v` — PATCH semantics |
| INSERT column/value pair | a `, col` item paired with its `, :v` item |

Anchor rule: a clause can't consist only of optional items. WHERE and
HAVING keep an explicit `TRUE` anchor; UPDATE keeps one unconditional
SET item. `sqletch fmt` inserts the `TRUE` anchors for you.

INSERT pairing: for each guard set, the guarded column items and
guarded VALUES items must be equinumerous, aligned, and at the tail of
their lists — so pairs appear and disappear together.

## `@when` — value-conditional fragments

```sql
@when(include_cron = false)
  AND a.actor_id IS NOT NULL
@end
```

Like `@if-present`, but the condition is a comparison of a parameter
against a literal (`=`, `!=`, `<>`; string / integer / boolean
literals). The parameter is a plain (non-pointer) field; the fragment
exists when the comparison holds. A parameter used *only* in `@when`
heads is a control value typed by its literal and is never sent to the
database.

## `@choose` — one of N verified fragments

```sql
@choose(sort)
@case(created_at_desc)
ORDER BY u.created_at DESC
@case(email_asc)
ORDER BY u.email ASC, u.id ASC
@default
ORDER BY u.id ASC
@end
```

Exactly one case is emitted, selected by a generated enum
(`SearchUsersSortEmailAsc`, …). With a `@default`, the zero value
selects it; without one, the zero value is an error before any SQL is
sent. Every case is verified as its own rendering.

Slots: a whole `ORDER BY` clause, a projection expression
(`@choose … @end AS alias` — the alias stays in the skeleton, and
cases may not define names), or a `GROUP BY` clause.

## `@order-by` — caller-ordered sort keys

```sql
@order-by(sort)
@key(created_at)
u.created_at
@key(email)
u.email
@default
ORDER BY u.id ASC
@end
```

The caller passes a sequence of generated key constants
(`OrderedUsersSortEmailDesc`, …); sqletch emits `ORDER BY` in that
order. Duplicate or out-of-range keys error before the database.
Empty sequence → the `@default` body (or no ORDER BY without one).
This is the one sanctioned reordering in the language; each key body
must be a single sort key expression. Not available under
`DISTINCT ON`; `FETCH FIRST … WITH TIES` requires a `@default`.

## `@filter-tree` — caller-composed boolean trees

```sql
WHERE TRUE
  AND @filter-tree!(scope)
@predicate(tenant)
u.tenant_id = :scope_tenant_id
@predicate(email_prefix)
u.email LIKE :scope_prefix || '%'
@end
```

The caller builds an And/Or tree over the **closed predicate
vocabulary** using generated typed constructors:

```go
scope := gen.And(
    gen.FilterUsersTenant(42),
    gen.Or(gen.FilterUsersStatusEq("active"), gen.FilterUsersEmailPrefix("a")),
)
```

Predicate parameters are constructor arguments (never struct fields);
values ride as binds, positions come from cached bind *plans*. The
`!` marks a **required** tree: `nil` returns `ErrFilterRequired`, and
`gen.<Query>Unscoped()` is the explicit, greppable opt-out. Tree size
is capped (`filter_tree_caps`; defaults 32 nodes / depth 8). One block
per query, WHERE-conjunct slot only.

## `@in` — variable-length membership

```sql
WHERE u.status @in(:statuses)
```

`statuses` is a slice. PostgreSQL renders `= ANY($1)` (one static
shape); MySQL/SQLite render `IN (?, …)` with one placeholder per
element (the arity is part of the verified shape key). An empty slice
matches nothing — FALSE even for a NULL operand — on every dialect.
Allowed at top-level WHERE/HAVING skeleton positions; inside guarded
bodies write the membership test directly (PostgreSQL:
`= ANY(:param)`).

## The rules, informally

- **One complete node per fragment** (R1): a fragment can't regroup
  its neighbors' meaning.
- **Constant result shape** (R2): optional parts never add or remove
  result columns (no `SELECT *` over an optional join; optional joins
  are INNER/LEFT only).
- **No dangling references** (R3): referring to an optional join's
  columns requires sharing its guard.
- **No nesting** (R5), **anchors** (R6), **INSERT pairing** (R7),
  **no name definitions in cases** (R8), **guards must matter** (R9 —
  a guard on a required parameter, or a control parameter that also
  binds, is an error).

The full statement of the rules with rationale lives in
[`docs/spec.md`](../spec.md); the diagnostics quote them when violated.

## What is deliberately NOT expressible

Free-form SQL splicing, dynamic identifiers (table/column names from
values), guarded fragments inside subqueries, arbitrary reordering.
If a query's dynamism doesn't fit the vocabulary, write two queries —
each fully verified — and choose in Go. The boundary is the product:
everything inside it is verified; nothing outside it can be smuggled
in at runtime.
