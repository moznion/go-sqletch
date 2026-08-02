# sqletch Specification — Statically Verified, Dynamically Composed SQL for Go

## Vision

Build a standalone compiler for a small, statically analyzable extension
of SQL that solves three needs of Go applications at once:

-   Write plain SQL to operate the database. SQL stays the source of truth.
-   Handle results and parameters with full type safety.
-   Construct queries dynamically (optional filters, partial updates,
    optional joins, selectable sort orders) without giving up static
    verification.

The key idea is to **separate static guarantees from dynamic assembly**:

-   Every fragment of SQL that can ever appear in a query is verified at
    compile time.
-   At runtime, the generated code only *selects and concatenates*
    compile-time-constant, pre-verified fragments. There is no runtime
    SQL parsing, no string interpolation of user data, and no free-form
    query builder API. SQL injection is impossible by construction.

One sentence positioning: **a query builder's everyday dynamism,
authored the sqlc way.** (Deliberately not "a query builder's *full*
dynamism": sqletch covers the closed-vocabulary 80% and explicitly
scopes out the rest; see Design Boundary.)

## Positioning

sqletch is **not** an sqlc frontend and does not invoke sqlc. Earlier
drafts of this project proposed enumerating every SQL variant and
feeding them to sqlc. That model breaks down combinatorially (N
independent conditions produce 2^N variants) and fights sqlc's
one-static-query-one-function model. Instead, sqletch adopts
*compositional verification* (see below), which makes full enumeration
unnecessary — and with it, the sqlc dependency.

sqletch remains **sqlc-compatible in spirit and in practice**:

-   Same authoring style: `-- name: QueryName :many` annotations in
    `.sql` files (multiple queries per file); optionally the same
    templates as `//sqletch:query` consts inside `.go` files.
-   Compatible generated-code conventions (`DBTX`, `Queries`, `WithTx`;
    see Generated API Conventions) so sqlc- and sqletch-generated code
    share a transaction naturally.
-   Designed to coexist with sqlc in the same repository: keep static
    queries in sqlc, move conditional queries to sqletch, or migrate
    incrementally.

Pipeline:

``` text
Template SQL (.sql files, or //sqletch:query consts in .go files;
              multiple queries per file)
    ↓  template scanner (construct layer; lexically dialect-aware)
Skeleton + guarded fragments (with source positions)
    ↓  dialect frontend parses the maximal rendering (+ per-case renderings)
Dialect AST + fragment↔AST source map
    ↓  structural rule check (R1–R9)
Verified fragment set
    ↓  type oracle (dialect driver; PREPARE/Describe on a dev DB, cached)
Parameter types + result column types + nullability
    ↓  code generation
Generated Go:
    one function, one params struct, one row struct per query
    runtime = deterministic composition of verified constant fragments
```

------------------------------------------------------------------------

# Core Insight: Compositional Verification

The combinatorial explosion in the naive design comes from treating
every optional block as an independent boolean and materializing the
full cross product. But most conditional SQL is **compositional**:

-   `WHERE TRUE AND p1 AND p2 … AND pn` — if each predicate `pi`
    type-checks on its own, *any subset* of the conjunction also
    type-checks. The same holds for `SET` items in an `UPDATE`, keys in
    an `ORDER BY` list, and — with positional pairing — column/value
    items in an `INSERT`. Optional items in list-shaped clauses never
    require cross-product verification: N fragments need N checks,
    not 2^N.
-   The constructs that break independence are those that change
    **scope** (a join introducing a relation that other fragments
    resolve into), **shape** (blocks that change the result columns or
    their nullability), **clause validity** (a required clause becoming
    empty, or an order-sensitive clause like `DISTINCT ON`'s `ORDER BY`
    being reordered), or **grouping** (a fragment that is not a single
    complete AST node, so deleting a neighbor regroups it).

sqletch therefore restricts the template language *structurally* so
that each kind of interference is impossible or explicitly localized.
The restrictions are not arbitrary ergonomics limits — each one exists
so that a compositional soundness argument holds. Under rules R1–R9
(below), verifying:

1.  the **maximal query** (all guards on — including mutually exclusive
    `@when` guards; over-approximation is deletion-sound — and one
    representative case per `@choose`), plus
2.  each remaining `@choose` case and each `@order-by` `@default` body
    substituted into the maximal query (per-case, not cross-product —
    sound because cases cannot interact, R8), plus
3.  the structural validity of the **minimal shape** (anchors R6,
    pairing R7),

is sufficient to guarantee that **every reachable query shape** parses,
resolves, and type-checks. Total verification work is
`O(fragments + choose cases)` — linear, regardless of how many shapes
are reachable. There is no variant cap because there is no enumeration.

This argument is a statement about the algebra of SQL clause lists
(conjunctions, item lists, closed choices), **not about any particular
database**. It ports to every SQL dialect; only the SQL grammar
frontend and the type oracle are dialect-specific (see Dialect
Architecture). The argument also rests on two runtime premises, stated
here because the implementation must uphold them:

-   **P1 — Pinned parameter types.** The runtime binds parameters with
    the compile-time-determined types through the dialect driver's
    typed-bind interface: explicit OIDs via pgx on PostgreSQL,
    binary-protocol types on MySQL, and Go-side conversion to the
    declared type before binding on SQLite (the database itself is
    dynamically typed there, so the pin happens in Go). Per-shape
    re-inference by the database never occurs.
-   **P2 — Verbatim fragments, fixed connectives.** Fragments are
    emitted byte-for-byte; `@order-by` keys follow the caller's
    sequence, everything else follows source order. The composer
    synthesizes **only** the fixed connective vocabulary: clause
    keywords (emitting or omitting `WHERE`/`ORDER BY`/…), list
    separators and boolean connectives (`AND`, `OR`, commas),
    mandatory parentheses around composed list items and
    `@filter-tree` predicates/subtrees, placeholder tokens (renumbered
    per shape), `ASC`/`DESC`, `@in`'s membership tokens
    (`= ANY(…)` / `IN (…)`), and the literals `TRUE`/`FALSE` for empty
    tree/list renderings. It never synthesizes identifiers,
    expressions, or any text derived from user values. Separator
    ownership: the scanner lifts the leading `AND`/comma off each
    list-item fragment (examples write it inside the block for
    readability); the remaining fragment body is the complete AST node
    R1 demands, and verification renderings and runtime composition
    wrap it in the same parentheses — **what is verified is byte-wise
    what is composed**. Fragments are joined with single newlines, so
    a trailing line comment can never swallow a neighboring fragment.

------------------------------------------------------------------------

# Goals

## Primary Goals

-   SQL remains the source of truth.
-   Support conditional SQL with static verification of every reachable
    query shape, at linear (not exponential) verification cost.
-   sqlc-quality type safety: typed parameter and row structs per query.
-   Zero runtime SQL parsing or string interpolation. Runtime work is
    limited to deterministic selection and concatenation of
    compile-time-constant fragments.
-   Deterministic output: identical inputs produce byte-identical SQL
    and Go code.
-   **Dialect-portable core.** The template language, structural rules,
    verification model, shape/composition runtime, and code generator
    are RDBMS-agnostic. Database specifics live behind a dialect driver
    interface. PostgreSQL is the first driver, not a design assumption.

## Non-Goals

-   General-purpose template engine.
-   Turing-complete language.
-   Free-form runtime query builder exposed to users. (The typed
    combinators generated for `@filter-tree` are the deliberate,
    bounded exception: they compose a *closed, compiled* predicate
    vocabulary and cannot express SQL that was not verified at compile
    time.)
-   ORM.
-   SQL DSL embedded in Go.
-   A lowest-common-denominator SQL. Templates are written in the
    dialect of the target database; sqletch does not translate SQL
    between dialects.

------------------------------------------------------------------------

# Design Principles

1.  SQL-first.
2.  Static where possible, dynamic where necessary — but dynamic means
    *composition of verified constants*, never construction.
3.  Restrictions must buy soundness. Every language restriction exists
    to make compositional verification valid, and is documented with
    its rationale.
4.  **Closed vocabularies.** Every dynamic choice ranges over a
    compile-time-closed set of verified fragments. Shape spaces are
    enumerable on demand, except for tree- and arity-valued parameters
    (`@filter-tree`, `@in`), whose audit surface is the closed
    vocabulary plus configured size caps.
5.  Dialect-agnostic core, pluggable dialect drivers. Lean on each
    database's own parser and prepare/describe protocol instead of
    reimplementing type inference. Where a dialect's oracle is weaker,
    degrade **explicitly** (require annotations), never silently.
6.  Friendly coexistence with sqlc-based projects and with hand-written
    dynamic SQL for the cases sqletch deliberately does not cover.

------------------------------------------------------------------------

# Dialect Architecture

Everything above the dashed line is shared across databases; everything
below it is a driver.

