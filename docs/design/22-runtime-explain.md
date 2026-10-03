# sqletch Design — 22: Runtime EXPLAIN (`Explain<Query>`)

Status: **implemented** (2026-10-02). Adds to design 06 (generated API)
and the runtime package's USER API. Additive: no existing generated
signature changes (one `argIdent` reservation, §6).

## 1. Problem

`sqletch explain --analyze` plans every shape against the *dev*
database with all parameters NULL. What it cannot show is the plan of
the statement an application actually sends, with the application's
values, against a real (staging, production-like) database — the plan
that matters when a query is slow. Today a user reproduces that by
copying `OnQuery`'s SQL by hand, re-deriving the bind values, and
prefixing `EXPLAIN` themselves.

## 2. Decisions (owner, 2026-10-02)

| Question | Decision |
|---|---|
| Shape of the feature | **Explicit call API**: every query gets a generated `Explain<Query>` method that EXPLAINs instead of executing. (Not an auto-explain hook.) |
| `ANALYZE` | **Supported**; statements run inside a transaction (or savepoint) that is **always rolled back**. |
| Output | **Text or JSON**, a closed choice. |
| Generation | **Always emitted** (no config knob, no build tag). |

## 3. Generated API

For a query `SearchUsers` the generator emits, next to the query method:

```go
func (q *Queries) ExplainSearchUsers(ctx context.Context, /* required args… */
	arg SearchUsersParams, opts runtime.ExplainOptions) (runtime.Plan, error)
```

The parameter list is the query method's, plus a trailing `opts`.
Required arguments (policy-woven parameters, `@filter-tree!`) stay
required: an explain of a policy-scoped query is an explain of the
scoped statement.

`Explain<Query>` methods are **not** part of `Querier`: `Querier`
exists for mocking, and adding methods to it would break every
existing mock implementation. They are a diagnostic surface.

### 3.1 Runtime types (USER API)

```go
type ExplainFormat uint8
const ( ExplainText ExplainFormat = iota; ExplainJSON )

type ExplainOptions struct {
	Analyze bool          // execute (inside a rolled-back tx) and report actuals
	Format  ExplainFormat // zero value = text
}

type Plan struct {
	Query     string        // query name
	ShapeKey  string        // canonical shape key (same as OnQuery's)
	SQL       string        // composed statement — byte-identical to what Query() sends
	Statement string        // what was sent: the EXPLAIN prefix + SQL
	Format    ExplainFormat
	Analyzed  bool
	Output    string        // the plan (text or a JSON document)
}

var ErrExplainUnsupported // option combination the dialect cannot express
var ErrExplainNoTx        // Analyze, but the DBTX cannot open a tx/savepoint
```

`runtime.ExplainStatement(dialect, opts, sql)` builds `Statement`; it,
`ExplainDialect` and `SQLitePlan` are part of the GENERATED-CODE
CONTRACT.

### 3.2 Codegen plumbing

`codegen.Options.Explain` (a `runtime.ExplainDialect`) selects the
db.gen.go back end; the CLI driver table sets it per dialect. Zero is
accepted only with `StyleDollar` (PostgreSQL) — MySQL and SQLite share
`StyleQuestion`, so a zero there is an internal error rather than a
guess — and a dialect that contradicts the style is refused likewise.

## 4. Soundness: the EXPLAIN statement is still constant composition

The statement sent is `prefix + SQL` where

- `SQL` comes from **the same generated preamble** the query method
  uses (codegen emits both from one writer, `writePreamble`): the
  same shape-key derivation, the same `@choose`/`@order-by`/
  `@filter-tree!` rejections, the same `q.cache` lookup, the same
  bind resolution. So `Plan.SQL` is byte-identical to the SQL the
  query method would execute for those arguments, and the bind list is
  identical too. A codegen test pins the textual identity of the two
  preambles per query kind.
- `prefix` is one of a **closed set of constants** in
  `runtime.ExplainStatement`, selected by `(dialect, Analyze, Format)`.
  There is no free-form option string: a caller cannot inject SQL
  through `ExplainOptions`. Invalid enum values are
  `ErrExplainUnsupported`.

So the runtime premise "deterministic composition of pre-verified
constant fragments, user values only through binds" is preserved. The
prefix is not oracle-verified per shape, but the oracle's plan check
already EXPLAINs every verified rendering (PG `GENERIC_PLAN`, MySQL
`EXPLAIN`, SQLite `EXPLAIN QUERY PLAN`), so the wrapped statement is
known to be explainable.

## 5. Dialects

| | Text | JSON | Analyze |
|---|---|---|---|
| PostgreSQL | `EXPLAIN ` | `EXPLAIN (FORMAT JSON) ` | `EXPLAIN (ANALYZE) ` / `EXPLAIN (ANALYZE, FORMAT JSON) ` |
| MySQL | `EXPLAIN FORMAT=TREE ` | `EXPLAIN FORMAT=JSON ` | `EXPLAIN ANALYZE ` / `EXPLAIN ANALYZE FORMAT=JSON ` |
| SQLite | `EXPLAIN QUERY PLAN ` | same, rows rendered as JSON | `ErrExplainUnsupported` (no such facility) |

