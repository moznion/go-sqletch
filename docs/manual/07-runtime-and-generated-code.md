# Generated code and the runtime

## The generated package

Per query file, sqletch emits `<query_name>.sql.go` plus shared
`db.go` / `querier.go`:

```go
q := gen.New(conn)                 // pgx conn/pool/tx, or *sql.DB / *sql.Tx
q = q.WithTx(tx)                   // share a transaction
q.OnQuery(func(shapeKey, sql string) { ... })  // observability hook

rows, err := q.SearchUsers(ctx, gen.SearchUsersParams{...})
```

- `DBTX` matches sqlc's interface for the same driver flavor
  (PostgreSQL → pgx v5; MySQL/SQLite → database/sql), so sqletch and
  sqlc code share connections and transactions.
- `Querier` is the all-queries interface for mocking.
- Params structs: required parameters are plain fields; `@if-present`
  parameters are pointers (`gen.Ptr(v)` helper; `nil` omits);
  `@choose` is an enum; `@order-by` a key-constant slice; `@in` a
  slice; `@filter-tree` a `*runtime.Tree`.
- Row structs: one field per result column; nullable columns are
  pointers (see nullability below).
- Errors before any SQL is sent: zero value of a required `@choose`
  (`runtime.ErrChooseRequired`), invalid `@order-by` selection
  (`runtime.ErrOrderKey`), `nil` required tree
  (`runtime.ErrFilterRequired`), oversized tree
  (`runtime.ErrTreeTooLarge`).

## What happens on a call

1. Guard bits, choose ordinals, order sequences, and `@in` arities are
   computed from the params struct → a **shape key**.
2. The composed SQL for that key comes from an in-process LRU
   (per-`Queries` value, 256 entries, full-key compared) or is
   composed by concatenating the pre-verified constant fragments —
   byte-identical to what the compiler verified, pinned by a
   conformance test.
3. Values are bound by position. Values never enter the SQL string.

With `static_expansion`, step 2 is a map lookup into precomposed SQL
(the `.sqletch/expanded/` files are the audit surface).

## Nullability

A row field is a pointer when the column can be NULL in **any** shape.
The analysis reads catalog NOT NULL, understands outer-join
null-extension, and deliberately **never narrows based on optional
fragments** — a guarded `INNER JOIN` does not make the FK non-null,
because other shapes lack the join. When you know better (e.g. an
application invariant), use `overrides` in sqletch.yaml rather than
editing generated code.

## The runtime package

`github.com/moznion/sqletch/runtime` has two faces (see its package
doc, "API contract (v1)"):

- **For you**: `Tree`, `And`/`Or`, the generated predicate
  constructors and `<Query>Unscoped()`, `TreeCaps`, and the sentinel
  errors above. Filter trees are values — build them in HTTP handlers
  or use-case layers and pass them down; the repository stays the only
  place that knows SQL.
- **For generated code**: fragment tables, composers, caches. Public
  only because your generated package lives outside sqletch's module.
  Don't construct these by hand; after upgrading sqletch, re-run
  `sqletch generate`.