``` text
        template scanner (with per-dialect lexer profile),
        structural rules R1–R9, guard/scope analysis, shape keys,
        composition runtime, code generation skeleton, diagnostics
  ----------------------------------------------------------
        SQL grammar frontend | type oracle | placeholder
        style | DB-type → Go-type mapping | Go driver binding
```

A dialect driver provides:

-   **Lexer profile** — string/comment/placeholder/operator syntax so
    the shared scanner can walk dialect SQL without parsing it.
-   **SQL grammar frontend** — parses rendered SQL (see Compiler
    Architecture, Phase 1) into the dialect AST used for slot and
    scope analysis.
-   **Type oracle** — given a rendered query, returns parameter types
    and result column names/types by asking a dev database, plus a
    **catalog snapshot** (columns, `NOT NULL`, defaults) for offline
    analysis.
-   **Placeholder style and Go driver binding** — `$1` or `?`, and the
    typed-bind mechanism upholding premise P1: pgx for PostgreSQL,
    `database/sql` on MySQL and SQLite (any driver works; the tested
    ones are `go-sql-driver/mysql` and `ncruces/go-sqlite3`).
-   **Type mapping** — database types to Go types, including the
    pointer/enum conventions for optional and `@choose` parameters.

## Oracle backends (staged de-dependency strategy)

The type oracle is an interface; *where the dialect's type semantics
live* is a backend choice invisible to the core:

1.  **Server-backed**: a disposable container or user-supplied DSN,
    mitigated by the committed cache — the database is needed only on
    cache misses. This is how PostgreSQL and MySQL are served.
2.  **Embedded engine**: the same oracle served in-process, removing
    the external-database dependency entirely **without reimplementing
    type semantics** — the design principle stays "the database is the
    type checker", the database just moves in-process. The pinned
    `server_version` doubles as the embedded engine's version. SQLite
    is served this way today (the real engine compiled to WASM, run
    under wazero). The same treatment for PostgreSQL is designed and
    proven feasible but not yet shipped; see Beyond v1.0.
3.  **Native inference**: a self-implemented inference engine à la
    sqlc. Deliberately last, and only once the project has
    accumulated a large corpus of oracle results — every cache entry
    and conformance-suite run is a `(schema, query, types)`
    ground-truth triple, so a native backend is built and continuously
    **differential-tested against the real engine's answers** instead
    of written blind. MySQL, which has no embeddable real engine, is
    the dialect this backend serves (the others use their real
    engine). Selected with `database.oracle: native` (default
    `server`); the selection changes no verification semantics, only
    who answers. The backend is strict and fail-closed: constructs
    outside its modeled subset are refused with a diagnostic naming
    the escape hatches (`SQLETCH214` for query constructs,
    `SQLETCH215` for schema DDL), never guessed. Its subset leans on
    the Tier 2 annotation discipline: `-- @param` stays mandatory as
    everywhere on MySQL, and expression result columns additionally
    require an `AS` alias and a `-- @column` annotation. Acceptance
    is gated on byte-identical cache entries versus the server
    backend over the committed corpus (`internal/corpus`), and the
    corpus itself is continuously re-derived against a real MySQL in
    CI. Native runs assert nothing about planner-stage behavior
    (`EXPLAIN` coverage requires a server-backed run; `check
    --exhaustive` says so).

Backends are swappable per driver without touching the template
language, the structural rules, or the soundness argument.

No dialect protocol supplies *reliable* nullability (MySQL's result
metadata carries a `NOT_NULL` flag, but it degrades across joins and
expressions), so sqletch owns nullability analysis on every dialect
(see Phase 3). Dialect-semantic divergences inside SQL itself (e.g.
`LIMIT NULL` means "no limit" in PostgreSQL but is invalid in MySQL)
are the template author's concern: sqletch verifies against the
configured target dialect only.

## Drivers and support tiers

**Tier 1 — protocol-inferred types.**

-   **PostgreSQL** (first driver): `pg_query` (libpg_query) as the
    grammar frontend; extended-protocol `PREPARE`/`Describe` as the
    oracle. Parameter and result types are inferred by the database
    itself (the pgtyped approach). Where PostgreSQL cannot infer a
    parameter's type ("could not determine data type of parameter"),
    the fix is an explicit cast (`:param::type`); the diagnostic says
    so.

**Tier 2 — annotation-assisted types.**

-   **MySQL**: TiDB's parser as the frontend;
    `COM_STMT_PREPARE` metadata as the oracle. Result column metadata
    is reliable (including `org_table`/`org_name` source identity,
    resolved against an information_schema snapshot with synthetic
    stable table OIDs); **parameter types are not inferred by the
    protocol** and must come from template comment annotations
    (`-- @param status: varchar(16)`). Missing annotations are a
    compile error, not a guess. The plan check prepares `EXPLAIN
    <sql>` and executes it with all parameters NULL — executing an
    EXPLAIN plans without touching data. Placeholders are `?` per
    occurrence (repeated named params repeat the bind); generated code
    is the `database/sql` flavor. One documented approximation: the
    TiDB AST has no byte offsets on relation nodes, so relation
    locations are recovered lexically (a FROM-position name is preceded
    by FROM/JOIN/STRAIGHT_JOIN/','/'('/INTO/UPDATE, or by a `.` whose
    qualifier was itself in FROM position — a db-qualified `db.t`, not a
    bare `x.y` column reference; a backquoted identifier never counts as
    a keyword predecessor, and `SELECT`/`WITH`/`VALUES` parenthesized
    subqueries are skipped whole); the real-database property suite
    backstops it. A mis-location only shifts a diagnostic span, never a
    verdict.
-   **SQLite**: `sqlite3_prepare` plus declared
    column types as the oracle — over ncruces/go-sqlite3, the real
    SQLite compiled to WASM and run in-process under wazero, so this
    driver needs **no external database and no Docker at all**.
    Preparing compiles through SQLite's planner (prepare is the plan
    check); errors carry byte offsets. The engine supplies **no
    parameter type information at all**, so `-- @param` annotations
    are always required (same mechanism as MySQL); result types follow
    declared types through the affinity rules (with deliberate BOOLEAN
    and date/time carve-outs), and expression columns — whose declared
    type SQLite reports as NULL, `count(*)` included — require
    `-- @column name: type` annotations, enforced with diagnostics.
    Source-column identity comes from `column_origin/table_name`
    against a `pragma_table_info` snapshot with synthetic OIDs. The
    grammar frontend is the pure-Go rqlite/sql parser (byte offsets on
    every node); its known gaps versus the current SQLite grammar —
    RIGHT/FULL JOIN (3.39+), a few non-reserved keywords such as
    `ACTION` used as bare identifiers (quote them) — surface as parse
    diagnostics, and everything the grammar accepts is backstopped by
    the real engine.

The soundness model (R1–R9, maximal/minimal verification) is identical
across tiers; only the amount of type information the oracle supplies
for free differs. The conformance test suite (see Testing Strategy)
runs the same templates against every driver.

Dialect choice is per `sqletch.yaml` (`dialect: postgres | mysql |
sqlite`), one dialect per generation target. Templates are written in
that dialect's SQL — portability of the *tool*, not of the *queries*,
is the goal.

------------------------------------------------------------------------

# Template Language

Templates are **not text templates**. A template file consists of a
constant SQL skeleton with template constructs attached at fixed
grammatical positions ("slots"); construct placement is validated
against the dialect's parsed AST. Anything else is a compile error.

## Template inputs

A template lives either in a `.sql` file or in a Go file, as a `const`
marked `//sqletch:query` whose value is a single raw string literal.
The two forms are the same language and produce byte-identical
generated code and cache entries; extraction from Go is purely
syntactic (`go/parser`, never `go/types`), so the package need not
compile. The `const` requirement is load-bearing: the SQL that was
verified must be the SQL that runs, and conditionality must stay in
the constructs rather than migrating into Go control flow.

Lexing: constructs are recognized only as an exact, lowercase,
hyphenated keyword from the fixed vocabulary (`@if-present(`,
`@choose(`, …). PostgreSQL operators containing `@` (`@>`, `<@`, `@@`)
never match. MySQL user/system variables (`@var`, `@@sysvar`) never
match either — hyphenated names are not valid variable syntax. The
scanner's per-dialect lexer profile handles strings, comments, and the
`:param` vs `::cast` distinction.

## Query annotations

sqlc conventions apply and are constant per query (annotations are
never dynamic): `:one` (exactly one row; `sql.ErrNoRows` when absent),
`:maybe-one` (zero or one row; the method returns
`optional.Option[Row]` and maps the driver's no-rows error to `None` —
absence is a value, not an error), `:many` (slice), `:exec` (no
result), `:execrows` (affected count).

## Constructs

### `@if-present(param, …)` … `@endif`

Includes the enclosed fragment iff **all** listed parameters are
provided at runtime. Presence is expressed in Go by `Some` values of
`optional.Option[T]` fields (see Generated API Conventions).

Allowed slots:

