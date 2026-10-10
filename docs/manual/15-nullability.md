# Nullability

Every result column of a generated row struct is either a plain `T` or
an `optional.Option[T]`. This chapter explains how sqletch decides, why
it sometimes says "nullable" where you know better, and what to do
about it. The full design, including the counterexamples behind each
rule, is [design doc 05](../design/05-nullability.md).

## The contract

> A column is generated as `T` only if **no reachable shape** of the
> query can return NULL in it.

The guarantee is one-sided on purpose:

- A column typed `T` that receives NULL at runtime would be a sqletch
  bug — a NULL silently turned into a zero value, or a scan error in
  production.
- A column typed `Option[T]` that never actually holds NULL costs you
  an `.Unwrap()`/`OrElse` call. That is the accepted price.

So whenever the analysis cannot *prove* a column non-null, it says
nullable. No database protocol reports result nullability reliably
(MySQL's `NOT_NULL` flag degrades across joins and expressions;
PostgreSQL's describe carries none), so sqletch performs this analysis
itself, on
every dialect, from two inputs: the schema catalog (the `NOT NULL`
constraints your DDL declares) and the parse tree of the query.

Like everything else sqletch verifies, the decision covers every shape
at once: a template's dynamic parts never change a column's type
between calls.

## Seeing the reason: `sqletch explain`

`sqletch explain <Query>` prints each column's verdict together with
the rule that decided it:

```console
$ sqletch explain GetUserProfile
GetUserProfile
  …
  columns:
    id: int8 (not null; users.id is NOT NULL in the catalog)
    email: text (not null; users.email is NOT NULL in the catalog)
    nickname: text (nullable; users.nickname is nullable in the catalog)
    org_name: text (nullable; orgs.name is null-extended by an outer join)
```

When a column is `Option[T]` and you expected `T`, start here: the
reason names exactly which gate stopped the narrowing.

## How a column is decided

For each result column, in order:

1. **A config override** (`overrides` in sqletch.yaml, below) wins
   outright.
2. **A direct column reference** (`u.email`) is non-null when
   *all* of these hold:
   - the column is `NOT NULL` in the catalog;
   - its table is not on the null-extended side of an outer join
     (the right side of `LEFT JOIN`, the left side of `RIGHT JOIN`,
     either side of `FULL JOIN`);
   - sqletch can trust where the database says the column came from
     (see [Provenance](#provenance-when-sqletch-stops-trusting-the-database)).
3. **An unconditional `col IS NOT NULL` conjunct** in the WHERE clause
   makes that column non-null even when it would otherwise be nullable,
   because WHERE runs after the joins. "Unconditional" means skeleton
   text: a predicate inside `@if-present`, `@when`, `@choose`,
   `@filter-tree`, or a woven policy does not count. The table must
   also appear only once in the query (in a self join the database
   cannot tell `u1.org_id` from `u2.org_id`), and `UPDATE … RETURNING`
   never narrows this way (WHERE tests the old row, RETURNING returns
   the new one).
4. **An expression column** (`count(*)`, `lower(email)`,
   `a + b`, …) is nullable unless it is *provably total* — non-null
   regardless of the data:
   - `count(…)`, `now()`;
   - non-NULL literals, `EXISTS (…)`, `x IS [NOT] NULL` and other
     boolean tests;
   - `coalesce(…)` with at least one total argument
     (`coalesce(nickname, '')` is non-null; `coalesce(a, b)` over two
     nullable columns is not);
   - casts of total expressions (not on MySQL, where a failing `CAST`
     yields NULL);
   - `sum`/`min`/`max`/`avg` over a single `NOT NULL` column of a
     non-null-extended table, under a `GROUP BY` without grouping
     sets, `FILTER`, or `OVER` (every group is non-empty, so the
     aggregate has an input).

   Anything else — any other function call, arithmetic, `CASE`, a
   scalar subquery — is nullable. Add a `coalesce` or an override
   when you know better.

## Optional fragments never make a column non-null

The single most important rule: **conditional parts of a template are
ignored when deciding nullability.**

```sql
-- name: ListMembers :many
SELECT u.id, u.org_id
FROM users AS u
@if-present(org)
JOIN orgs AS o ON o.id = u.org_id AND o.slug = :org
@endif
WHERE TRUE
@when(only_assigned = true)
  AND u.org_id IS NOT NULL
@end
LIMIT :limit;
```

With `org` present the INNER JOIN filters out users whose `org_id` is
NULL; with `only_assigned` true the predicate does. But both are
optional — in the shape where neither applies, `org_id` can be NULL,
so it is `Option[int64]`. Narrowing from a guarded join or predicate
would be correct in some shapes and wrong in others, and one generated
struct serves all of them.

The rule has a structural counterpart: optional joins may not
contribute result columns and must be `INNER` or `LEFT` (rule R2,
SQLETCH1xx). That is what guarantees a guarded join can never
null-extend a column you select.

`@choose` projection cases are analyzed one by one, and the column is
nullable if it is nullable in *any* case.

## Provenance: when sqletch stops trusting the database

For a direct column reference, the database's prepare/describe answer
says which base table and column a result column came from. sqletch
cross-checks that answer against the query's own FROM list before
using it, because engines attribute columns *through* derived tables,
CTEs, set operations, and (on SQLite) views — a column read from
`users` inside a LEFT-JOINed derived table is still reported as
`users.email`, a `NOT NULL` column, even though the join can null it.

What this means in practice:

- **Derived tables and CTEs** are analyzed recursively; a NULL-extending
  join *outside* them carries into their columns. If the same table is
  reached by several paths and any path can null it, it is nullable.
- **These disable narrowing for the whole statement** (every column
  sourced from a table becomes nullable; total expressions are
  unaffected):
  - a statement-level `UNION`/`INTERSECT`/`EXCEPT`;
  - `GROUP BY ROLLUP`/`CUBE`/`GROUPING SETS` (super-aggregate rows put
    NULL in grouping columns regardless of `NOT NULL`);
  - on MySQL and SQLite, a database-qualified table name
    (`otherdb.users`);
  - on SQLite, any view referenced anywhere in the statement (SQLite
    reports a view's columns as its base table's and hides the view
    body).
- **These disable narrowing for the tables they mention**: a set
  operation, grouping sets, or `WITH RECURSIVE` *inside* a derived
  table or CTE.
- **PostgreSQL inheritance**: a child table can drop an inherited
  `NOT NULL`, so a parent with children narrows only under `FROM ONLY`.
  (Partitioned tables are unaffected; partitions cannot drop it.)
- **Scalar subqueries in the select list** return NULL when they match
  no row, so they are nullable, whatever column they read.
- **Views** on PostgreSQL and MySQL are fine: the view itself is
  reported as the source, with the engine's own nullability for its
  columns.

`explain` names the gate in each case ("narrowing disabled: statement
is a set operation", "… is exposed through a hazardous subquery", …).

## Overriding a verdict

When an application invariant guarantees a column is non-null and the
analysis cannot see it, override it in sqletch.yaml instead of editing
generated code:

```yaml
overrides:
  - query: ListMembers
    column: org_id      # the result column's output name (alias)
    nullable: false
```

`nullable: true` forces the other direction, which is always safe.

An override is **your** claim, not a proof: `nullable: false` on a
column that does return NULL brings back exactly the failure the
analysis exists to prevent. Prefer a rewrite that makes the fact
visible to the analysis when one exists:

| Instead of overriding … | write |
|---|---|
| a column you filter for non-NULL anyway | an unconditional `AND col IS NOT NULL` |
| a nullable column with a natural default | `coalesce(col, '') AS col` |
| an aggregate over a possibly-empty input | `coalesce(sum(x), 0) AS total` |
| a PostgreSQL parent table with children | `FROM ONLY parent` |

Overrides are keyed by query name and output column name:

- an entry naming no result column is **SQLETCH312** (a warning: the
  override is dead — usually a renamed alias);
- an entry matching several same-named columns is **SQLETCH313**
  (it forces all of them; give them distinct aliases);
- an entry naming a query that exists in several targets is
  **SQLETCH317** (it applies to all of them).

Overrides are a code-generation input only: they never change
renderings, the cache, or what is verified.

## Parameters

Nullability of *parameters* (whether you may pass SQL NULL) is a
separate, derived property: a parameter is `optional.Option[T]` only
when every place it is bound writes a nullable column. See
[nullable value parameters](02-template-language.md#nullable-value-parameters).

## How this is kept honest

The analysis is a whitelist that under-reports by design, and its
claims about engine behavior are pinned against the real databases:
the dev-database suites (`TestNullabilitySoundnessAdversarial` and its
MySQL/SQLite variants) seed NULLs into every nullable column and run
each proven counterexample — guarded joins, derived tables, set
operations, views, inheritance, scalar subqueries — asserting the
column stays nullable. A new precision rule is admitted only with such
a counterexample suite behind it (see
[the testing policy](../development/testing.md)).
