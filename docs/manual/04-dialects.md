# Dialects

One dialect per generation target (`dialect:` in sqletch.yaml).
Templates use `:name` parameters everywhere; the compiler owns
placeholder emission per dialect. The soundness model is identical
across dialects — what differs is how much the database tells the
compiler for free, and the generated driver flavor.

| | PostgreSQL | MySQL | SQLite |
| --- | --- | --- | --- |
| Tier | 1 (oracle infers everything) | 2 (annotation-assisted) | 2 (annotation-assisted) |
| Dev database | container or DSN | container or DSN | **in-process; nothing external** |
| `server_version` pin | major (e.g. `"16"`) | major (e.g. `"8.4"`) | dotted prefix (e.g. `"3"`, `"3.50"`) |
| Parameter types | inferred (Describe) | `-- @param` mandatory | `-- @param` mandatory |
| Result types | wire protocol | wire protocol | declared types / affinity; `-- @column` for expressions |
| Placeholders | `$n` (shared params reuse a number) | `?` per occurrence | `?` per occurrence |
| `@in` | `= ANY($n)`, one shape | `IN (?, …)`, arity in the shape key | `IN (?, …)`, arity in the shape key |
| Generated driver | pgx v5 | database/sql | database/sql |
| Plan check | `EXPLAIN (GENERIC_PLAN)` | prepared `EXPLAIN`, all params NULL | prepare is the plan check |

## PostgreSQL

- Requires server 16+ (`EXPLAIN (GENERIC_PLAN)`).
- Generated code speaks pgx v5; `DBTX` matches sqlc's pgx flavor, so a
  `pgx.Conn`, `pgxpool.Pool`, or `pgx.Tx` — including one shared with
  sqlc-generated code — satisfies it.
- `numeric` maps to `float64` (documented lossy choice); cast in SQL
  for exact decimals.
- When the oracle cannot type a parameter (SQLETCH201), add a cast:
  `:v::text`.

## MySQL

- Grammar frontend is the TiDB parser; templates must parse under it.
- `database.dsn` uses the go-sql-driver format
  (`user:pass@tcp(host:port)/db`). Empty = testcontainers.
- `-- @param` on every bind parameter (SQLETCH311 names the missing
  one). `TEXT` vs `BLOB` and signedness map to `string`/`[]byte` and
  `int*`/`uint*` faithfully.
- The plan check prepares `EXPLAIN <query>` and executes it with all
  parameters NULL — EXPLAIN plans without touching data.
- Generated code uses `database/sql`; bring any MySQL driver
  (`go-sql-driver/mysql` is the tested one; add `parseTime=true` for
  `time.Time` scans).

## SQLite

- **No Docker, no server**: the oracle runs the real SQLite (compiled
  to WASM, executed in-process). `database.dsn` is a database file
  path, resolved relative to sqletch.yaml; empty = a temp file.
- The dev database file is disposable: sqletch drops every table and
  view before applying the schema.
- Prepare compiles through SQLite's planner, so prepare alone is the
  full validity check; errors carry exact byte offsets.
- Types follow the declared-type affinity rules with two deliberate
  carve-outs: `BOOLEAN` → `bool`, `DATE`/`DATETIME`/`TIMESTAMP` →
  `time.Time`. `NUMERIC`/`DECIMAL` → `float64` (lossy, as elsewhere).
  Expression columns need `-- @column` ([annotations](03-annotations.md)).
- Grammar frontend is rqlite/sql. Known gaps versus the newest SQLite
  grammar surface as parse diagnostics: `RIGHT`/`FULL JOIN` (3.39+)
  are unsupported, and a few non-reserved keywords (e.g. `ACTION`)
  must be quoted (`a."action"`) to use as identifiers.
- Generated code uses `database/sql`; the tested driver is
  `github.com/ncruces/go-sqlite3/driver` (pure Go), but any SQLite
  driver works.
