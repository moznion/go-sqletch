# Type annotations

Annotations are ordinary SQL comments inside a query (they stay in the
emitted SQL verbatim). They exist because not every database tells the
compiler every type.

## `-- @param name: type`

```sql
-- name: UsersInStatuses :many
-- @param tenant_id: bigint
-- @param statuses: varchar(32)
SELECT ...
```

| Dialect | Role |
| --- | --- |
| PostgreSQL | Optional **assertion**. The oracle infers parameter types itself; an annotation that disagrees is an error (SQLETCH213) — never a silent override. Useful as documentation and drift protection. |
| MySQL | **Mandatory** for every bind parameter. `COM_STMT_PREPARE` does not type parameters. |
| SQLite | **Mandatory** for every bind parameter. `sqlite3_prepare` does not type parameters. |

Control-only parameters (`@when`/`@choose`/`@order-by`/`@filter-tree`
selectors that never bind) need no annotation — they never reach the
database.

Type names are dialect type names, case-insensitive, length arguments
ignored (`varchar(16)` ≡ `varchar`). MySQL understands `unsigned`
(`bigint unsigned` → `uint64`). On expanding dialects the `@in`
parameter's annotation gives the **element** type — the Go field is a
slice of it.

## `-- @column name: type` (SQLite)

```sql
-- name: TenantActivity :many
-- @column actions: integer
SELECT a.tenant_id, count(*) AS actions ...
```

SQLite reports no declared type for expression columns (`count(*)`,
computed values, `CAST` included). Such columns require a `@column`
annotation naming the result column; direct table columns never need
one (their declared type flows through the affinity rules). Missing or
unknown annotations are diagnostics naming the exact column
(SQLETCH311).

## `-- @policy-optout: name (reason)`

```sql
-- name: ListAllOrdersForBackfill :many
-- @policy-optout: tenant_scope (batch job; runs outside any tenant)
SELECT ...
```

Exempts one query from one [cross-query policy](12-policies.md). The
parenthesized reason is mandatory (SQLETCH001 without it); naming a
policy that does not exist or does not apply to the query is
SQLETCH126. Like every annotation it must follow the `-- name:` header
and stays in the skeleton verbatim.