-   a conjunct of the statement-level `WHERE` (an `AND …` term),
-   a conjunct of the statement-level `HAVING`,
-   a join item in the `FROM` clause (filter-only; see R2; `INNER` or
    `LEFT` only),
-   a `SET` item in `UPDATE`,
-   a column item and its positionally paired `VALUES` item in `INSERT`
    (see R7) — omitting the pair lets the database apply the
    column's `DEFAULT`.

Multiple blocks may share the same guard parameters; they switch on and
off together. Guards do not nest — a fragment needing two conditions
uses a multi-parameter guard: `@if-present(a, b)`.

Note: presence Options mean `None` = "omit the fragment". They cannot
express "filter where the column IS NULL" (SQL `NULL` as a *value* of
an optional filter). That case is `@when`'s job. See Design Boundary.

### `@choose(param)` / `@case(value)` / `@default` / `@end`

Selects exactly one case based on a **closed enum** parameter. The
compiler generates a Go enum type for `param`. If `@default` is absent,
the parameter is required, and passing the zero value makes the
generated function return an error before touching the database. A
`@default` block may be empty (meaning: emit nothing).

Allowed slots: the statement-level `ORDER BY` clause, a projection
expression, and `GROUP BY`. Projection and `GROUP BY` cases are
governed by rule R2 — all cases must produce the same column alias,
the same type,
**and are analyzed per case for nullability, the nullable-most case
winning** (see Phase 3).

The same `@choose` parameter may drive multiple slots; the selected
case applies to all of them simultaneously (coupled), so verification
cost stays linear in the number of cases. Distinct `@choose`
parameters cannot interact (R8), so per-case verification remains
sound without checking case combinations. Case bodies have an empty
guard set: they may not reference optional-join relations (R3).

## Further Constructs

These extend the same verification model; none of them reintroduces
enumeration or unverified SQL.

### `@when(param op literal)` … `@end` — value-conditioned guards

``` sql
@when(include_deleted = false)
  AND u.deleted_at IS NULL
@end
```

Like `@if-present`, but the condition compares a **required** parameter
against a compile-time literal (`op` is `=` or `!=`; `<>` is accepted
as an alias), evaluated in Go. Literals are strings (with `''`
escapes), integers, or booleans — the literal fixes the parameter's Go
type. An integer literal must be a plain decimal digit run that fits a
64-bit signed integer, with no leading zero: the literal is emitted
verbatim into the generated Go comparison, where a leading zero would
read as an octal constant (`010` == 8) and silently change which value
the guard matches, so it is rejected (SQLETCH014).
The parameter's Go type comes from the literal (it need not bind in
SQL — a sanctioned pure-control form, cf. R9's closing bullet; if it
*does* bind, the literal-derived and SQL-inferred types must agree,
checked in Phase 3). The shape key gains one bit per `@when`. Verification is
identical to `@if-present`: **all** `@when` fragments are included in
the maximal rendering, even mutually exclusive ones — an unreachable
maximal is still deletion-sound for every reachable shape. The
practical consequence: mutually exclusive fragments must not conflict
structurally in the maximal rendering (e.g. two `@when`-guarded joins
must use distinct aliases). For R3, a value condition is a guard
*atom* compared by exact equality (`mode = 'a'` and `mode = 'b'` are
unrelated atoms). This is also the idiomatic way to express "filter
where column IS NULL": `@when(status_mode = 'null') AND u.status IS
NULL @end`.

### `@filter-tree(param)` / `@predicate(name)` … `@end` — user-composed boolean trees

Covers advanced-search UIs (arbitrary AND/OR nesting over a **closed
predicate set**):

``` sql
WHERE TRUE
  AND @filter-tree(criteria)
      @predicate(status_eq)     u.status = :status
      @predicate(email_prefix)  u.email LIKE :email_prefix || '%'
      @predicate(created_after) u.created_at >= :created_after
      @end
```

``` go
crit := gen.And(
    gen.SearchUsersStatusEq("active"),
    gen.Or(
        gen.SearchUsersEmailPrefix("admin-"),
        gen.SearchUsersCreatedAfter(since),
    ),
)
rows, err := q.SearchUsers(ctx, gen.SearchUsersParams{Criteria: crit, Limit: 50})
```

Each predicate is verified once (all predicates conjoined in the
maximal query). Because every predicate is a verified boolean fragment
and AND/OR/parentheses preserve type and safety, **any** runtime tree
over the closed set is sound — compositional verification extends to
arbitrary boolean combination. Composition remains
concatenation-of-constants; parameters bind through typed per-predicate
constructors, so injection remains impossible.

Specification details:

-   The composer wraps **every predicate and every subtree in
    parentheses** — a predicate containing a top-level `OR` can never
    change the meaning of its neighbors (P2).
-   Predicates have empty guard sets: they may reference only
    constant-skeleton scope, never optional-join relations (R3).
-   The same predicate may appear multiple times in one tree with
    independent bindings; placeholder numbering is per-occurrence.
-   An empty/nil tree renders as `TRUE` — unless the construct is
    declared **required** with `@filter-tree!(param)`: then the zero
    value makes the generated function return an error before touching
    the database, and deliberately unfiltered access must be spelled
    with the generated `<Query>Unscoped()` constructor (which renders
    `TRUE`). Required mode exists for multi-tenant robustness:
    *forgetting* the filter is an error; *opting out* is one
    greppable, reviewable line at the call site. The empty form is a
    **verified rendering** of its own (the tree slot as the literal
    `TRUE`), so the fallback is parsed, described, and planned like
    any other rendering, and the runtime's nil/`Unscoped` composition
    is conformance-pinned to it byte-for-byte.
-   The runtime enforces configurable tree caps (default: 32 nodes,
    depth 8; `filter_tree_caps` in sqletch.yaml, baked into generated
    code) to bound adversarially large inputs.
-   The construct occupies a **WHERE or HAVING conjunct slot** and
    must be one whole conjunct: it is written directly after an
    unconditional `AND` and nothing may extend its conjunct after
    `@end`. This is enforced — lexically at scan time (both anchors)
    and structurally by R1 on the empty rendering, whose `TRUE` must
    map to exactly one top-level conjunct of its clause (which catches
    precedence splicing like `a OR b AND @filter-tree(…)`). Anywhere
    else the `TRUE` fallback would silently disarm or change the
    filter (`OR TRUE`, `NOT TRUE`).
-   Implementation constraint: at most **one** `@filter-tree` per
    query — a local restriction, not a model limit; lifting it is
    future work (its main use case, a required scope tree alongside an
    optional criteria tree, is largely covered by policy weaving).
-   Statement/text caches key on the canonical tree encoding (hash as
    index, full encoding compared on hit) and are capacity-bounded
    with approximate-LRU (second-chance) eviction.

### `@order-by(param)` / `@key(name)` … `@end` — multi-key sorting

Covers data-grid style multi-column sort (any subset of a closed key
set, in any order, each ascending or descending) without `@choose`'s
factorial case explosion:

``` sql
@order-by(sort)
@key(created_at) u.created_at
@key(email)      u.email
@key(id)         u.id
@end
```

``` go
Sort: []gen.ListUsersSortKey{
    gen.ListUsersSortEmailAsc,
    gen.ListUsersSortCreatedAtDesc,
},
```

Each key expression is verified once; subsets and permutations are
sound by the same list-clause argument. An empty list omits the
clause, or emits the `@default` body if declared — **the `@default`
body is verified as an extra rendering, exactly like a `@choose`
case**. Constraints: statement-level `ORDER BY` only; key expressions
have empty guard sets (R3); **forbidden in queries using PostgreSQL's
`DISTINCT ON`** — `DISTINCT ON` makes ORDER BY validity
prefix-order-sensitive, which breaks the subset/permutation argument
(`@choose` remains available there, since every case is verified
whole); and in statements whose skeleton makes ORDER BY mandatory
(PostgreSQL's `FETCH FIRST … WITH TIES`), a `@default` is required so
the clause can never vanish.

### `expr @in(:param)` — variable-arity membership

``` sql
WHERE TRUE
  AND u.status @in(:statuses)