- PostgreSQL output rows are joined with `\n` (text) or are the single
  JSON document. Real binds: no `GENERIC_PLAN` needed (that was for
  the param-less oracle).
- MySQL text uses `FORMAT=TREE`, matching `sqletch explain --analyze`.
  Measured on 8.4: single-table `UPDATE`/`DELETE` print
  `<not executable by iterator executor>` in TREE form — use JSON for
  those. `EXPLAIN ANALYZE FORMAT=JSON` is refused by 8.4's default
  `explain_json_format_version=1` (ERROR 1235); sqletch does not
  second-guess the server, the driver error is returned as-is.
- SQLite `EXPLAIN QUERY PLAN` rows `(id, parent, notused, detail)` are
  rendered by `runtime.SQLitePlan`: text = `detail` indented two spaces
  per tree depth (rows in engine order); JSON =
  `[{"id":…,"parent":…,"detail":…}]`. Deterministic for identical rows.

## 6. ANALYZE always runs inside a rolled-back transaction

The owner decision was "DML is rolled back". The implementation
**wraps every `Analyze` call**, not only DML: a `SELECT` can call a
volatile function with side effects, and statement-kind detection would
be one more thing to get wrong. The cost (a transaction around a
diagnostic call) is irrelevant.

- pgx (`DBTX` = Conn / Pool / Tx): the DBTX is asserted to
  `interface{ Begin(context.Context) (pgx.Tx, error) }` — Conn, Pool,
  pgxpool.Conn and Tx all satisfy it; on a `pgx.Tx` this is a
  savepoint. Rollback runs under `context.WithoutCancel(ctx)` so a
  cancelled ctx still rolls back; a rollback failure is reported when
  the explain itself succeeded.
- database/sql (`*sql.DB`, `*sql.Conn`): `BeginTx` + `Rollback`.
  `*sql.Tx`: `SAVEPOINT sqletch_explain` / `ROLLBACK TO SAVEPOINT` /
  `RELEASE SAVEPOINT`, so the caller's transaction stays usable.
- Any other DBTX: `ErrExplainNoTx` — never an un-wrapped ANALYZE.

Rollback does not undo non-transactional effects (sequence advances,
MyISAM tables, external side effects of functions). Documented.

### 6.1 The query's deadline applies (design 23)

`Explain<Query>` emits the same `-- @timeout` / `query_timeout.default`
deadline as the query method (`queryGen.writeTimeout`, one writer),
after the preamble and the `key.Trees` assignment and right before the
`q.explain` hand-off — so it bounds the transaction begin, the EXPLAIN
round trip and the row scan. With `Analyze` the statement really runs:
an ANALYZE must not be the one way to execute a deadline-bound query
without its deadline. It applies to plain EXPLAIN as well, for one
rule rather than a mode-dependent one. `@timeout none` (or no
effective deadline) emits nothing, as for the query method (design 23
D6). Expiry surfaces exactly as design 23 §5 tabulates per driver; the
rollback runs under `context.WithoutCancel`, so an expired deadline
still rolls back, and the expiry error (not a rollback error) is what
the caller sees.

## 7. Observability

`Explain<Query>` calls neither the `OnQuery` hook nor the observer's
exec/reject events: it is not an execution of the query. It does go
through the composed cache (that is what makes `Plan.SQL` identical),
so a cache observer sees its compose hit/miss like any lookup.

## 8. Name space

- A query `Foo` generates method `ExplainFoo`. A second query literally
  named `ExplainFoo` would declare the method twice: SQLETCH310
  (`CodeNameCollision`), pointed at the `ExplainFoo` query's header.
- The db.gen.go helpers `explain` and `explainTx` join
  `queriesMethodNames` (SQLETCH307 for a query of that name).
- `opts` joins `argIdent`'s reserved locals (a required argument named
  `opts` becomes `optsArg`, in both methods — the query method's
  signature changes only for that spelling).

## 9. Tests (written first)

- runtime: `ExplainStatement` table over dialect × Analyze × Format,
  invalid enums, SQLite analyze refusal; `SQLitePlan` text/JSON.
- codegen: methods emitted for every annotation and every preamble kind
  (plain, `@choose`/`@order-by`, filter-tree / filter-tree!, static
  expansion, question style); preamble textual identity with the query
  method; not in `Querier`; SQLETCH310 for `Foo` + `ExplainFoo`;
  `opts` reservation; generated packages compile.
- devdb E2E per dialect through the generated module: `Plan.SQL`
  equals the `OnQuery` SQL of the real call; text and JSON (valid JSON)
  outputs; PG ANALYZE of an `UPDATE` leaves the row unchanged, from a
  Conn and from inside a caller's Tx (which stays usable); MySQL
  ANALYZE from `*sql.DB` and `*sql.Tx`; SQLite ANALYZE →
  `ErrExplainUnsupported`. The deadline (§6.1) is pinned in codegen
  (`TestGenerate_ExplainTimeout`) and against real engines in
  `TestQueryTimeout{Postgres,MySQL}` (an ANALYZE of the slow statement
  expires with the driver's documented error).
