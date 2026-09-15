# sqletch Design — 20: Nullable Value Parameters (NULL vs Omission)

Status: **accepted** (2026-09-15; all doc-20 questions settled, not yet
implemented). Amends design 17 §1 surface 1 and
the spec's "@if-present", R9, Use Case 2, Generated API Conventions,
and Design Boundary sections. Breaking change to generated code (v0.x;
`docs/manual/11-compatibility.md` withholds the promise until v1.0.0).

## 1. Problem

A required parameter is a plain `T` field whose value is always bound
(`internal/codegen/generate.go`, the `default:` bind arm). Writing SQL
`NULL` into a nullable column is therefore impossible without a
guard, and the guard only *omits* the item:

- `INSERT` with `@if-present(x)` omits the column/value pair, so the
  column receives its `DEFAULT` — `NULL` only when the column has no
  default (or a `NULL` default).
- `UPDATE … SET` with `@if-present(x)` omits the item, so the column
  keeps its current value. Clearing it to `NULL` is inexpressible.

Two distinct caller intents — "leave this column out" and "write NULL
into this column" — are today both spelled `optional.Option[T]`'s
`None`, and only the first is available.

## 2. Decision (owner, 2026-09-15)

**Each intent gets its own Go type**, so the template decides the
type and a template edit that changes the meaning also changes the
type (compile-visible):

| Intent | Go type | Zero value means |
|---|---|---|
| SQL value that may be `NULL` | `optional.Option[T]` | bind `NULL` |
| presence guard (`@if-present`) | `sqletch.Omittable[T]` | omit the guarded fragment(s) |

This is "split 2" of the design discussion: `Option[T]` keeps meaning
*SQL NULL* everywhere it already does on the result side (nullable
row columns, design 05/17), so a row read with a nullable column can
be written back without conversion. Omission is a query-*shape*
concept, not a SQL value, so it gets a sqletch type in the new
module-root package `sqletch` (import path
`github.com/moznion/go-sqletch`; see §4.1).

Rejected alternative ("split 1"): keep `Option[T]` for presence and
introduce `sql.Null[T]` for NULL. Less breaking, but `Option[T]` would
mean NULL on rows and "omit" on params, and a row → params round trip
would need conversion.

Design 17 §1's "absence is uniformly `Option[T]`" is amended
accordingly: nullable result columns, `:maybe-one`, and nullable value
params use `Option[T]`; presence guards use `Omittable[T]`. Still no
pointer fallback and no config knob.

## 3. Deriving nullability from the catalog

A parameter is **nullable** when every one of its bind occurrences is
a *direct value position* of a nullable base-table column. No
annotation is required; the catalog snapshot (`cache.Column.NotNull`)
already holds the fact, offline, and the R7 companion warning
(`rules/resolved.go`, SQLETCH212)
already consults it the same way.

### 3.1 Direct value positions (v1)

- An `INSERT … (col, …) VALUES (…)` item whose entire expression is
  the placeholder, paired positionally with an **explicit** column
  list. Every row of a multi-row `VALUES` counts separately.
- A statement-level `UPDATE … SET col = <placeholder>` item (single
  column form).

"The placeholder" is looked for through any nesting of **explicit
casts** and parentheses (owner decision 2026-09-15, Q4): a cast of
`NULL` is `NULL` on every supported engine, so wrapping does not change
what `None` writes. This matters because sqletch itself steers authors
to casts — an unmappable type (e.g. a PostgreSQL enum column) is told
to "add an explicit cast to a supported type" (`codegen/generate.go`),
giving `:status::text::user_status`, and type disagreement is told to
pin with `:x::type` (`rules/types.go`). Only the parser's cast node
counts:

- PostgreSQL: `TypeCast` (both `::T` and `CAST(… AS T)`);
- MySQL (TiDB, server and native): `FuncCastExpr` — `CAST(… AS T)`,
  `CONVERT(…, T)`, `BINARY …`;
- SQLite (rqlite/sql): `CastExpr`.

`CONVERT(… USING charset)`, `COALESCE`, and every other function call
remain expressions.

Everything else keeps the parameter a plain `T`:

- expressions (`lower(:x)`, `:x || 'a'`, `COALESCE(:x, …)`,
  `CONVERT(:x USING utf8mb4)`);
- `INSERT … SELECT`, an `INSERT` without a column list;
- conflict arms (`ON CONFLICT … DO UPDATE SET`, `ON DUPLICATE KEY
  UPDATE`) — but see the note below — tuple `SET (a, b) = (…)`,
  MySQL `INSERT … SET`;
- `WHERE`, `HAVING`, `RETURNING`, subqueries, CTE bodies;
- any statement kind other than `INSERT`/`UPDATE`.