```

`:statuses` is a Go slice. On PostgreSQL this renders as
`u.status = ANY($1)` — a single static shape, no expansion. On dialects
without array parameters (MySQL, SQLite) it renders as
`u.status IN (?, ?, …)` with one placeholder per element — a
deterministic, injection-free expansion whose shape key includes the
arity. An empty slice renders as `FALSE` (identical to PostgreSQL's
`= ANY('{}')`). Three-valued-logic caveat, uniform across dialects and
identical to hand-written SQL: for a `NULL` operand, a non-empty list
yields `NULL` while an empty list yields `FALSE` — under negation these
differ (row kept vs dropped at arity 0); the generated documentation
notes this per use. Arity-expanded queries are excluded from strict
static expansion, like `@filter-tree`.

Rationale: on Tier 1 the author can simply write `= ANY(:ids)` by hand;
`@in` exists so the *same need* is expressible on Tier 2 dialects,
keeping the everyday dynamic-SQL vocabulary dialect-complete.

Implementation constraints: `@in` is accepted at depth-0 WHERE/HAVING
skeleton positions; inside guarded fragment bodies it is rejected with
a diagnostic (on PostgreSQL, write `= ANY(:param)` directly there). On
the expanding dialects the arity is a shape-key dimension (canonical
`;n=` segment); verification quotients the
unbounded arity space to two representative classes — arity 1 stands
for every non-empty list (IN-list growth is parse-invariant) and arity
0 is its own verified rendering, emitted per dialect
(`IN (SELECT NULL FROM DUAL WHERE FALSE)` on MySQL,
`IN (SELECT NULL WHERE 0)` on SQLite) so the empty list is FALSE
even for a NULL operand, exactly like `= ANY('{}')`. `-- @param name:
type` annotations are **optional assertions on Tier 1 and the
mandatory source of parameter types on Tier 2** (a missing annotation
is a compile diagnostic naming the parameter; on expanding dialects
the annotation gives the @in ELEMENT type, on PostgreSQL the ARRAY
type). Where the oracle types parameters, an annotation may not
*override* it: a disagreement is a compile diagnostic and the oracle's
type wins. Overriding would let a bind be typed at something the query
was never verified with, defeating P1 — and it is unobservable to
every other phase, since the oracle types the rendered SQL and never
sees the annotation. Type names are matched case-insensitively with
length/precision arguments stripped (`varchar(16)` → `varchar`;
`bigint unsigned` folds the modifier); an unknown parameter or type
name is a compile diagnostic.

## Structural Rules

-   **R1 — Structured slots.** Template constructs appear only at the
    slots listed above, at the **top level of the statement** — never
    inside subqueries, CTEs, derived tables, `OVER (…)` windows, or
    aggregate-internal `ORDER BY` (guarded fragments may themselves
    *contain* subqueries). Each fragment must correspond to **exactly
    one complete AST node** in its slot (one conjunct, one join item,
    one SET item, …) — so deleting a neighbor can never regroup a
    fragment's meaning. Placement and node-completeness are validated
    against the dialect AST. At most one dynamic construct per
    `ORDER BY` clause.
-   **R2 — Constant result shape.** Optional blocks must not change the
    result shape. Optional joins may not contribute result columns and
    must be `INNER` or `LEFT` — `RIGHT`/`FULL` are rejected because
    they null-extend skeleton relations, changing nullability per
    shape. Optional `SET`/`INSERT` items affect what is written, never
    what is returned (`RETURNING` lists are part of the constant
    skeleton). `SELECT *` is expanded at compile time against the
    maximal scope; if the expansion would include columns from an
    optional join, that is an R2 error. `@choose` projection cases
    must agree on alias and type, and nullability is unioned across
    cases (Phase 3). Consequently, *every* reachable shape has
    identical result columns, names, types, and nullability.
-   **R3 — Guarded scope (resolution-based).** Any column or alias
    reference that **resolves to** a relation introduced by an optional
    join must appear in a fragment whose guard set is a superset of
    that join's guard set. The rule operates on resolved references on
    the maximal query's AST, so unqualified column references are
    covered, not just explicit alias mentions. Guard sets are sets of
    atoms — presence atoms (`organization_id`) and value atoms
    (`mode = 'a'`) — compared by exact atom equality. `@choose` cases,
    `@order-by` keys, and `@filter-tree` predicates have **empty guard
    sets by definition**: they may not reference optional-join
    relations at all. (Ambiguous references are already errors on the
    maximal query.)
-   **R4 — Closed choice.** `@choose` cases, `@filter-tree` predicates,
    and `@order-by` keys form closed sets known at compile time. No
    arbitrary expressions, identifiers, or SQL fragments can be
    injected through them.
-   **R5 — Flat and finite.** No nesting of guards, no loops, no
    recursion, no arbitrary identifier interpolation, no dynamic table
    or column names, no user-defined template functions.
-   **R6 — Anchored clauses.** A clause that is syntactically optional
    as a whole (`WHERE`, `ORDER BY`, `HAVING`) is simply omitted when
    all its items are inactive (the composer owns the clause keyword,
    P2). A clause that **must** be non-empty (`SET`, the `INSERT`
    column list) must contain at least one unconditional item; the
    compiler rejects templates whose minimal shape would be invalid
    SQL. (Convention when *every* `WHERE` conjunct is optional: write
    `WHERE TRUE` as the anchor; `sqletch fmt` inserts it.)
-   **R7 — Paired guards.** In `INSERT`, a guarded column item and its
    corresponding `VALUES` item must carry identical guard sets and
    matching positions; for multi-row `VALUES`, the same-guard item
    must appear at the same position in **every** row. The compiler
    verifies the pairing.
-   **R8 — Skeleton-defined names.** Names referenced across fragment
    boundaries must be defined in the constant skeleton; the sole
    exception is optional-join aliases, governed by R3. Names defined
    inside a `@choose` case are local to that case and may not be
    referenced from anywhere else (including other cases). This is what
    makes distinct `@choose` parameters non-interacting and per-case
    substitution sufficient — without it, verifying case *combinations*
    would be required and linearity would be lost.
-   **R9 — Parameter discipline.**
    -   A parameter is **optional** iff every one of its bind
        appearances lies in fragments whose guard sets include it; it
        becomes an `optional.Option[T]` field (`None` = absent).
    -   A parameter with any unguarded appearance is **required** (a
        plain value field). Listing a required parameter as an
        `@if-present` guard is a compile error (the guard would be
        vacuously true).
    -   A parameter appearing only inside fragments guarded by *other*
        parameters is required; its value is simply unused when those
        fragments are inactive.
    -   A parameter binding only inside `@choose` cases or `@order-by`
        keys is required; its value is unused when its case/key is not
        selected. Its type is inferred from the renderings containing
        it, which must agree (checked in Phase 3).
    -   Every `@if-present` guard parameter must bind in at least one
        fragment among those it guards — otherwise its Go type would be
        uninferable. Pure control parameters that never bind in SQL are
        exclusively `@when`'s role (typed by the literal) and
        `@choose`/`@order-by`/`@filter-tree`'s role (typed by their
        generated enum/tree types).

## Language details

-   Construct keywords are lowercase and matched exactly.
-   Parameters are named (`:name`) only; writing `$1` or `?` directly
    in a template is an error. `:name` is rewritten to the dialect's
    placeholder style before the frontend parses a rendering.
-   SQL comments are legal anywhere the dialect allows them; `-- @param`
    comments are compiler directives. Fragment bytes, including
    comments, are preserved verbatim in composed SQL (P2's newline
    joining keeps trailing line comments safe).
-   Whitespace is preserved inside fragments and normalized to single
    newlines between them — this is what makes "byte-identical composed
    SQL per shape key" well-defined.
-   Generated identifiers map `snake_case`/case values to PascalCase;
    collisions after mapping are compile errors.

## Rejected Examples

Scope violation (R3) — note the *unqualified* reference is caught,
because R3 operates on resolved references:

``` sql
@if-present(organization_id)
JOIN organization_users AS ou ON ou.user_id = u.id
@endif
WHERE TRUE
  AND organization_id = :organization_id   -- ERROR: resolves to ou
```

``` text
users.sql:6:7: "organization_id" resolves to column
"ou.organization_id" of the optional join guarded by `organization_id`
(users.sql:2), but this predicate is not guarded by it.
Move this predicate inside @if-present(organization_id).
```

Construct outside a slot (R1) — a projection item is not a slot,
precisely because it would change the result shape (R2):

``` sql
SELECT
    u.id
@if-present(with_email)
  , u.email        -- ERROR: not a slot; optional blocks may not
@endif             --        change the result shape
FROM users AS u
```

Unanchored required clause (R6):

``` sql
UPDATE users SET
@if-present(email)
    email = :email     -- ERROR: every SET item is optional; the shape
@endif                 -- with all guards off would be `UPDATE users SET`
WHERE id = :id;        -- add an unconditional item, e.g. updated_at = now()
```

Vacuous guard (R9):

``` sql
WHERE u.tenant_id = :tenant_id      -- :tenant_id binds unguarded ⇒ required
@if-present(tenant_id)              -- ERROR: guard on a required parameter
  AND u.plan = 'enterprise'         --        is always true
@endif
```

Nesting (R5) — use a multi-parameter guard instead (note both guard
parameters bind inside the fragment, satisfying R9):

``` sql
@if-present(min_score)
  @if-present(max_score)   -- ERROR: guards do not nest
    AND t.score BETWEEN :min_score AND :max_score
  @endif
@endif

-- write instead:
@if-present(min_score, max_score)
  AND t.score BETWEEN :min_score AND :max_score
