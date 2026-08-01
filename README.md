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

> **Status: v1.0 candidate.** PostgreSQL, MySQL, and SQLite. The
> template language and generated API are frozen for v1 (see the
> [stability audit](docs/design/12-v1.md)); the remaining steps to the
> tag are release mechanics.

## The problem

Every sqlc user eventually hits the same wall: a search screen with
optional filters. The escape hatches are all bad —

- `WHERE (:status IS NULL OR u.status = :status)` pessimizes the query
  plan for *every* caller, and
- runtime query builders (squirrel, goqu, …) throw away static
  verification entirely.

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
[docs/template-language.md](docs/template-language.md).

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

## Compared to the alternatives

|                              | SQL-first | dynamic queries | statically verified |
|------------------------------|:---------:|:---------------:|:-------------------:|
| sqlc                         | ✓         | ✗               | ✓                   |
| squirrel / goqu (builders)   | ✗         | ✓               | ✗                   |
| GORM / Ent (ORMs)            | ✗         | ✓               | partial             |
| **sqletch**                  | ✓         | ✓               | ✓                   |

sqletch deliberately does **not** replace sqlc — it uses the same
authoring conventions (`-- name:` headers, `DBTX`/`Queries`/`WithTx`)
so both generators coexist in one codebase and one transaction. Keep
your static queries in sqlc; move the conditional ones to sqletch.

## Quick start

Requirements: Go 1.24+, and Docker (or a disposable PostgreSQL 16 via
`database.dsn`) for cold generates.

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

$ go run github.com/moznion/sqletch/cmd/sqletch generate
sqletch: 3 queries ok (oracle cache: 0 hits, 6 misses; offline: no)

$ go run github.com/moznion/sqletch/cmd/sqletch check   # warm: no DB needed
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
projections, and caller-supplied SQL strings. The template-language
reference is [docs/template-language.md](docs/template-language.md);
the full boundary — and the reasoning behind every rule — lives in
[docs/spec.md](docs/spec.md); the implementation
design is under [docs/design/](docs/design/). **User documentation lives in
the [manual](docs/manual/README.md)** — getting started, the template
language reference, per-dialect guides, and the
[sqlc migration guide](docs/manual/10-sqlc-migration.md).

## Roadmap

- **v0.2 (shipped)** — partial `UPDATE` (PATCH semantics), optional
  `INSERT` column/value pairs, `@choose` in projections and GROUP BY,
  `sqletch fmt`, strict static expansion, `explain --enumerate`
- **v0.3 (shipped)** — `@when` value guards, HAVING slot,
  `@filter-tree` (typed, composable filters across layer boundaries —
  with a required mode for multi-tenant safety), `@order-by` multi-key
  sorting, `explain --analyze`
- **v0.4 (shipped)** — `@in` (`= ANY` on PostgreSQL, arity-expanded
  `IN (?, …)` on MySQL/SQLite), `-- @param` / `-- @column` type
  annotations, the MySQL driver (TiDB-parser frontend,
  COM_STMT_PREPARE oracle, `database/sql` codegen), the SQLite driver
  (rqlite/sql frontend, in-process WASM SQLite oracle — no Docker at
  all), the LSP server (`sqletch lsp`), and editor grammars
  (`editors/`: VS Code extension with TextMate injection + LSP client,
  tree-sitter). The embedded PostgreSQL oracle spike is done
  (feasible; see `docs/design/09-embedded-oracle.md` — shipping waits
  on upstream libpglite)
- **v1.0 (in progress)** — stability freeze with written compatibility
  promises (`docs/design/12-v1.md`), self-describing cache format, the
  [user manual](docs/manual/README.md) and
  [sqlc migration guide](docs/manual/10-sqlc-migration.md), per-dialect
  examples. Remaining: CHANGELOG, the `v1.0.0` tag

## Development

```console
$ go test ./...                          # unit suites
$ go test -tags devdb ./internal/e2e/    # real-database E2E (Docker)
$ golangci-lint run --build-tags devdb ./...
```

## License

[Apache-2.0](LICENSE).
