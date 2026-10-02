# Performance lints

sqletch can warn about query shapes that defeat an index or grow
without bound: a function wrapped around an indexed column, a `LIKE`
that starts with `%`, a `:many` query with no `LIMIT`, OFFSET paging,
and a parameter type that forces the database to convert the column.

These are **lints**, a separate axis from sqletch's soundness rules:

- they are **off by default** and run only when you enable them;
- every finding is a **warning** — a lint never fails `generate` or
  `check`, and never changes what is verified, cached, or generated;
- their codes have their own prefix, `SQLETCHLnnn`, so a lint is never
  confused with a rule violation (`SQLETCHnnn`).

## Why sqletch can lint better than a SQL linter

A template is not one SQL statement: every `@if-present`, `@when`,
`@choose`, and `@order-by` adds shapes, and a slow predicate may live
in a fragment only some calls reach. sqletch already materializes every
verification rendering of a query, so the lints read **every reachable
shape**, not just the ones a test happened to execute:

```sql
-- name: SearchUsers :many
SELECT u.id FROM users AS u
WHERE TRUE
  @if-present(email) AND lower(u.email) = :email @endif
ORDER BY u.id
LIMIT :limit;
```

```console
queries/users.sql:3:26: warning[SQLETCHL001]: the column is wrapped in a function or cast in this comparison: …
```

The guarded predicate is reported (at its place in the template) even
though the `email = None` shape never contains it. SQLETCHL005 also
uses what only sqletch knows at compile time: each column's catalog
type and each parameter's resolved type.

## Enabling lints

| How | Effect |
| --- | --- |
| `lint: true` in [sqletch.yaml](05-config.md) | lints run on every `generate` and `check`, and in the editor |
| `sqletch check --lint` / `sqletch generate --lint` | lints run for this invocation, whatever the config says |
| `sqletch check --lint=false` | lints are off for this invocation, whatever the config says |
| editor ([`sqletch lsp`](09-editors.md)) | follows `lint:` in sqletch.yaml (the LSP takes no flags) |

```yaml
# sqletch.yaml
lint: true
```

Turning lints on or off never changes rendered SQL, the committed cache,
or the cache fingerprint, so it never triggers a cold run. `-- @nolint`
directives are checked for typos (SQLETCH016) whether or not lints are
on: a template's validity does not depend on config.

## Reading the output

```console
$ sqletch check --lint
queries/users.sql:1:1: warning[SQLETCHL003]: this :many query can run without a LIMIT: its result (and the slice the generated method builds) grows with the table
  |
1 | -- name: ListUsers :many
  | ^^^^^^^^^^^^^^^^^^^^^^^^
help: add `LIMIT :limit` (keyset-paginate with an @if-present cursor), or `-- @nolint SQLETCHL003` for a result bounded by the data model
queries/users.sql:3:35: warning[SQLETCHL001]: the column is wrapped in a function or cast in this comparison: an index on the plain column cannot serve it, so the predicate is evaluated row by row
  |
3 | SELECT u.id FROM users AS u WHERE lower(u.email) = :email;
  |                                   ^^^^^^^^^^^^^^
help: compare the bare column and transform the parameter instead (e.g. `col >= :day_start AND col < :day_end`); if an expression index on exactly this expression exists, `-- @nolint SQLETCHL001`
sqletch: 1 queries ok (oracle cache: 1 hits, 0 misses; offline: yes)
```

Every finding points at template bytes you wrote, and its `help:` line
shows the rewrite. A finding that would land in text sqletch
synthesized (a [policy](12-policies.md)-woven conjunct) is dropped. A
pattern that appears in several shapes is reported once.

### In CI

The exit status ignores warnings (it is `0` above). To make lint
findings block a merge, read the JSON diagnostics (`--json` writes one
object per line to stderr; the summary line stays on stdout):

```sh
sqletch check --lint --json 2> lint.jsonl
! grep -q '"code":"SQLETCHL' lint.jsonl
```

## Suppressing a finding

When a finding is a deliberate choice, say so in the query:

```sql
-- name: CountByStatus :many
-- @nolint SQLETCHL003 (one row per status)
SELECT status, count(*) AS n FROM users GROUP BY status;
```

- The directive suppresses the named codes for this query, in every
  shape. Several codes: `-- @nolint SQLETCHL001, SQLETCHL003 (…)`.
- Codes are mandatory, and only `SQLETCHL001`–`SQLETCHL005` may be
  named; anything else (a rule code, an unknown code, a malformed
  directive, golangci-style `@nolint:CODE`) is the error SQLETCH016.
- An `@nolint` whose lint no longer fires is itself a warning
  (SQLETCHL006). Delete it, so the lint can catch the next regression.
- The reason is optional but is what a reviewer reads.

Full syntax: [`-- @nolint`](03-annotations.md#---nolint-code-code-reason).

**Fix or suppress?** Prefer the rewrite when the query really is on a
hot path. Suppress when the data model bounds the cost (a lookup table,
a GROUP BY over a small vocabulary, an admin export) or when an index
you created matches the expression exactly (sqletch does not read
index definitions).

## The lints

All lints are whitelists: each flags only the unambiguous form and
stays silent when unsure. A missed warning costs nothing the database
was not already charging; a noisy one would train authors to sprinkle
`@nolint`.

### SQLETCHL001 — function or cast on the column

**Flags** a comparison in `WHERE` or `JOIN … ON` (including a
subquery's own `WHERE`) where one side wraps a single column in a
function or cast and the other side contains no column:

```sql
WHERE lower(u.email) = :email            -- flagged
WHERE u.created_at::date = :day          -- flagged (PostgreSQL)
WHERE CAST(u.id AS text) = :id           -- flagged
WHERE date(u.created_at) BETWEEN :a AND :b
```

**Why it is slow.** A B-tree index on `email` stores `email`, not
`lower(email)`; the database must compute the function for every row.

**Rewrite.** Transform the parameter, not the column, or compare a
range:

```sql
WHERE u.email = :email                   -- normalize in Go before binding
WHERE u.created_at >= :day_start AND u.created_at < :day_end
```

If an expression index on exactly that expression exists
(`CREATE INDEX … ON users (lower(email))`), `@nolint SQLETCHL001`.

**Not flagged:** both sides referencing columns (`lower(a.x) =
lower(b.y)`), arithmetic on the column (`u.id + 1 = :id`), negated
forms (`NOT LIKE`, `NOT IN`), anything inside `CASE`, `HAVING`, and an
aggregate's `FILTER (WHERE …)` — the last two filter rows already
fetched or grouped, where no index applies.

**Dialects:** all three.

### SQLETCHL002 — LIKE starting with a wildcard

**Flags** `col LIKE p` / `col ILIKE p` where the pattern provably
starts with `%` or `_`:

```sql
WHERE u.email LIKE '%' || :q             -- flagged
WHERE u.email LIKE '%example.com'        -- flagged
WHERE u.email LIKE CONCAT('%', :q)       -- flagged (MySQL)
```

**Why it is slow.** An index can only seek to a known prefix; a pattern
with no prefix matches every row.

**Rewrite.** Anchor the pattern (`u.email LIKE :q || '%'`), or use a
full-text / trigram index built for substring search and
`@nolint SQLETCHL002`.

**Not flagged:** a bare `:q` pattern (its content is unknown at compile
time), `NOT LIKE`, a dollar-quoted pattern (`$$…$$`), and a non-column
left side.

**Dialects:** all three.

### SQLETCHL003 — `:many` without LIMIT

**Flags** a `:many` SELECT where some reachable shape has no `LIMIT`
(other than `LIMIT ALL`) or `FETCH FIRST`/`NEXT`. The warning points at
the query header.

```sql
-- name: ListUsers :many                 -- flagged
SELECT u.id FROM users AS u;

-- name: ClaimJobs :many                 -- flagged: row-locking, unbounded
SELECT id FROM jobs WHERE state = 'queued' FOR UPDATE SKIP LOCKED;
```

**Why it is slow.** The result — and the slice the generated method
allocates — grows with the table, and a locking SELECT locks all of it.

**Rewrite.** Page by key with a cursor parameter:

```sql
-- name: ListUsers :many
SELECT u.id FROM users AS u
WHERE TRUE
  @if-present(after_id) AND u.id > :after_id @endif
ORDER BY u.id
LIMIT :limit;
```

When the data model bounds the result (one row per status, per
enum value, per tenant setting), `@nolint SQLETCHL003` with that reason.

**Not flagged:** `:one` and `:maybe-one`; `INSERT`/`UPDATE`/`DELETE …
RETURNING` (bounded by what they modify). A `LIMIT` only inside a
subquery does not bound the outer result, so it does not count.

**Dialects:** all three.

### SQLETCHL004 — OFFSET pagination

**Flags** an `OFFSET` (or MySQL/SQLite `LIMIT offset, count`) whose
offset comes from a parameter:

```sql
LIMIT :limit OFFSET :offset              -- flagged
LIMIT 20 OFFSET (:page - 1) * 20         -- flagged
LIMIT :off, :n                           -- flagged (MySQL/SQLite)
```

**Why it is slow.** The database produces and discards every skipped
row, so page *n* costs *n* times page 1.

**Rewrite.** Keyset pagination, as in the SQLETCHL003 example: pass the
last key you saw instead of a row count.

**Not flagged:** a constant offset (`OFFSET 1`, `OFFSET 10 * 2`), a
subquery offset, and a column, table, or alias that happens to be named
`offset` (a non-reserved word on MySQL/SQLite).

**Dialects:** all three.

### SQLETCHL005 — parameter type converts the column

**Flags** a top-level `WHERE`/`ON` comparison between a bare column and
a parameter whose type makes the engine convert the **column** on every
row:

| Dialect | Column | Parameter | Example |
| --- | --- | --- | --- |
| PostgreSQL | `int2` / `int4` / `int8` | `numeric`, `float4`, `float8` | `WHERE u.id = :p::numeric` |
| MySQL | `CHAR` / `VARCHAR` / `TEXT` / `BLOB` | integer, float/double, decimal | `WHERE u.code = :p` with `-- @param p: bigint` |
| SQLite | — | — | never flagged: a bound parameter takes the column's affinity |

**Why it is slow.** PostgreSQL has no integer-vs-numeric index
operator, so it compares `id::numeric`; MySQL compares a string column
with a number as floating point. Either way the column's index is
skipped.

**Rewrite.** Bind the parameter at the column's type: drop the cast, or
fix the `-- @param` annotation.

**Not flagged:** pairs that stay inside one index family (int4 vs int8,
float4 vs float8, date vs timestamp, varchar vs text), and MySQL's
reverse direction (a numeric column vs a string value converts the
value). On PostgreSQL a cast on the parameter is judged at the cast's
type (`id = :v::int4` is safe even if `:v` is numeric elsewhere).
Subqueries and set operations (`UNION` …) are not inspected, nor is a
column of a `WITH` query that shares a table's name (its types are not
the table's).

**When it runs.** This lint needs column and parameter types, so it
runs in the catalog-dependent pass: on every `generate`/`check`, and in
the editor only when the committed cache holds the catalog and an
entry for every shape of the query (otherwise the editor shows the
other lints only).

### SQLETCHL006 — `@nolint` that suppresses nothing

**Flags** an `@nolint` naming a lint that does not fire on the query.
Remove the code (or the directive). It cannot itself be suppressed: a
stale suppression must not silence its own staleness report.

## Known limits

- Detection is lexical, per dialect. A non-reserved keyword used as a
  column name (`group`, `offset`, …) can end a predicate early; the
  result is a missed warning, never a wrong one.
- Index definitions are not read, so an expression index that serves
  `lower(email)` still draws SQLETCHL001 — suppress it with a reason.
- The editor grammars do not highlight `@nolint` specially; it renders
  as a comment.