@endif
```

------------------------------------------------------------------------

# Use Cases and Examples

## Use Case 1: Faceted Search (admin user search screen)

The canonical case sqlc cannot express well: a search screen with many
independent optional filters, an optional tenant-scoping join, and a
user-selectable sort order.

``` sql
-- name: SearchUsers :many
SELECT
    u.id,
    u.email,
    u.status,
    u.created_at
FROM users AS u

@if-present(organization_id)
JOIN organization_users AS ou
  ON ou.user_id = u.id
 AND ou.organization_id = :organization_id
@endif

WHERE TRUE

@if-present(status)
  AND u.status = :status
@endif

@if-present(email_prefix)
  AND u.email LIKE :email_prefix || '%'
@endif

@if-present(created_after)
  AND u.created_at >= :created_after
@endif

@choose(sort)
@case(created_at_desc)
ORDER BY u.created_at DESC
@case(created_at_asc)
ORDER BY u.created_at ASC
@case(email_asc)
ORDER BY u.email ASC, u.id ASC
@default
ORDER BY u.id ASC
@end

LIMIT :limit;
```

This template reaches 2^4 × 4 = **64 distinct query shapes**, yet
compilation verifies only **4 renderings** (the maximal query plus
three alternative `@choose` cases) and runs the structural rules once —
work linear in template size, not in shape count. No shape is
materialized unless the user asks for it (`sqletch explain`).

Generated API (sketch):

``` go
type SearchUsersSort int

const (
    SearchUsersSortDefault       SearchUsersSort = iota // ORDER BY u.id ASC
    SearchUsersSortCreatedAtDesc
    SearchUsersSortCreatedAtAsc
    SearchUsersSortEmailAsc
)

type SearchUsersParams struct {
    OrganizationID optional.Option[int64]  // None = omit the join
    Status         optional.Option[string] // None = omit the predicate
    EmailPrefix    optional.Option[string]
    CreatedAfter   optional.Option[time.Time]
    Sort           SearchUsersSort         // zero value = @default
    Limit          int64                   // LIMIT params describe as int8
}

type SearchUsersRow struct {
    ID        int64
    Email     string
    Status    string
    CreatedAt time.Time
}

func (q *Queries) SearchUsers(ctx context.Context, arg SearchUsersParams) ([]SearchUsersRow, error)
```

Call site (`optional` is `github.com/moznion/go-optional`):

``` go
rows, err := q.SearchUsers(ctx, gen.SearchUsersParams{
    Status: optional.Some("active"),
    Sort:   gen.SearchUsersSortCreatedAtDesc,
    Limit:  50,
})
```

Note: the filtering join above could equally be written as an `EXISTS`
predicate inside the same guard; both are filter-only and verify the
same way. Authors who worry about row multiplication from joins should
prefer the `EXISTS` form.

## Use Case 2: Partial UPDATE — PATCH semantics

Alongside faceted search, this is the most common dynamic-SQL need in
practice: update only the fields the caller provided (a REST `PATCH`
handler), letting untouched columns keep their values.

``` sql
-- name: UpdateUserProfile :one
UPDATE users
SET
    updated_at = now()   -- unconditional anchor (R6)
@if-present(email)
  , email = :email
@endif
@if-present(nickname)
  , nickname = :nickname
@endif
@if-present(bio)
  , bio = :bio
@endif
WHERE id = :id
RETURNING id, email, nickname, bio, updated_at;
```

``` go
row, err := q.UpdateUserProfile(ctx, gen.UpdateUserProfileParams{
    ID:    userID,
    Email: optional.Some("new@example.com"), // nickname and bio remain untouched
})
```

Every `SET` item verifies independently (same list-clause algebra as
WHERE conjuncts); the anchor guarantees the minimal shape is valid; the
`RETURNING` list is constant, so there is exactly one row struct.

The `INSERT` counterpart (optional column/value pairs so omitted
columns receive their `DEFAULT`) works the same way under R7:

``` sql
-- name: CreateUser :one
INSERT INTO users (
    email
@if-present(nickname)
  , nickname
@endif
) VALUES (
    :email
@if-present(nickname)
  , :nickname
@endif
)
RETURNING id;
```

Caveat: a `NOT NULL` column without a default that is omitted fails at
execution time, not compile time — prepare-level verification cannot
see per-shape constraint outcomes. The compiler warns when an optional
insert column is `NOT NULL` without a default.

## Use Case 3: Cursor Pagination

The first page and subsequent pages differ only by one predicate.
Without conditional SQL this forces either two hand-maintained queries
or a sargability-hostile `(:after_id IS NULL OR …)` workaround.

``` sql
-- name: ListAuditLogs :many
SELECT a.id, a.actor_id, a.action, a.created_at
FROM audit_logs AS a
WHERE a.tenant_id = :tenant_id

@if-present(after_id)
  AND a.id < :after_id
@endif

ORDER BY a.id DESC
LIMIT :limit;
```

(The unconditional `tenant_id` predicate is the anchor here, so no
`WHERE TRUE` is needed — R6 requires it only when every conjunct is
optional.)

``` go
// first page
page1, _ := q.ListAuditLogs(ctx, gen.ListAuditLogsParams{TenantID: t, Limit: 100})
// next page
page2, _ := q.ListAuditLogs(ctx, gen.ListAuditLogsParams{
    TenantID: t,
    AfterID:  optional.Some(page1[len(page1)-1].ID),
    Limit:    100,
})
```

Each shape is a plain static query as far as the database is concerned,
so both shapes get their own optimal plan — unlike the `IS NULL OR`
workaround, which pessimizes the plan for every caller.

## Use Case 4: Dashboard Time Bucketing

`@choose` in a projection slot, valid because every case yields the
same alias and type, with nullability unioned across cases (R2). The
alias `bucket` is defined in the constant skeleton (R8), which is why
the `GROUP BY` and `ORDER BY` references to it are legal:

``` sql
-- name: SignupsByBucket :many
SELECT
    @choose(bucket)
    @case(daily)   date_trunc('day',   u.created_at)
    @case(weekly)  date_trunc('week',  u.created_at)
    @case(monthly) date_trunc('month', u.created_at)
    @end AS bucket,
    count(*) AS signups
FROM users AS u
WHERE u.created_at >= :since
GROUP BY bucket
ORDER BY bucket;
```

``` go
rows, _ := q.SignupsByBucket(ctx, gen.SignupsByBucketParams{
    Bucket: gen.SignupsByBucketBucketWeekly, // required: no @default
    Since:  monthStart,
})
```

## Use Case 5: Advanced Search and Cross-Layer Filters

Two shapes of the same `@filter-tree` mechanism (specification above):

**End-user search UI** — a Jira/Gmail-style screen where end users
combine a fixed vocabulary of conditions with arbitrary AND/OR
nesting — fully inside the verification model because the predicate
set is closed.

**Repository/use-case layering** — a repository function that does not
hard-code its filtering; each use case passes the filter in. What
crosses the layer boundary is **not SQL text but a typed value over
the query's compiled predicate vocabulary**, so every guarantee
survives the crossing:

``` sql
-- name: ListOrders :many
SELECT o.id, o.total, o.created_at
FROM orders AS o
WHERE TRUE
  AND @filter-tree!(scope)
      @predicate(tenant)     o.tenant_id = :tenant_id
      @predicate(org)        o.org_id = :org_id
      @predicate(created_in) o.created_at >= :from AND o.created_at < :to
      @end
ORDER BY o.id DESC;
```

``` go
// repository: receives the filter, knows nothing about its content
func (r *OrderRepo) List(ctx context.Context, scope gen.ListOrdersScope) ([]Order, error) {
    return r.q.ListOrders(ctx, scope, gen.ListOrdersParams{})
}

// use cases decide applicability and combination:
repo.List(ctx, gen.ListOrdersTenant(tenantID))                        // tenant-scoped
repo.List(ctx, gen.And(gen.ListOrdersOrg(orgID),
                       gen.ListOrdersCreatedIn(from, to)))            // admin view
repo.List(ctx, gen.ListOrdersUnscoped())                              // explicit opt-out
```

The division of ownership is deliberate: predicates reference the
query's aliases and columns — repository knowledge — so the vocabulary
lives in the template; the caller owns only selection and combination.
A new filter is a one-line `@predicate` addition, never a string from
outside. Because the construct is declared required (`@filter-tree!`),
a forgotten filter is a runtime error before any SQL is sent, and
unscoped access is a visible, greppable call — silent tenant-boundary
leaks are structurally impossible.

------------------------------------------------------------------------

# Cross-Query Policies

*Added after the v1.0 freeze (design record: `design/14-policy-weaving.md`;
decisions settled 2026-08-02). Additive only: templates and configs
that declare no policy are byte-identically unaffected.*

`@filter-tree!` protects the queries that declare it; a query added
next month with no filter is silently unscoped. A **policy** inverts
that default at the codebase level: a boolean predicate declared once
in `sqletch.yaml` is woven, at compile time, into every query that
touches a designated table — and a check proves that **no reachable
shape of any query touches a designated table unscoped**. Forgetting
becomes impossible; opting out becomes one explicit, reviewable
annotation.

## Declaration

```yaml
policies:
  - name: tenant_scope
    tables: [orders, order_items, invoices]
    predicate: "{} .tenant_id = :tenant_id"
    param:
      name: tenant_id
      type: bigint            # Tier 2 dialects require it; Tier 1 asserts it
    applies_to: [select, update, delete]   # default: select, update, delete
