# Coming from sqlc

sqletch is deliberately sqlc-shaped: same file layout (`schema.sql` +
`queries/*.sql` + generated package), same header comments, same
generated-API conventions. The delta is exactly one capability:
**compile-time-verified dynamic SQL** — the thing that today forces
`if` chains of string concatenation, an ORM, or N near-duplicate sqlc
queries.

## Coexistence first (recommended)

You do not migrate; you add. sqletch and sqlc generate into different
packages and share connections:

- PostgreSQL: both `DBTX` interfaces are satisfied by a `pgx.Conn`,
  `pgxpool.Pool`, or `pgx.Tx` — one `pgx.Tx` can span sqlc and
  sqletch calls in the same transaction.
- MySQL/SQLite: both use `database/sql`; share the `*sql.DB`/`*sql.Tx`.

A sane split: keep static queries in sqlc, move the search screens,
PATCH updates, and filtered listings — the queries that were painful —
to sqletch. Point both tools at the same `schema.sql`.

## Translating a query

Static queries port nearly verbatim:

| sqlc | sqletch |
| --- | --- |
| `-- name: GetUser :one` | identical |
| `$1` / `?` / `sqlc.arg(name)` | `:name` (named params only) |
| `sqlc.narg(name)` + `IS NULL` tricks | a real `@if-present` fragment |
| `sqlc.slice(names)` (MySQL/SQLite) | `u.status @in(:names)` |
| `overrides` in sqlc.yaml for types | annotations in the template (`-- @param` / `-- @column`) |

The `WHERE ($1::text IS NULL OR u.status = $1)` idiom deserves a
special mention: it ports to

```sql
WHERE TRUE
@if-present(status)
  AND u.status = :status
@endif
```

which is not just clearer — the two variants are *separately
prepared, planned, and typed* at compile time, and the planner never
sees the `OR IS NULL` de-optimization.

## What sqlc users must unlearn

- **No positional parameters.** `:name` everywhere; sqletch owns
  placeholder numbering per dialect.
- **The dev database is part of the compiler.** sqlc parses your
  schema itself; sqletch asks a real database (auto-managed container,
  or in-process for SQLite) and caches its answers in
  `.sqletch/cache/` — commit that directory and CI stays offline.
- **Dynamic parts must fit the construct vocabulary.** There is no
  string splicing escape hatch, by design: if it composes at runtime,
  it was verified at compile time. When a query's dynamism doesn't
  fit, write two queries and pick in Go.
- **Nullable columns are pointers** (no `sql.Null*` / pgtype
  wrappers). Nullability comes from catalog analysis with
  conservative treatment of optional joins; use `overrides` for
  application-level invariants.

## Migration checklist

1. `sqletch.yaml` next to sqlc.yaml, pointing at the same schema.
2. Move one painful query family; run `sqletch generate`.
3. Swap call sites (`sqlcgen.New(db)` → `gen.New(db)` for those
   calls); shared transactions keep working.
4. Commit `.sqletch/cache/`; add `sqletch check` to CI (exit 1 =
   template mistake, exit 2 = environment).
5. Repeat per query family; sqlc keeps whatever never needed to be
   dynamic.
6. Multi-tenant codebase? Once the tenant-scoped tables' queries are
   in sqletch, declare a [cross-query policy](12-policies.md) over
   them — the tenant filter becomes something the compiler proves
   rather than something reviews catch. (Policies cover sqletch
   queries only, so this lands naturally *after* a family migrates.)
