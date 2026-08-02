# Examples

One self-contained sqletch project per dialect, each with its
committed oracle cache (so `sqletch check` runs offline):

- `postgres/` — the full showcase (guards, @choose, @when, HAVING,
  @order-by, @filter-tree, @in) with a runnable `main.go`
  (needs Docker or a DSN).
- `mysql/` — Tier 2 annotations, `@in` arity expansion, PATCH update;
  runnable via `SQLETCH_MYSQL_DSN` (cold generate needs Docker or a
  DSN).
- `sqlite/` — **zero setup**: `go run .` works with no server and no
  Docker; also shows `-- @column` for expression columns.

All three carry the same `@filter-tree!(scope)` query, so the required
mode is shown end to end per dialect: `And`/`Or` over the generated
predicate vocabulary, `ErrFilterRequired` for a forgotten filter, and
the explicit `FilterUsersUnscoped()` opt-out.

Regenerate any of them with
`go run ./cmd/sqletch generate --config examples/<dialect>/sqletch.yaml`.
