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

## `-- @policy-apply: name`

```sql
-- name: CountAuditLogs :one
-- @policy-apply: tenant_scope
SELECT count(*) AS total FROM audit_logs;
```

Acknowledges that a [cross-query policy](12-policies.md) scopes this
query. It **changes nothing**: the conjunct is woven whether or not the
annotation is present. Its purpose is to make the query's scoping
readable in the template file — and to satisfy a policy declared with
`require_annotation: true`, which fails an unannotated query with
SQLETCH127.

A trailing `(reason)` is optional, unlike the opt-out's: an
acknowledgment claims no exemption, so there is nothing to justify.
Naming a policy that does not exist or does not apply to the query is
SQLETCH126, as is carrying both this and `-- @policy-optout` for one
policy.

## `-- @timeout <duration>`

```sql
-- name: AllAuditActions :many
-- @timeout 30s
SELECT a.action, count(*) AS occurrences FROM audit_logs AS a GROUP BY a.action;
```

Bounds the generated method with a context deadline: the method wraps
its `ctx` in `context.WithTimeout` before calling the driver and keeps
it until the rows are scanned, so a statement that outlasts the
deadline is interrupted by the driver and the call fails. The value is
a positive Go duration (`500ms`, `2s`, `1m30s`). A caller's own
shorter deadline still wins — the generated deadline only ever
tightens. The error on expiry is the driver's own, returned unchanged
— `context.DeadlineExceeded` on pgx (default) and MySQL,
`sqlite3.INTERRUPT` on SQLite, SQLSTATE 57014 with pgx's cancel-request
handler; see [the per-driver table](07-runtime-and-generated-code.md#timeouts-and-cancellation-what-the-driver-returns).

`-- @timeout none` opts the query out of `query_timeout.default` (see
[the config reference](05-config.md#field-notes)); without a default
it is the same as no directive. A malformed, non-positive, or repeated
`@timeout` is SQLETCH017 — never a silent fallback to "no deadline".
The timeout is a runtime property only: it changes no rendering and no
verification. Like every annotation, the comment stays in the skeleton
verbatim, so editing it re-keys the query's oracle cache entries (one
cold `generate`).

## `-- @nolint CODE[, CODE…] (reason)`

```sql
-- name: ActionCounts :many
-- @nolint SQLETCH130 (one row per distinct action)
SELECT action, count(*) FROM audit_logs GROUP BY action;
```

Suppresses [performance lints](08-diagnostics.md#performance-lints-warnings)
(SQLETCH128–132) for this one query, in every shape. The trailing
reason is optional but recommended — it is what a reviewer reads.

- Only performance-lint codes may be named. A structural, oracle, or
  configuration code, an unknown code, or a malformed directive is
  SQLETCH016, and the directive then suppresses nothing at all.
- Unlike golangci-lint's `//nolint`, the codes are mandatory (a bare
  `-- @nolint` would hide every lint added later) and follow a space,
  not a colon: `-- @nolint:SQLETCH130` is SQLETCH016 too.
- An `@nolint` whose lint does not fire on the query is a warning
  (SQLETCH133): delete it, so the lint can catch the next regression.
- Like every annotation it follows the `-- name:` header and stays in
  the skeleton verbatim (adding one changes the query's rendered SQL,
  so the next `generate` re-verifies it).
