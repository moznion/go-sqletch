# Dialects

One dialect per generation target (`dialect:` in sqletch.yaml).
Templates use `:name` parameters everywhere; the compiler owns
placeholder emission per dialect. The soundness model is identical
across dialects — what differs is how much the database tells the
compiler for free, and the generated driver flavor.

| | PostgreSQL | MySQL | SQLite |
| --- | --- | --- | --- |
| Tier | 1 (oracle infers everything) | 2 (annotation-assisted) | 2 (annotation-assisted) |
| Dev database | container or DSN | container or DSN; **or none** (`database.oracle: native`) | **in-process; nothing external** |
| `server_version` pin | dotted prefix (e.g. `"16"`, `"16.4"`) | dotted prefix (e.g. `"8"`, `"8.4"`) | dotted prefix (e.g. `"3"`, `"3.50"`) |
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

### The native oracle backend (`database.oracle: native`)

MySQL is the one dialect with no embeddable real engine, so it is the
one dialect sqletch offers a **native-inference** oracle for: type
answers come from sqletch's own name resolution over a catalog built
from your schema DDL, and a cold `generate`/`check` needs **no Docker
and no DSN at all**. Every answer it gives is continuously proven
byte-identical to a real MySQL server's over a committed corpus — and
anything it cannot prove, it refuses:

```yaml
dialect: mysql
server_version: "8.4"        # the semantics being modeled (>= 8.0.19)
database:
  oracle: native             # no dsn — there is no server to point at
```

The discipline, relative to the server backend:

- **Schema files must be plain DDL**: `CREATE TABLE`, `DROP TABLE`,
  and `SET` statements only. `ALTER TABLE`, views, and generated
  columns are refused (`SQLETCH215`) — consolidate migrations into a
  dump, or use the server backend.
- **Expression result columns** (`count(*)`, arithmetic, `concat`)
  need an `AS` alias *and* a `-- @column alias: type` annotation
  (`SQLETCH214` otherwise). Direct column references are typed from
  the catalog with no annotation. `-- @param` stays mandatory exactly
  as on the server backend.
- **Subqueries, derived tables, and `ENUM`/`SET` projections are
  outside the modeled subset** and refused with `SQLETCH214`; the
  diagnostic names the rewrite or the escape hatch. Refusing more
  than the server is the design: the native backend never guesses.
- **`check --exhaustive` proves preparation, not planning**: there is
  no planner, so the `EXPLAIN` leg needs a server-backed run (the
  summary says so). `explain --analyze` likewise requires the server
  backend.
- A wrong `-- @column` annotation is reported (`SQLETCH216`) whenever
  an oracle answer exists to check it against; the oracle wins.

The cache is backend-agnostic: entries written natively and entries
written by a server are byte-identical for the same inputs, so teams
can mix backends (native on laptops and CI, a server run for the
EXPLAIN-grade exhaustive pass) against one committed cache.

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
