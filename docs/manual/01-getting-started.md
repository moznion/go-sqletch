# Getting started

sqletch compiles SQL templates with *conditional fragments* into typed
Go code, the sqlc way — except every reachable query shape is verified
against a real database at compile time, and runtime work is nothing
but deterministic concatenation of pre-verified constant fragments.

## Install

```console
$ go install github.com/moznion/go-sqletch/cmd/sqletch@latest
```

## Project layout

```
myapp/
  sqletch.yaml          # configuration
  db/schema.sql         # plain DDL, applied to a disposable dev database
  queries/users.sql     # templates
  gen/                  # generated Go (output)
  .sqletch/cache/       # committed oracle cache (commit this!)
```

Minimal `sqletch.yaml`:

```yaml
version: 1
dialect: postgres          # postgres | mysql | sqlite
server_version: "16"
database:
  # dsn: postgres://…      # optional, LITERAL (no ${VAR}); empty = auto-managed disposable DB
schema:
  files: [db/schema.sql]
targets:                   # one entry per generated package
  - queries: [queries/*.sql]
    output:
      package: gen
      path: gen
```

The dev database is **disposable by contract**: sqletch resets it and
applies your schema on every cold run. With an empty `dsn`, PostgreSQL
and MySQL start a temporary container (Docker); **SQLite needs nothing
at all** — the real engine runs inside the compiler.

## A first template

```sql
-- name: SearchUsers :many
SELECT u.id, u.email, u.status
FROM users AS u
WHERE TRUE

@if-present(status)
  AND u.status = :status
@endif

@if-present(email_prefix)
  AND u.email LIKE :email_prefix || '%'
@endif

ORDER BY u.id
LIMIT :limit;
```

`@if-present(status)` means: this conjunct exists exactly when the
caller provides `status`. sqletch verifies every combination — with
and without each fragment — by preparing and planning it on the dev
database, then generates:

```go
users, err := q.SearchUsers(ctx, gen.SearchUsersParams{
    Status: sqletch.Present("active"), // the zero value omits the fragment
    Limit:  50,
})
```

Optional parameters are `sqletch.Omittable[T]` values
(`github.com/moznion/go-sqletch`); the zero value removes their
fragments. SQL `NULL` is spelled with
[go-optional](https://github.com/moznion/go-optional)'s `Option[T]` —
nullable result columns, and parameters written into nullable
columns. The
SQL sent for each combination is byte-for-byte one of the shapes that
were verified at compile time — values only ever travel as bind
parameters.

## The loop

```console
$ sqletch generate     # verify everything, emit gen/
$ sqletch check        # verify only (CI)
$ sqletch fmt          # canonicalize construct layout, insert anchors
$ sqletch explain      # per-query guards, shapes, types
```

## The committed cache and CI

The first `generate` needs the database; it writes every oracle answer
into `.sqletch/cache/`. **Commit that directory.** From then on,
`check` and `generate` run fully offline until a query, the schema,
the dialect, or the pinned `server_version` changes — so CI needs no
database for unchanged queries:

```console
$ sqletch check
sqletch: 6 queries ok (oracle cache: 10 hits, 0 misses; offline: yes)
```

The tree is meant to be read in review, so every file is named after
what it is — one entry per verified rendering of a query, under the
target that generates it:

```
.sqletch/cache/
  catalog.json                                 the schema snapshot
  env.json                                     which server produced it
  oracle/gen/SearchUsers/maximal.json          one file per rendering
  oracle/gen/SearchUsers/case-sort-email_asc.json
  oracle/gen/ListUsers/order-default-sort.json
```

Editing a query changes the files of the shapes it affects, in place;
a new shape is one new file whose name says which construct created
it. `generate` also **removes cache files it did not write** — an
entry for a deleted query or a superseded schema would otherwise stay
forever — so what you commit is exactly what the current templates and
schema need. (`check` never deletes anything.)

The cache is an optimization, never a source of truth: entries are
keyed by the full rendered SQL plus a schema fingerprint, verified on
read, and safe to delete at any time.

## Where to next

- The construct vocabulary: [template language](02-template-language.md)
- Type annotations for MySQL/SQLite: [annotations](03-annotations.md)
- Dialect specifics: [dialects](04-dialects.md)
- Editor setup (LSP + highlighting): [editors](09-editors.md)
- Coming from sqlc: [migration guide](10-sqlc-migration.md)