```

- `tables` entries and `param.name` are bare lowercase identifiers.
- `{}` in the predicate stands for the designated relation as it is
  named *in the target query*: its alias if it has one, else the table
  name. The substituted name must be a bare identifier
  (`[A-Za-z_][A-Za-z0-9_]*`); anything needing dialect quoting is
  rejected.
- The predicate may reference only the designated relation (via `{}`)
  and its own `:param`s. It must parse as one complete boolean
  expression in the dialect (`SQLETCH303` otherwise).
- Multiple policies weave in declaration order, each as its own
  conjunct. `INSERT … VALUES` is not a policy target (no rows are
  filtered); an `INSERT … SELECT` that reads a designated table is
  rejected (`SQLETCH125`) — v1 has no modeled insertion point inside
  an INSERT's select body. Opt out or restructure.

## Weaving semantics

Weaving happens **after scanning and before rendering**: every
downstream phase — structural rules, the type oracle, nullability,
shape enumeration, codegen — sees an ordinary template and cannot tell
a woven conjunct from a hand-written one. The SQL that is verified is
the scoped SQL; there is no window in which the verified statement and
the executed statement differ. The soundness argument applies
verbatim.

The woven conjunct is **unconditional skeleton text with the empty
guard set** (the same discipline as `@filter-tree` predicates): it is
present in every shape, appended to the WHERE clause as a final
`AND`-conjunct, or as a synthesized `WHERE` when the query has none
(the `DELETE FROM orders` case). It does not count as the author's R6
anchor: the anchor rule is checked on the *unwoven* template, so a
template's validity never depends on configuration. Nullability is
unaffected.

Rules, settled deliberately (D1–D6 in the design record):

- **Every top-level occurrence** of a designated table gets its own
  conjunct (self-joins get one per side). Skipping one would silently
  weaken "every row you read is scoped".
- **A designated table on the null-extended side of an outer join is
  woven into its own join's `ON` clause**, not WHERE: a WHERE conjunct
  would silently turn the outer join into an inner join, while an `ON`
  conjunct preserves the outer row set and scopes only the joined
  rows. `USING`/`NATURAL` joins have no `ON` expression to extend and
  are rejected (`SQLETCH125`): rewrite as an explicit `ON`, or opt
  out.
- **A designated table visible only inside a subquery or CTE body is
  rejected** (`SQLETCH125`): v1 weaves at the top level only, and loud
  incompleteness beats silent incompleteness. A CTE whose *name*
  shadows a designated table is conservatively treated as touching it.
- **A designated table introduced by a guarded (`@if-present`) join is
  rejected** (`SQLETCH125`): it cannot be unconditionally scoped.
- **A parameter-name collision with an incompatible kind is rejected**
  (`SQLETCH125`): the woven parameter is a *required* value bound
  unconditionally, so it may share a name only with a plain required
  value parameter the author already declared. A collision with an
  optional (`@if-present`) parameter would bind `NULL` in every shape a
  caller omits it (silently emptying the result), and a collision with
  a control parameter (`@when`, presence guard, or `@filter-tree`
  `@predicate` argument) would bind a value where a control parameter
  belongs — both rejected loudly rather than woven.
- **The policy parameter is an ordinary parameter**: it appears in the
  affected queries' generated `Params` structs, typed by the oracle
  (Tier 1, where `param.type` is asserted like a `-- @param` hint) or
  by `param.type` (Tier 2, where it is mandatory). No ambient state,
  no context extraction; the generated API contract is unchanged.

## Enforcement and opt-out

Weaving covers what the weaver reaches; the enforcement check states
the invariant: for every relation whose table is designated by a
policy, a conjunct matching that policy is present **in every
reachable shape** (`SQLETCH124` otherwise) — in the query's WHERE
clause, or in the relation's own `ON` clause for a null-extended
outer-join occurrence. A
hand-written scoping conjunct inside `@if-present` satisfies only the
guard-on shapes and therefore fails — the quantifier is what makes
this a proof rather than a formality.

The opt-out is a per-query annotation with a mandatory reason:

```sql
-- name: ListAllOrdersForBackfill :many
-- @policy-optout: tenant_scope (batch job; runs outside any tenant)
SELECT ...
```

An opt-out naming an unknown policy, or one that does not apply to the
query, is `SQLETCH126` — renaming a policy can never silently disarm
its opt-outs. `sqletch explain` reports per-query policy coverage
(woven / opted out with reason), machine-readably under
`--format json`.

## Boundary

Policies constrain only sqletch-generated queries; they express
conjunctive row filters, not column masking or per-role rules; and
they guarantee the predicate is *present*, not that its runtime
argument is *correct*. They complement, not replace, `@filter-tree!`
(caller-chosen filters) and database RLS (runtime defense in depth for
non-sqletch clients).

------------------------------------------------------------------------

# Compiler Architecture

## Phase 1 — Scanning and parsing (two layers)

The dialect grammar cannot parse template constructs directly, so
parsing is honestly two-layered:

1.  The **template scanner** extracts constructs and produces the
    constant skeleton plus guarded fragments, all with exact source
    positions. The scanner is construct-generic but **lexically
    dialect-aware** via the driver's lexer profile (string/comment/
    placeholder/operator syntax), because it must walk dialect SQL
    without parsing it. The scanner owns all source positions; named
    parameters are rewritten to the dialect placeholder style before
    any rendering is parsed.
2.  The **maximal rendering** (and each `@choose` case rendering) is
    parsed by the dialect frontend (PostgreSQL: `pg_query`). The
    scanner-owned source map ties every fragment to its AST nodes; slot
    legality and node-completeness (R1) and scope analysis (R3, R8) are
    checked on the parsed AST.

"Single AST" is therefore shorthand for: one template structure layer +
one dialect AST per rendering, joined by exact source maps.

## Phase 2 — Structural rule check

Enforce R1–R9 on the scanner output and the parsed renderings: guard
groups, resolution-based scope coupling, case locality, minimal-shape
validity (anchors, pairing), parameter discipline. Known
planner-sensitive combinations are also rejected statically as they are
identified (e.g. `FOR UPDATE` combined with an optional `LEFT JOIN`,
which PostgreSQL's planner rejects per shape). All diagnostics point at
template-file positions.

## Phase 3 — Type extraction (dialect type oracle)

Render the maximal query (all guards on, first case of each `@choose`,
all `@filter-tree` predicates conjoined, all `@order-by` keys listed).
Against a dev database with the project's schema applied, the dialect
oracle extracts parameter types and result column names/types (Tier 1:
inferred; Tier 2: from explicit annotations — missing ones are compile
errors). Repeat the oracle call once per remaining `@choose` case (and
per `@order-by` `@default` body) to confirm case-wise agreement on
names and types.

**Schema setup** is a list of plain ordered `.sql` files/globs that
sqletch applies itself; sqletch does not reimplement migration
tooling. (Handing schema setup to a user command, so goose/Atlas/Flyway
users can reuse their migrations directly, is designed but not shipped
— see Beyond v1.0.)

**Nullability** is not reliably provided by any prepare/describe
protocol. It is computed by sqletch's own analysis under a
**per-shape-sound discipline**: narrowing uses only *unconditional*
(skeleton) predicates and joins — guarded fragments never narrow
nullability, and optional joins are `INNER`/`LEFT` only (R2) so they
can filter but never null-extend skeleton columns; `@choose` projection
cases are analyzed per case with the nullable-most result winning.
This makes the skeleton-based result a correct upper bound for every
reachable shape, which is what licenses running the analysis once per
query. It is the largest piece of compiler-owned analysis and
historically the hardest part of tools in this space (sqlc included);
the fallback is conservative (`nullable`), with per-column overrides
available.

**The dev database is a hard dependency of a cold `generate`/`check` —
a real DX regression versus sqlc's offline model, mitigated by
caching**: the committed cache stores oracle results *and the catalog
snapshot* (needed offline for resolution, `SELECT *` expansion, and
nullability), keyed by `(dialect, server version, schema fingerprint,
rendered query hash)`. The schema fingerprint is a hash over the
ordered schema inputs; the server version comes from a pinned
`server_version` in configuration (validated against the dev DB
whenever one is connected) — so the full key is offline-computable,
and a dev-DB upgrade is an explicit config change, never silent
drift. Cache entries
store their full keys and inputs; hashes are an index, compared on
read, never trusted as identity. When neither the schema nor a query
changed, `check` and `generate` run fully offline (CI included). The
database is needed only on cache misses — and "the database" itself is
an oracle *backend* choice (server, embedded in-process engine, or
eventually corpus-validated native inference; see Oracle backends in
Dialect Architecture), so this dependency is scheduled to shrink to
zero without changing the verification model.

## Phase 4 — Code generation

Per query, emit:

-   one params struct (optional params as `optional.Option[T]` fields,
    `@choose` params as generated enum types, `@filter-tree` params as
    typed tree values with per-predicate constructors),
-   one row struct,
-   one public function,
-   precompiled fragment tables and a deterministic composition routine
    (see Runtime Model), using the dialect's placeholder style and the
    compile-time parameter types (premise P1).

Generated code imports the dialect's canonical Go driver (pgx for
PostgreSQL) plus a small sqletch `runtime` helper package; the helper
itself has no dependencies beyond the driver interfaces.

------------------------------------------------------------------------

# Generated API Conventions

sqlc-compatible on purpose, so both generators' output coexists in one
codebase and one transaction:

-   A `DBTX` interface, `New(db DBTX) *Queries`, and
    `(*Queries).WithTx(…) *Queries`, matching sqlc's convention for
    the same target: pgx flavor on PostgreSQL (`pgx.Tx`),
    `database/sql` flavor on Tier 2 dialects (`*sql.Tx`). On each
    target, one transaction value satisfies both sqlc's and sqletch's
    `DBTX`.
-   A generated `Querier` interface over all query methods, for mocking
    in user tests.
-   An optional per-`Queries` hook (`OnQuery(shapeKey, sql string)`)
    exposing the composed SQL text for logging and tracing — "what SQL
    did this call actually run" is observable at runtime, not only via
    `sqletch explain`.
-   Absence is uniformly `github.com/moznion/go-optional`'s
    `Option[T]`: optional (presence) parameters, nullable result
    columns, and the `:maybe-one` result all use it — never bare
    pointers. The generated code depends on go-optional; the scan path
    still hands the driver plain `*T` destinations and converts with
    `optional.FromNillable`, so driver behavior is unchanged.
-   Designed to run under `//go:generate sqletch generate`.