Upserts need no dedicated handling. The idiomatic arm references the
proposed row (`DO UPDATE SET col = EXCLUDED.col`; MySQL `new.col` row
alias or `VALUES(col)`), so the parameter occurs only in `VALUES` and
derives normally — `None` writes `NULL` on both the insert and the
update path. An arm repeating the placeholder (`SET col = :x`) is not a
direct value position, so by the every-occurrence rule (§3.4) the
parameter stays `T`: today's behavior, never the unsafe direction.
Excluding the arm is a definitional line, not a soundness requirement
— the arm writes the same table and column as the `INSERT`, so a
facade that over-reported it would still derive correctly. Facade
tests pin the exclusion so all dialects agree.

### 3.2 Parameter eligibility

Derivation considers only parameters that are:

- author-written (`Param.Policy == ""`) — a policy scoping value is
  never nullable; it is additionally unreachable by rule because its
  woven occurrence sits in `WHERE`/`ON`;
- not `@filter-tree` predicate arguments, not `@in` lists, not `@when`
  control parameters (typed by literal);
- **fully visible in the maximal rendering**: an occurrence inside a
  non-representative `@choose` case or an `@order-by` key is not in
  `rs[0]`, so such a parameter stays `T` (conservative; no
  per-rendering union in v1).

### 3.3 Column resolution

The target relation is `Tree.Relations()[0]` for `INSERT`/`UPDATE`,
resolved through the catalog with the same folding as R2/R3/R7 (F3:
case-insensitive dialects must not miss a mixed-case column). If the
relation or column does not resolve, the parameter stays `T`.

A target whose catalog entry has `IsView` (PostgreSQL views and
materialized views, MySQL server-oracle views, SQLite views) also
stays `T`: a view column's catalog `NotNull` does not carry its base
column's constraint (PostgreSQL `attnotnull` is always false on a
view), so it cannot vouch for nullability. The native MySQL oracle
refuses `CREATE VIEW` (SQLETCH215), so it has no views to exclude.
No catalog change is needed — every dialect already records
`IsView` (PostgreSQL/MySQL since a532d7c).

### 3.4 Placement in the pipeline

A new catalog-dependent pass `rules.DeriveNullableParams(q, rs[0],
tree, cat) map[string]bool` runs inside `cli.resolvedChecks`, so
`pipeline.Run` and the LSP's `OfflineChecker` share it (the
"extend it, don't fork it" rule). Its result reaches codegen as
`QueryInput.NullableParams` and `explain` as a param annotation.

The dialect facade gains one method:

```go
// ValueTargets reports every direct value position (design 20 §3.1)
// of a statement-level INSERT VALUES item or UPDATE SET item: the
// target column name and the byte offset of the placeholder in the
// parsed SQL, looking through explicit casts and parentheses.
// Anything not listed is, by definition, not a direct value position
// — facades under-report rather than over-report.
ValueTargets() []ValueTarget
```

Offsets map to template parameter occurrences through the rendering's
`ast.SourceMap` (placeholders are synthesized segments anchored at
their parameter). A parameter is nullable iff **every** occurrence
maps to a listed target whose column is nullable.

Implemented for all four frontends: PostgreSQL (pg_query), MySQL
(TiDB parser, server and native oracle), SQLite (rqlite/sql).

## 4. Generated API

The two properties compose — a parameter can be both a presence
guard and a nullable value:

| guard (`Optional`) | nullable | field type | bind expression |
|---|---|---|---|
| no | no | `T` | `arg.X` |
| no | yes | `optional.Option[T]` | `arg.X.UnwrapAsPtr()` |
| yes | no | `sqletch.Omittable[T]` | `arg.X.Ptr()` |
| yes | yes | `sqletch.Omittable[optional.Option[T]]` | `arg.X.OrZero().UnwrapAsPtr()` |

Presence compiles to `arg.X.IsPresent()` (was `arg.X.IsSome()`).
The driver boundary is unchanged from design 17 §2: every slot binds
a `*T` (nil for NULL), and composition selects a guarded slot only in
shapes where its guard is on.

The nested form is exactly PATCH tri-state:

```go
// UPDATE users SET updated_at = now()
// @if-present(nickname) , nickname = :nickname @endif   -- nullable column
Nickname sqletch.Omittable[optional.Option[string]]

// zero value                              → column untouched
// sqletch.Present(optional.None[string]()) → SET nickname = NULL
// sqletch.Present(optional.Some("neo"))    → SET nickname = 'neo'
```

Field comments distinguish the meanings: `// omitted: fragment(s)
left out` for `Omittable`, `// None binds NULL` for a nullable value.

### 4.1 `sqletch.Omittable[T]`

```go
// Omittable is a presence-guard value: the zero value omits every
// fragment the parameter guards.
type Omittable[T any] struct {
	v  T
	ok bool
}

func Present[T any](v T) Omittable[T]
func (o Omittable[T]) IsPresent() bool
func (o Omittable[T]) Get() (T, bool)
func (o Omittable[T]) OrZero() T
func (o Omittable[T]) Ptr() *T // nil when omitted
```

