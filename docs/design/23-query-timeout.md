# sqletch Design — 23: Per-query timeouts

Status: ACCEPTED — owner decisions D1–D3 settled 2026-10-02, plus the
SQLite error normalization (§5.1, same day); implemented. D4–D7 are
the implementation choices made within them, recorded here for review.

This is a *runtime guardrail*, not a verification feature: nothing in
the verification model changes. A deadline bounds how long a generated
method lets one statement run; it does not make any statement faster
and proves nothing about plans.

## 1. Surface

```sql
-- name: AllAuditActions :many
-- @timeout 30s
SELECT a.action, count(*) AS occurrences FROM audit_logs AS a GROUP BY a.action;
```

```yaml
query_timeout:
  default: 5s     # every method whose query has no `-- @timeout`
```

| Template | Config default | Generated deadline |
|---|---|---|
| none | absent | none (byte-identical to pre-23 output, except SQLite's §5.1 error normalization) |
| none | `5s` | 5s |
| `-- @timeout 30s` | any | 30s |
| `-- @timeout none` | any | none |

## 2. Owner decisions (2026-10-02)

- **D1 — directive.** `-- @timeout <duration>` per query, Go
  `time.ParseDuration` syntax, buffered and attached exactly like
  `-- @param` (gap directives bind to the FOLLOWING query). Malformed,
  non-positive, or a second directive on one query → SQLETCH017
  (error).
- **D2 — mechanism: `context.WithTimeout` only.** No server-side
  setting (`SET LOCAL statement_timeout` needs a transaction;
  `MAX_EXECUTION_TIME` is SELECT-only; both would change the rendered
  SQL and so the verified shape space). The deadline covers execution
  AND row iteration/scan: the cancel is deferred until the method
  returns. A caller's shorter deadline wins by context semantics.
- **D3 — config default.** `query_timeout.default` in sqletch.yaml,
  global to the run like every non-target key; a directive overrides
  it; `-- @timeout none` opts out. Baked into generated code, NEVER in
  the cache fingerprint. Invalid value → SQLETCH318.

## 3. Implementation choices

- **D4 — placement.** The deadline is derived after composition and
  the reject branches (`ChooseOrdinal`/`OrderSeq`/filter-tree errors
  are not deadline-bound and still observe the caller's ctx) and right
  after the `OnQuery` hook, before the exec clock and the driver call:

  ```go
  q.hook(key, sqlText)
  // Deadline from `-- @timeout 30s` (design 23).
  ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
  defer cancel()
  var execStart time.Time
  ```

  `ctx` is reassigned, so every later use — the driver call and the
  doc-18 `ObserveExec` events — sees the derived context. A deadline
  failure is an ordinary exec error, observed like any other (the
  devdb suite counts one `ObserveExec` error per expired call).
- **D5 — literal spelling.** `N*time.Unit` with the largest unit
  (Hour … Nanosecond) that divides the duration exactly (`1.5s` →
  `1500*time.Millisecond`): readable, a pure function of the value
  (determinism), and plain pre-1.26 Go. The comment names the
  source (`-- @timeout 1.5s` or `query_timeout.default (5s)`).
- **D6 — no deadline, no bytes.** A method with no effective deadline
  emits nothing new, so PostgreSQL/MySQL projects that use neither
  knob regenerate byte-identically. SQLite is the exception since the
  §5.1 normalization (every method changes once).
- **D7 — config shape.** `query_timeout:` is a mapping with one key,
  `default`, leaving room for later knobs without a breaking rename.
  The value is a string (`*string` in Go, so an explicit `""` is told
  apart from an absent key and rejected); `none` is NOT accepted there
  — absence is how a config says "no default".

## 4. Directive text in the skeleton

Like every directive, the comment stays in the skeleton verbatim
(`TestScan_TimeoutStaysInSkeleton`): it is sent to the database and
it is part of the rendered SQL. Consequently adding or editing a
`-- @timeout` re-keys that query's oracle cache entries — one cold
`generate`, exactly as for `-- @param` and `-- @policy-apply`. Changing
`query_timeout.default` re-keys nothing (it is not in any rendering)
and only regenerates Go code.

Stripping directive comments from the skeleton would avoid the
re-keying, but it would make `-- @timeout` the one directive that does
not survive verbatim, and it would change composed SQL — out of scope.

## 5. Driver behavior on expiry (devdb-verified)

`TestQueryTimeout{Postgres,MySQL,SQLite}` run the real pipeline,
compile the generated package, and execute against the dev database:

| Driver | Error on expiry |
|---|---|
| pgx v5 | `errors.Is(err, context.DeadlineExceeded)` |
| go-sql-driver/mysql (database/sql) | `errors.Is(err, context.DeadlineExceeded)` |
| ncruces/go-sqlite3 (database/sql) | raw: `sqlite3.INTERRUPT` only — normalized by generated code, below |

With pgx, a single `*pgx.Conn` whose query is interrupted by its
context is closed by pgx; production code uses a pool (`pgxpool`), as
the suite does.

### 5.1 SQLite normalization — owner decision 2026-10-02

ncruces/go-sqlite3 stops the statement promptly (sqlite3_interrupt)
but reports SQLITE_INTERRUPT without wrapping the context's error, so
`errors.Is(err, context.DeadlineExceeded)` failed on SQLite only. The
owner decided to normalize it:

- **`runtime.CtxErr(ctx, err)`**: nil stays nil; when `ctx.Err() !=
  nil` and `err` does not already match it, the result is
  `fmt.Errorf("%w: %w", ctx.Err(), err)` — BOTH
  `errors.Is(err, context.DeadlineExceeded|Canceled)` AND
  `errors.Is(err, sqlite3.INTERRUPT)` hold; otherwise `err` is
  returned unchanged (identity), so drivers that already wrap the
  context error are never double-wrapped.
- **Condition = `ctx.Err()`, not the driver's error code**, because
  neither `runtime` nor generated code may import a driver. Known edge:
  an unrelated error racing the deadline also gets the context error
  prefixed; the original stays reachable via `errors.Is/As` and in the
  message.
- **Scope: every SQLite method** (`codegen.Options.NormalizeCtxErr`,
  set when `dialect: sqlite`), not only @timeout ones — a caller's own
  deadline or cancel hits the same INTERRUPT. Every driver-error site
  goes through one codegen funnel (`failExec`): Query/QueryRow/Exec,
  Scan, `RowsAffected`, plus the `:many` terminal `rows.Err()`
  (normalized inline). `:maybe-one`'s `sql.ErrNoRows` check runs
  BEFORE normalization and stays `(None, nil)`. Composition rejects
  are not driver errors and are untouched.
- **The observer sees the normalized error** — the variable is
  rewritten before `ObserveExec`, so metrics and the caller agree on
  the error class (pinned in the devdb suite).
- **Byte-identity (D6) no longer holds for SQLite**: every SQLite
  method gains the normalization lines whether or not a deadline is
  configured (accepted by the owner; examples/sqlite regenerated, its
  committed cache untouched). pgx and MySQL output is unchanged.

## 6. Not in scope

- Server-side timeouts (D2).
- Per-call overrides beyond the caller's own context (already
  possible: a shorter caller deadline wins; a LONGER one cannot extend
  the generated deadline — by design, the template author's bound is
  an upper limit).
- Timeouts as a verification input (e.g. "EXPLAIN cost must fit the
  deadline") — see the performance-lint discussion, separate design.