------------------------------------------------------------------------

# Runtime Model

At runtime, a call computes its **shape key**: a bitmask of active
guards (`@if-present` and `@when`), the ordinal of each `@choose`
selection, plus the canonical encoding of each `@filter-tree`
value, the key sequence of each `@order-by` value, and the arity of
each `@in` list on expanding dialects. Composition then:

1.  walks the fragment table in source order (`@order-by` keys follow
    the caller's sequence — the one sanctioned reordering, P2),
2.  appends the fragments active under the shape key verbatim, joined
    per P2's fixed connective vocabulary,
3.  assigns placeholder numbers/markers in first-occurrence order
    (per-occurrence for repeated `@filter-tree` predicates) using
    precomputed per-fragment parameter lists,
4.  binds the corresponding params-struct fields **with their
    compile-time types** (premise P1).

Properties:

-   **Deterministic**: a given shape key always yields byte-identical
    SQL.
-   **Injection-free by construction**: only compile-time-constant
    fragments plus P2's fixed connectives are emitted; user values
    travel exclusively through bind parameters.
-   **Plan-cache friendly**: composed SQL text is cached per shape key
    (capacity-bounded, keys compared in full on hit). The set of shapes
    an application actually uses is typically tiny.
-   **No runtime parsing**: composition is table-driven concatenation,
    O(fragments).

## Statement caching

The composed SQL string is cached per shape key in a per-`Queries`
bounded cache, and statements execute unnamed/ad-hoc. Eviction
approximates LRU (second chance) rather than ordering exactly: cache
hits take no lock, so recency is recorded per entry instead of by
reordering shared state. The capacity bound is exact either way, and
the cache is a memoization of a pure function — eviction order can
never change which SQL a shape yields. That is safe under
transaction-pooling proxies (PgBouncer transaction mode, RDS Proxy),
where server-side prepared statements are unreliable. Opting into
server-side prepared statements — by delegating to the driver's own
per-connection statement cache, which is the only place connection
affinity and deallocation can live — is designed but not shipped; see
Beyond v1.0.

## Strict static expansion (optional mode)

For teams that require every possible SQL text to exist on disk for
audit, a per-query (or size-thresholded) option materializes all
reachable shapes into `.sql` files at generate time and dispatches to
them instead of composing. Default is hybrid composition. Queries
using `@filter-tree` (unbounded tree space) or `@in` on expanding
dialects (arity-dependent) cannot use this mode; their audit surface
is the closed vocabulary, the caps, and `sqletch explain`.

------------------------------------------------------------------------

# Verification Model — Soundness Sketch

Claim: under R1–R9 and runtime premises P1–P2, if the maximal query,
each `@choose` case and `@order-by` `@default` substitution, each
`@in` arity-0 form (expanding dialects), each `@filter-tree` empty
form (`TRUE`), and the minimal-shape structural checks pass, every
reachable shape parses, resolves, and type-checks.

-   *Parsing*: shapes are produced by deleting guarded items from
    list-shaped clauses (conjunctions, SET items, paired INSERT items,
    ORDER BY keys) or swapping closed cases at grammar slots of a valid
    AST. Deletion preserves syntactic validity because R6 guarantees
    clauses stay non-empty or are omitted whole, R7 keeps INSERT lists
    aligned, R1's node-completeness prevents regrouping, and the
    `DISTINCT ON`/`WITH TIES` restrictions keep order-sensitive or
    conditionally required ORDER BY clauses out of dynamic subsetting.
    (This restriction list is maintained per-construct as such
    couplings are identified, not assumed complete a priori — the
    conformance suite is the enforcement mechanism.)
-   *Identifier resolution*: R3 is resolution-based, so **every**
    reference (qualified or not) into an optional join's relation is
    guard-coupled; removing the join removes all such references with
    it. `@choose` cases, `@order-by` keys, and `@filter-tree`
    predicates cannot reference optional joins at all (empty guard
    sets). Removal only shrinks the candidate set for unqualified
    resolution, so a reference that resolved to the skeleton in the
    maximal query resolves identically in every shape. R8 confines
    case-defined names to their case.
-   *Typing*: removing an item from a list-shaped clause never changes
    the type of the remainder; parameter types are fragment-local, were
    checked in a verified rendering, and are pinned at bind time (P1),
    so no shape can re-infer them differently. `@choose` cases are each
    checked explicitly, and R8's non-interaction makes per-case checks
    sufficient without case combinations. Boolean combination of
    verified, individually parenthesized boolean predicates
    (`@filter-tree`) preserves typing and grouping.
-   *Result shape*: constant across shapes by R2. *Nullability*:
    constant because the analysis discipline (Phase 3) narrows only
    through the skeleton, optional joins cannot null-extend (R2), and
    `@choose` cases are unioned — i.e. constancy is **enforced by
    analysis rules**, not assumed.

The argument references only SQL's clause-list algebra, so it holds for
every dialect driver.

A property test backs the argument mechanically: enumerate all shapes
of every example and testdata template (up to a test-only cap; sampled
trees and arities for `@filter-tree`/`@in`) and both **prepare and
plan** (`EXPLAIN`) each against the dev DB of every supported dialect —
planning catches the planner-stage failures that prepare alone cannot.
Any counterexample is a compiler bug and becomes a permanent
regression test.

Known limits of prepare-level verification (documented, warned about at
compile time where detectable):

-   Per-shape constraint outcomes (e.g. omitting a `NOT NULL` insert
    column without a default) surface at execution time.
-   **Planner-stage errors are invisible to prepare/describe** (e.g.
    PostgreSQL's "FOR UPDATE cannot be applied to the nullable side of
    an outer join"). Known combinations are rejected statically
    (Phase 2); the catch-all for user queries is
    `sqletch check --exhaustive`, which EXPLAINs every enumerable shape
    on the dev DB.
-   Tier 2 dialects verify against author-supplied parameter types;
    wrong annotations produce wrong Go types, not silent corruption
    (the database still checks at execution).
-   Verification is against the dev schema at compile time; production
    drift is the same risk sqlc carries (a startup drift check is
    designed but not shipped; see Beyond v1.0).

------------------------------------------------------------------------

# Design Boundary

Explicit statements about what sqletch does **not** do, and what to do
instead. sqletch does not need to own 100% of an application's queries.

## Out of scope on principle (open state spaces)

No finite verification can cover these; use hand-written code (pgx,
database/sql, a builder) alongside sqletch in the same repository:

-   Dynamic table names over open sets: date-rolled partitions,
    per-tenant sharded tables. (Closed sets may become a `@choose`
    identifier slot in the future. Schema-per-tenant setups can often
    keep SQL static by switching `search_path`/database at the
    connection level instead.)
-   User-defined columns (runtime-created physical columns).
-   BI-style report builders: user-chosen dimensions, measures, and
    projections.
-   Statements the dialect cannot prepare: `COPY`, multi-statement
    batches, dynamic `LISTEN`/`NOTIFY` channel names, dynamic DDL.

## Deliberate stances (workarounds exist and are the better design)

-   **Shape-changing toggles** — "include heavy details column when
    asked", "return rows or an aggregate depending on a flag": write
    two queries. Constant result shape is what makes one typed row
    struct per query possible; two shapes are two queries. The split
    also keeps each query independently plannable and auditable.
-   **NULL-as-value filters** — presence Options reserve `None` for
    "absent"; "filter where column IS NULL" is expressed with `@when`,
    not by overloading the Option.
-   **Caller-supplied SQL fragments** — a use-case layer passing WHERE
    snippets as strings into a repository is rejected on principle:
    runtime SQL text is unverifiable. The supported form is a typed
    filter value over the query's closed predicate vocabulary
    (`@filter-tree`, Use Case 5) — same layering and flexibility,
    guarantees intact.
-   **Constructs inside subqueries/CTEs** — not supported (R1);
    conditional behavior belongs at the statement's top level, and a
    guarded conjunct may *contain* a subquery, which covers the common
    cases (`EXISTS`, `IN (SELECT …)`).
-   **Exact-text SQL allow-listing at a proxy**: use strict static
    expansion for queries with modest shape counts; for huge shape
    spaces, exact-text allow-listing is infeasible under any design
    that supports conditional SQL. `sqletch explain --enumerate`
    provides the audit surface.
-   **Transaction-pooling proxies**: nothing to configure — statements
    execute unnamed/ad-hoc, which is what such proxies require (see
    Runtime Model).

------------------------------------------------------------------------

# Error Reporting

-   All diagnostics reference the original template file and position
    via the scanner-owned fragment↔AST source maps built in Phase 1.
-   Errors from the dialect oracle are translated back to template
    positions the same way; dialect-specific fixes are suggested (e.g.
    PostgreSQL's undetermined-parameter-type error → "add an explicit
    cast `:param::type`").
-   Rule violations (R1–R9) explain the rule *and its rationale*, and
    suggest the compliant rewrite (see Rejected Examples).

------------------------------------------------------------------------

# CLI

``` text
sqletch generate     # compile templates, run type extraction, emit Go
sqletch check        # verify only (CI-friendly; offline on cache hit)
                     #   --exhaustive: EXPLAIN every enumerable shape
                     #                 (always needs the dev DB)
sqletch explain      # per query: guards, cases, vocabularies, shape count
                     #   --enumerate: print every reachable shape
                     #   --analyze:   EXPLAIN per shape on the dev DB
sqletch fmt          # canonical template formatting (inserts anchors,
                     #   normalizes construct layout); --check for CI
sqletch lsp          # language server over stdio; strictly offline
sqletch version      # release identification
```

Exit codes are 0 (success), 1 (diagnostics — something in the user's
templates or configuration), 2 (environment — database unreachable,
version mismatch, unreadable files). The split is deliberate for CI.

Configuration (`sqletch.yaml`): `dialect`, `server_version` (pinned;
part of the cache key), schema inputs (ordered `.sql` files/globs),
query file globs, output package/path, dev database strategy
(auto-managed ephemeral instance or user-supplied DSN), cache path
(committed to the repository), filter-tree caps, per-query static
expansion, and per-column nullability overrides. Unknown keys are
rejected. Parameter and result types on Tier 2 dialects come from
template annotations, not configuration.

------------------------------------------------------------------------

# Project Layout

``` text
cmd/sqletch/
internal/
    template/      # template scanner + per-dialect lexer profiles
    gosrc/         # //sqletch:query const extraction from .go inputs
    ast/           # shared view over dialect ASTs + source maps
    rules/         # structural rules R1–R9, guard/scope/case analysis
    shape/         # shape keys, enumeration, hashing
    nullability/   # skeleton-based catalog-constraint propagation
    codegen/       # Go code generation (dialect-parameterized)
    diagnostics/
    dialect/       # driver interface
        postgres/  # pg_query frontend + Describe oracle (Tier 1)
        mysql/     # TiDB parser frontend + COM_STMT_PREPARE oracle
        sqlite/    # rqlite/sql frontend + prepare/decltype oracle
    devdb/         # ephemeral dev-database lifecycle per dialect
    cache/         # committed oracle results + catalog snapshots
    lsp/           # language server (stdlib only; offline checker seam)
runtime/           # small public package imported by generated code
editors/           # TextMate injection grammar + VS Code extension,
                   #   tree-sitter grammar
examples/
testdata/          # shared dialect conformance suite
```

------------------------------------------------------------------------

# Testing Strategy

Unit tests

-   template scanner (per lexer profile), AST/source maps, structural
    rules, shape computation, placeholder numbering, guard/scope/case
    analysis, nullability propagation (including the
    skeleton-only-narrowing discipline).

Golden tests

-   template → generated Go (byte-exact),
-   template → enumerated SQL shapes (byte-exact), per dialect.

Integration tests

-   spin up a dev database per dialect, apply schema, run `generate`,
    compile the output, execute representative shapes.
-   cache round-trip: cold generate → warm offline check, including
    catalog-snapshot-dependent analyses.

Dialect conformance suite

-   the same `testdata/` templates run against every driver; a driver
    ships only when the suite passes (with its tier's annotation
    requirements applied).

Property tests

-   soundness check: every enumerable shape of every test template must
    **prepare and EXPLAIN** successfully on every dialect (sampled
    trees/arities for `@filter-tree`/`@in`); see Verification Model.

Regression tests

-   every previously fixed template becomes a permanent test case.

------------------------------------------------------------------------

# Stability and Beyond v1.0

Everything specified above is implemented and stable as of v1.0,
except §"Cross-Query Policies", which was specified after the freeze
(design/14) and ships in v1.1 as an additive extension. The
compatibility promises — what may change within v1 and what may not —
are stated in `manual/11-compatibility.md`: the template language, the
generated API, the `runtime` package, `sqletch.yaml`, the CLI surface,
and the *meanings* of diagnostic codes are fixed; diagnostic message
wording is not. The committed cache is self-describing, and an
unrecognized format degrades to a re-describe, never to a misread. The
pre-freeze audit behind those promises is `design/12-v1.md`.

Recorded, unscheduled, and none of it changes the verification model:

-   **Embedded PostgreSQL oracle backend** (WASM build of the real
    engine, or auto-fetched binaries as fallback): cold
    `generate`/`check` with no external database at all, the way SQLite
    already works. The spike is complete and the approach is feasible;
    shipping waits on upstream libpglite (`design/09-embedded-oracle.md`).
-   Native inference oracle backend, differential-tested against the
    accumulated `(schema, query, types)` corpus from caches and the
    conformance suite — pursued only where no embedded real engine
    exists (MySQL first) or where the corpus makes correctness
    demonstrable.
-   Lifting the local restrictions noted per construct (one
    `@filter-tree` per query; `@in` at depth-0 skeleton positions
    only). These are implementation limits, not model limits. (The
    `@filter-tree` WHERE-only slot restriction was lifted to
    WHERE/HAVING conjunct slots; the one-block-per-query limit
    remains, deferred alongside policy weaving, which covers the
    two-tree scope+criteria use case.)

Specified above and designed, but deliberately not shipped in v1.0:

-   `schema_setup_cmd` — delegating dev-database schema setup to a user
    command (goose/Atlas/Flyway) instead of applying `.sql` files.
-   An opt-in `prepared` statement-cache mode, alongside the shipped
    text-cache behavior.
-   A schema-drift check: generated code embedding the catalog
    fingerprint it was verified against, plus a startup helper that
    compares it with the live database's catalog — the same class of
    risk sqlc has, made observable.

------------------------------------------------------------------------

# Success Criteria

The project succeeds if it provides:

-   SQL as the primary authoring language.
-   Static verification of **every reachable query shape** at linear
    verification cost — no variant caps, no combinatorial explosion.
-   One ergonomic, fully typed Go function per query; no dispatcher
    surface, no per-variant API.
-   Zero runtime SQL parsing or string interpolation; runtime work is
    deterministic composition of verified constants plus a fixed
    connective vocabulary (P2).
-   Coverage of the everyday dynamic-SQL vocabulary on every supported
    dialect: optional filters, optional joins, partial updates,
    default-respecting inserts, selectable and multi-key ordering,
    variable-arity membership, and (closed-set) user-composed search
    conditions.
-   On-demand enumerability of shapes for audit (tree-/arity-valued
    constructs are audited via their closed vocabularies and caps),
    with an optional fully static expansion mode.
-   An RDBMS-agnostic core with pluggable dialect drivers; adding a
    database means writing a frontend and a type oracle, not forking
    the compiler.
-   Comfortable coexistence with existing sqlc-based projects — shared
    `DBTX`/transaction conventions — and with hand-written SQL for the
    deliberately out-of-scope cases.