`sqletch` stays free of a go-optional dependency (design 17 §5): the
nested form instantiates `T = optional.Option[U]` in generated code
only. The type lives in a new module-root package rather than
`runtime/` (owner decision 2026-09-15): it appears at every optional
call site, where `runtime.Present(v)` reads poorly and `runtime`
shadows the standard library name. `runtime` never touches the type
(the bind expression is `arg.X.Ptr()`), so the root package imports
nothing from `runtime`. The `go-` prefix / `sqletch` package-name
split follows go-optional, so generated code imports it without an
alias. Known cost: user-facing API is split — `sqletch.Omittable`
next to `runtime.Tree` and the `runtime.Err*` sentinels (see Q7).

Plain generics (go1.18) respect the generated-code language level
policy.

## 5. Soundness and failure direction

Nothing here changes renderings, fragments, shape keys, bind order, or
oracle inputs: `TestComposeConformance`, the cache fingerprint, and
the corpus byte-identity gates are untouched. The derivation only
selects a Go type.

- **False nullable** (a column the catalog calls nullable rejects
  NULL: domain `NOT NULL`, `CHECK (col IS NOT NULL)`, trigger,
  updatable view over a `NOT NULL` column): passing `None` fails at
  execution with a constraint error — loud.
- **False non-nullable** (facade under-reports a position): the
  parameter stays `T` — today's behavior, an ergonomic gap only.

A zero-valued params struct now writes `NULL` (not `""`/`0`) into a
derived column. Both are silent zero-value writes; this adds no new
class.

## 6. Breaking changes and migration

1. Every `@if-present` parameter: `optional.Option[T]` →
   `sqletch.Omittable[T]`; callers replace `optional.Some(v)` with
   `sqletch.Present(v)`.
2. Every unguarded parameter in a direct value position of a nullable
   column: `T` → `optional.Option[T]`.
3. Documentation: spec (@if-present, R9, Use Case 2, Generated API
   Conventions, Design Boundary "NULL-as-value"), design 17 §1,
   manual 01/02/07/10, README; regenerate `examples/*/gen`.
4. `explain` param listing: `(optional)` → `(omittable)`, plus
   `(nullable)`.

## 7. Tests (written first)

- **rules**: derivation table including every §3.1/§3.2 exclusion
  (co-occurrence in `WHERE`, `RETURNING`, conflict arm, `INSERT …
  SELECT`, missing column list, unresolved table/column, mixed-case
  folding on MySQL/SQLite, quoted identifiers, policy-woven param,
  `@choose` case, `@in`, `@when` control).
- **facades** (pg / mysql server / mysql native / sqlite):
  `ValueTargets` over multi-row `VALUES`, `DEFAULT` items, tuple
  `SET`, `INSERT … SET`, upsert arms, `INSERT OR REPLACE`,
  parenthesized placeholders, single and nested casts (`:x::text::e`,
  `CAST(CAST(? AS CHAR) AS JSON)`, `BINARY ?`), and cast look-alikes
  that must NOT count (`CONVERT(? USING …)`, `COALESCE(?, …)`); a node-kind diff against each parser's
  AST (audit-13 lesson) so a new statement form cannot be silently
  over-reported.
- **codegen**: golden emission for all four §4 rows and the presence
  check; conformance test untouched.
- **sqletch (root package)**: `Omittable` zero value, `Present`, `Ptr`, `OrZero`.
- **devdb E2E** (all three dialects, adversarial seed): nullable
  `INSERT` with `None` reads back NULL; a nullable column with a
  non-NULL `DEFAULT` gets the default when omitted and NULL when
  `Present(None)`; PATCH tri-state round trip; `NOT NULL` domain
  column gives a loud constraint error.
- **LSP/explain**: offline derivation from the committed catalog.

## 8. Owner questions (settled 2026-09-15)

- ~~Q1 naming~~ — resolved 2026-09-15: `sqletch.Omittable` /
  `sqletch.Present` in a new module-root package (§4.1). The method set
  in §4.1 stays open to review during implementation.
- ~~Q2 nesting~~ — resolved 2026-09-15: guarded + nullable always
  nests (`Omittable[Option[T]]`, §4).
- ~~Q3 views~~ — resolved 2026-09-15: writes into views stay `T`
  (§3.3); the catalog already marks views on every dialect.
- ~~Q4 casts~~ — resolved 2026-09-15: explicit casts (and parentheses)
  are looked through (§3.1).
- ~~Q5 JSON~~ — resolved 2026-09-15: not supported; `Omittable` has no
  JSON methods and the `sqletch` package does not import
  `encoding/json`.
- ~~Q6 conflict arms~~ — resolved 2026-09-15: no dedicated support
  (§3.1 note); the `EXCLUDED.col` idiom already derives, a repeated
  placeholder safely stays `T`.
- **Q7 user-API home** (separate design, not part of doc 20): make
  `sqletch` the user-facing package — alias `Tree`/`TreeCaps`, re-export
  `And`/`Or`/`Unscoped`, the `Err*` sentinels (same values, so
  `errors.Is` holds), and the observability types — narrowing
  `runtime` to the generated-code contract.
