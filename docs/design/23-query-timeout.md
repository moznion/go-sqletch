# sqletch Design — 23: Per-query timeouts

Status: ACCEPTED — owner decisions D1–D3 and the driver-error stance
(§5, §5.1) settled 2026-10-02;
implemented. D4–D7 are the implementation choices made within them,
recorded here for review.

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
| none | absent | none (byte-identical to pre-23 output) |
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
  emits nothing new, so projects that use neither knob regenerate
  byte-identically (verified on all three examples).
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

Generated code returns the driver's error **unchanged** (owner
decision 2026-10-02). What a caller sees on an expired deadline (or a
cancelled context) is therefore the driver's own behavior, documented
and pinned per driver (pgx v5.11.0, go-sql-driver/mysql v1.10.1,
ncruces/go-sqlite3 v0.35.6):

| Driver / configuration | Error on expiry | Connection afterwards |
|---|---|---|
| pgx, default `DeadlineContextWatcherHandler` | `errors.Is(err, context.DeadlineExceeded)` (`context.Canceled` on cancel) | **closed** — a single `*pgx.Conn` is unusable afterwards; `pgxpool` replaces it |
| pgx with `CancelRequestContextWatcherHandler` (below) | `*pgconn.PgError` SQLSTATE **57014** (query_canceled) — NOT `DeadlineExceeded` | **survives**; the next query on it succeeds |
| go-sql-driver/mysql (database/sql) | `errors.Is(err, context.DeadlineExceeded)` (`context.Canceled` on cancel) | managed by database/sql's pool (not pinned) |
| ncruces/go-sqlite3 (database/sql) | `sqlite3.INTERRUPT` — NOT `DeadlineExceeded`/`Canceled` | managed by database/sql's pool (not pinned) |

The pgx opt-in keeps connections alive by having the server cancel
the statement instead of closing the socket:

```go
cfg.BuildContextWatcherHandler = func(pc *pgconn.PgConn) ctxwatch.Handler {
	return &pgconn.CancelRequestContextWatcherHandler{Conn: pc, DeadlineDelay: time.Second}
}
```

Verified on PG 16 + pgx v5.11.0: about 300 ms to return for a 200 ms
deadline, three consecutive timeouts on one connection, then the next
query OK. Caveats: the cancel is an extra round-trip on a separate TCP
connection; if the server does not respond, the `DeadlineDelay`
fallback closes the connection after all; the cancel-request protocol
can race the statement finishing (pgx waits for the cancel to
complete, which mitigates it); connection poolers (PgBouncer, RDS
Proxy) must forward cancel requests; inside a transaction the
transaction is aborted either way.

**Portable check** across drivers: `ctx.Err() != nil` after a failed
call (or `errors.Is(err, context.DeadlineExceeded)` where the driver
wraps it — pgx default, MySQL).

`TestQueryTimeout{Postgres,MySQL,SQLite}` pin the error column of every
row (the connection column only for the cancel-request row): deadline and
caller-cancel errors per driver (SQLite asserts `sqlite3.INTERRUPT`
AND the absence of the context error, so a driver upgrade that starts
wrapping is noticed), the observer receiving the same error, and the
pgx cancel-request case on a single `*pgx.Conn` (57014, not closed,
next generated call succeeds).

### 5.1 Why no normalization — owner decision 2026-10-02

A `runtime.CtxErr` normalization (prefix `ctx.Err()` onto SQLite's
interrupt error in every SQLite method) was implemented and reverted
the same day:

- Design 17 keeps the **driver boundary deliberately unchanged**:
  generated code hands the driver what it always did and returns what
  the driver returns.
- Normalization **cannot be complete**: pgx with
  `CancelRequestContextWatcherHandler` returns `*PgError` 57014, not
  `DeadlineExceeded`, and other drivers/configurations have their own
  conventions; a half-normalized surface is worse than a documented
  one.
- Deciding from `ctx.Err()` (generated code cannot import a driver)
  would **misattribute an unrelated error** that races the deadline.

## 6. Not in scope

- Server-side timeouts (D2).
- Per-call overrides beyond the caller's own context (already
  possible: a shorter caller deadline wins; a LONGER one cannot extend
  the generated deadline — by design, the template author's bound is
  an upper limit).
- Timeouts as a verification input (e.g. "EXPLAIN cost must fit the
  deadline") — see the performance-lint discussion, separate design.
