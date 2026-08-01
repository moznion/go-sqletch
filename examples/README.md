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

Regenerate any of them with
`go run ./cmd/sqletch generate --config examples/<dialect>/sqletch.yaml`.
