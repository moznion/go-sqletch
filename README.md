# sqletch

**Statically verified, dynamically composed SQL for Go.** *(pronounced
like "sketch")*

sqletch compiles plain SQL with a tiny conditional-template layer into
fully typed Go code — like [sqlc](https://sqlc.dev), but for the
queries sqlc can't express: optional filters, optional joins,
selectable sort orders. Every SQL fragment that can ever reach your
database is verified at compile time; at runtime, generated code only
selects and concatenates those pre-verified constants.

One sentence positioning: **a query builder's everyday dynamism,
authored the sqlc way.**

> **Status: v1.0.** PostgreSQL, MySQL, and SQLite. The template
> language, the generated API, the `runtime` package, `sqletch.yaml`,
> the CLI, and the meanings of the `SQLETCHnnn` diagnostic codes are
> stable for all of v1 — see
> [compatibility and versioning](docs/manual/11-compatibility.md).

## The problem

Every sqlc user eventually reaches the same question: a search screen
with optional filters. The usual answers all carry a real cost —

- `WHERE (:status IS NULL OR u.status = :status)` pessimizes the query
  plan for *every* caller, and
- runtime query builders (squirrel, goqu, …) trade away static
  verification.

sqletch is the third option: conditional SQL that stays statically
verified.

## What it looks like

```sql
-- name: SearchUsers :many
SELECT u.id, u.email, u.status, u.created_at
FROM users AS u

@if-present(organization_id)
JOIN organization_users AS ou
  ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif

WHERE TRUE

@if-present(status)
  AND u.status = :status
@endif

@if-present(email_prefix)
  AND u.email LIKE :email_prefix || '%'
@endif

@choose(sort)
@case(created_at_desc)
ORDER BY u.created_at DESC
@case(email_asc)
ORDER BY u.email ASC, u.id ASC
@default
ORDER BY u.id ASC
@end

LIMIT :limit;
```

`sqletch generate` turns that into one typed function:

```go
rows, err := q.SearchUsers(ctx, gen.SearchUsersParams{
    Status: gen.Ptr("active"),               // nil omits the predicate
    Sort:   gen.SearchUsersSortCreatedAtDesc,
    Limit:  50,
})
```

Each combination of present parameters produces a *plain static query*
as far as PostgreSQL is concerned — its own optimal plan, its own
prepared statement. No `IS NULL OR` tricks, no string building.

Every construct, side by side with the Go it generates and the SQL it
composes, is documented in
[the template language reference](docs/manual/02-template-language.md).

## How it works

The template above reaches 2⁴ × 4 = **64 distinct query shapes**, but
sqletch never enumerates them. The key insight is that SQL's
list-shaped clauses are **compositional**: if each `AND` conjunct
verifies on its own, any subset of the conjunction verifies too.
Structural rules confine the constructs to positions where that
argument holds, so verifying

1. the *maximal* rendering (all fragments on), plus
2. each `@choose` case,

guarantees every reachable shape parses, resolves, and type-checks —
at cost linear in template size, not shape count. Types come from
PostgreSQL itself (`PREPARE`/`Describe` against a dev database), so
there is no hand-written inference engine to disagree with your server.

At runtime, generated code concatenates pre-verified constant
fragments selected by a shape key. User values travel exclusively
through bind parameters — **SQL injection is impossible by
construction**, and a conformance test pins that what was verified is
byte-for-byte what gets composed.

The dev database is needed only on cache misses: oracle results and a
catalog snapshot are committed to your repository, so CI and warm
builds run **fully offline**.

## Tenant isolation you can prove (cross-query policies)

The most expensive bug in a multi-tenant codebase is the query someone
writes next month with no tenant filter. Per-query discipline —
reviews, base repositories, `@filter-tree!` — protects the queries
that opt in; nothing protects the one that forgot. A **policy**
inverts that default:

```yaml
# sqletch.yaml
policies:
  - name: tenant_scope
    tables: [orders, order_items, invoices]
    predicate: "{}.tenant_id = :tenant_id"
    param:
      name: tenant_id
      type: bigint
```

Now every query touching those tables is scoped the moment it
compiles. This template never mentions tenants:

```sql
-- name: ListOrders :many
SELECT o.id, o.total FROM orders AS o ORDER BY o.id;
```

but it compiles as if it ended with `WHERE o.tenant_id = :tenant_id`,
and `tenant_id` shows up in `ListOrdersParams` like any other
parameter — forgetting the *value* is now a Go compile error, not a
data leak. `UPDATE` and `DELETE` are covered by the same declaration
(exactly the statements where a filter is easiest to forget), and a
designated table on the null-extended side of a `LEFT JOIN` is scoped
in the join's `ON` clause, so the outer join keeps its row set.

Three properties make this more than a macro:

- **The invariant is proved, not hoped.** A separate enforcement pass
  re-derives, from the compiled result itself, that *no reachable
  shape of any query touches a designated table unscoped*
  (`SQLETCH124`). That quantifier — "every shape" — is checkable
  because sqletch already enumerates every shape; a scoping conjunct
  hidden inside `@if-present` fails the check, because it vanishes in
  guard-off shapes.
- **Verification sees the real SQL.** Weaving happens before
  rendering, so the SQL that is prepared, `EXPLAIN`ed, typed, and
  cached *is the scoped SQL*. There is no runtime rewriting layer and
  no window where verified and executed statements differ.
- **Opting out is a visible event.** `-- @policy-optout: tenant_scope
  (backfill; runs across tenants)` — the reason is mandatory, the
  annotation is greppable, and `sqletch explain` reports per-query
  coverage (woven / opted out, with reasons) machine-readably, so CI
  can fail when the opt-out set grows.

And the boundary, stated as plainly as the pitch: policies constrain
**only sqletch-generated queries** — hand-written SQL in the same
process is untouched; they guarantee the predicate is *present*, not
that its runtime argument is *correct*; they express conjunctive row
filters, not column masking or per-role rules; and a position sqletch
cannot scope (a designated table inside a subquery or CTE, a
`USING`/`NATURAL` join, a guarded join) is a **loud compile error**
(`SQLETCH125`) requiring an explicit opt-out — never a silent skip.
Where row-level security exists, use it *as well*: RLS is runtime
defense in depth that also covers non-sqletch clients. The full story
is [the policies chapter](docs/manual/12-policies.md).

## Compared to the alternatives

|                              | SQL-first | dynamic queries | statically verified |
|------------------------------|:---------:|:---------------:|:-------------------:|
| sqlc                         | ✓         | ✗               | ✓                   |
| squirrel / goqu (builders)   | ✗         | ✓               | ✗                   |
| GORM / Ent (ORMs)            | ✗         | ✓               | partial             |
| **sqletch**                  | ✓         | ✓               | ✓                   |

Each of these tools is excellent at what it set out to do; sqletch
aims at the one cell none of them targets, and holding it is what the
design buys you:

- **Verification scales with the template, not with the shape count.**
  Adding an optional filter doubles the reachable shapes and adds one
  conjunct to check.
- **Types come from the database itself.** Parameter and result types
  are whatever `PREPARE` / `Describe` answered, cached in your
  repository so CI never opens a connection.
- **What was verified is byte-for-byte what runs**, pinned by a
  conformance test over every shape and every bind position — and
  values travel exclusively through bind parameters, so SQL injection
  is impossible by construction.
- **One row type for every shape.** Result columns, names, types, and
  nullability are identical across all reachable shapes, and
  nullability never narrows because a guarded join happens to be
  active — the analysis only claims non-null when it holds in *all* of
  them.
- **Plans are checked, not just syntax.** Each shape is a plain static
  query with its own optimal plan and its own prepared statement — no
  `IS NULL OR` idiom pessimizing every caller — and
  `check --exhaustive` prepares and `EXPLAIN`s every one of them.
- **Builder-grade composition stays typed.** `@filter-tree` accepts
  filters composed across layer boundaries, and `@order-by`
  multi-key sorts, from a closed vocabulary fixed at compile time.
- **Codebase-level invariants are checkable.** Because every reachable
  shape is enumerated, a policy can state — and the compiler can
  prove — that no shape anywhere touches a designated table unscoped
  ([cross-query policies](docs/manual/12-policies.md)). ORM default
  scopes are runtime and opt-out-able; RLS is per-connection
  discipline and absent on MySQL; neither can make this claim at
  compile time.
- **The editor sees the same compiler.** `sqletch lsp` reports the
  same `SQLETCHnnn` diagnostics as `check` while you type, without
  ever opening a database.

sqlc is a great tool, and sqletch deliberately does **not** replace it
— sqletch owes it the whole authoring model and uses the same
conventions (`-- name:` headers, `DBTX`/`Queries`/`WithTx`) so both
generators coexist in one codebase and one transaction. Keep your
static queries in sqlc; move the conditional ones to sqletch.

## Quick start

Requirements: Go 1.27+ (currently the 1.27 rc; `go.mod` pins the
toolchain). Cold generates need a disposable dev database — Docker, or
a DSN you point at via `database.dsn`. On SQLite there is nothing to
install: the oracle is the real engine, in-process.

```console
$ cat sqletch.yaml
version: 1
dialect: postgres
server_version: "16"
schema:
  files: [db/schema.sql]
queries: [queries/*.sql]
output:
  package: gen
  path: gen

$ go run github.com/moznion/go-sqletch/cmd/sqletch generate
sqletch: 3 queries ok (oracle cache: 0 hits, 6 misses; offline: no)

$ go run github.com/moznion/go-sqletch/cmd/sqletch check   # warm: no DB needed
sqletch: 3 queries ok (oracle cache: 6 hits, 0 misses; offline: yes)
```

Commit `.sqletch/cache/` — that's what keeps CI offline. The rest of
`.sqletch/` (`explain/`, `expanded/`) is derived output that an offline
`generate` rewrites, so `.gitignore` it. See [`examples/`](examples/)
for a complete working project (its generated code and cache are
committed; it builds with no database at all).

Other commands:

```console
$ sqletch check --exhaustive   # prepare + EXPLAIN every reachable shape
$ sqletch explain SearchUsers  # guards, cases, types, shape counts
$ sqletch lsp                  # language server over stdio (offline)
```

The language server reports the same `SQLETCHnnn` diagnostics as
`check` while you type — scanner and structural rules always, the
oracle-backed checks whenever the committed cache covers the query —
and provides go-to-definition between `:param` occurrences and their
`-- @param` annotations. It never touches a database. Point any LSP
client at `sqletch lsp` for `.sql` template files.

Templates do not have to live in `.sql` files. A `//sqletch:query`
const inside a Go file compiles identically — same generated code,
same cache entries — so a query can sit next to the repository code
that uses it:

```go
//sqletch:query
const searchUsersSQL = `
-- name: SearchUsers :many
SELECT u.id, u.email FROM users AS u
WHERE TRUE
@if-present(status)
  AND u.status = :status
@endif
;
`
```

List the file in `queries:` and generate as usual. Conditionality
still lives in the constructs — the const requirement is what keeps
Go control flow out of SQL construction. See
[the template language](docs/manual/02-template-language.md).

## Guarantees and limits

Verified at compile time, for **every** reachable shape: syntax,
identifier resolution, parameter and result types, constant result
shape, and (conservatively) nullability — nullable columns become
pointer fields, and the analysis never claims non-null unless it holds
in *all* shapes. Planner-only failures (e.g. `FOR UPDATE` with an
optional `LEFT JOIN`) are rejected statically where known and covered
by `check --exhaustive` otherwise.

Deliberately out of scope: dynamic table/column names, shape-changing
projections, and caller-supplied SQL strings. The full boundary — and
the reasoning behind every rule — lives in
[docs/spec.md](docs/spec.md); the implementation design is under
[docs/design/](docs/design/). **User documentation lives in the
[manual](docs/manual/README.md)** — getting started, the
[template language reference](docs/manual/02-template-language.md),
per-dialect guides, and the
[sqlc migration guide](docs/manual/10-sqlc-migration.md).

## What's in the box

- **Constructs**: `@if-present` (optional WHERE/HAVING conjuncts,
  filter-only INNER/LEFT joins, `UPDATE SET` items for PATCH
  semantics, paired `INSERT` column/value items), `@when` value
  guards, `@choose` closed choices (ORDER BY, projections, GROUP BY),
  `@order-by` multi-key sorting, `@filter-tree` typed filters
  composable across layer boundaries (with a required mode for
  multi-tenant safety), and `@in` variable-arity membership.
- **Policies**: config-declared predicates woven at compile time into
  every query touching designated tables, enforced across every
  reachable shape, with auditable per-query opt-outs
  ([manual](docs/manual/12-policies.md)).
- **Dialects**: PostgreSQL (types inferred by the server),
  MySQL and SQLite (types from `-- @param` / `-- @column`
  annotations). SQLite needs no Docker and no server at all — its
  oracle is the real engine, in-process.
- **Authoring**: `.sql` files or `//sqletch:query` consts in Go files;
  `sqletch fmt` for canonical layout; strict static expansion when
  every SQL text must exist on disk for audit.
- **Tooling**: `generate`, `check [--exhaustive]`,
  `explain [--enumerate|--analyze]`, `fmt`, and `lsp` — plus editor
  grammars under [`editors/`](editors/) (a VS Code extension with a
  TextMate injection grammar and an LSP client, and a tree-sitter
  grammar).

## Beyond v1.0

Recorded, unscheduled, and none of it changes the verification model:

- **Embedded PostgreSQL oracle** — cold `generate`/`check` with no
  external database, the way SQLite already works. The spike is done
  and feasible; shipping waits on upstream libpglite
  ([docs/design/09-embedded-oracle.md](docs/design/09-embedded-oracle.md)).
- **Native inference backend**, differential-tested against the
  `(schema, query, types)` corpus every cache entry already
  produces — for MySQL first, which has no embeddable real engine.

## Development

```console
$ go test ./...                          # unit suites
$ go test -tags devdb ./internal/e2e/    # real-database E2E (Docker)
$ golangci-lint run --build-tags devdb ./...
```

## License

[Apache-2.0](LICENSE). Attribution notices are in [NOTICE](NOTICE).
