# The template language

A sqletch template is **plain SQL with a small, fixed vocabulary of
constructs attached at fixed grammatical positions**. It is not a text
template: there is no interpolation, no loops, no nesting, and no way
to reach the database with SQL text that the compiler has not already
verified. Everything outside a construct is the **skeleton** — emitted
byte-for-byte, never reformatted.

The design bargain: constructs are only allowed where deleting or
inserting a fragment provably cannot change the meaning of anything
around it. In exchange, sqletch verifies *every reachable shape* of the
query against a real database at compile time. When a template won't
compile, the diagnostic names the rule and the rewrite
([diagnostics reference](08-diagnostics.md)).

This chapter documents every construct, with a worked example for each.

## How to read this page

Most constructs are shown as three blocks:

1. **Template** — what you write.
2. **Go** — the API `sqletch generate` produces for it.
3. **Composed SQL** — what the database actually receives, for one or
   two representative *shapes*.

The composed SQL blocks are real output, taken from
`sqletch explain --enumerate`. Two consequences are visible in them and
worth knowing up front:

- Fragments that are switched off leave **blank lines** behind. The
  composer joins fragments with newlines, so an inactive fragment
  contributes nothing but its line break. This is deliberate:
  composition is byte-deterministic per shape, and blank lines are free.
- **Guarded predicates get parentheses.** `AND (u.status = $1)` — a
  fragment can never regroup its neighbours' meaning, whatever it
  contains.

Terminology used throughout:

- **shape** — one combination of construct choices (which guards are
  on, which `@choose` case is selected, …). Every reachable shape is
  statically verified.
- **slot** — a grammatical position where a construct is allowed. Slots
  are the reason verification stays linear in template size instead of
  exponential in shape count.
- **guard set** — the set of conditions under which a fragment is
  emitted. Used by rule R3 to decide which references are legal where.

## Contents

1. [Query headers](#1-query-headers)
2. [Parameters](#2-parameters)
3. [Where templates live](#3-where-templates-live)
4. [`@if-present` — optional fragments](#4-if-present--optional-fragments)
5. [`@when` — value-conditioned fragments](#5-when--value-conditioned-fragments)
6. [`@choose` — one of a closed set](#6-choose--one-of-a-closed-set)
7. [`@order-by` — multi-key sorting](#7-order-by--multi-key-sorting)
8. [`@filter-tree` — caller-composed boolean trees](#8-filter-tree--caller-composed-boolean-trees)
9. [`@in` — variable-arity membership](#9-in--variable-arity-membership)
10. [Slot reference](#10-slot-reference)
11. [The rules, informally](#11-the-rules-informally)
12. [What is deliberately not expressible](#12-what-is-deliberately-not-expressible)

---

## 1. Query headers

Every statement in a template starts with an sqlc-style header naming
the query and its result kind. Template files are listed in
`sqletch.yaml` under `queries:`.

**Template**

```sql
-- name: GetUserProfile :one
SELECT u.id, u.email, u.nickname, u.org_id
FROM users AS u
WHERE u.id = :id;
```

**Go**

```go
row, err := q.GetUserProfile(ctx, gen.GetUserProfileParams{ID: 42})
```

Result kinds — constant per query, never dynamic:

| Kind | Generated return | Notes |
|------|------------------|-------|
| `:one` | `(Row, error)` | `pgx.ErrNoRows` / `sql.ErrNoRows` when absent |
| `:many` | `([]Row, error)` | slice, possibly empty |
| `:exec` | `error` | no result columns |
| `:execrows` | `(int64, error)` | affected row count |

Rules:

- One statement per header (`SQLETCH005` otherwise), and a header
  before every statement (`SQLETCH003`).
- Query names are unique across all template files (`SQLETCH004`).
- The name maps to a Go identifier: `GetUserProfile` →
  `(*Queries).GetUserProfile`, `GetUserProfileParams`,
  `GetUserProfileRow`.
- SQL comments are legal anywhere the dialect allows them and are
  **preserved verbatim** in the composed SQL — including the `-- @param`
  and `-- @column` directives ([type annotations](03-annotations.md)).

---

## 2. Parameters

Parameters are always named: `:name`, snake_case. Writing `$1` or `?`
directly is an error (`SQLETCH011`) — sqletch owns placeholder
numbering, because it is what makes composition deterministic. A
PostgreSQL cast (`::text`) is not a parameter; the lexer distinguishes
the two.

### Required vs. optional (rule R9)

You never declare this — it follows from where the parameter binds:

| Where the parameter binds | Generated field |
|---------------------------|-----------------|
| anywhere outside a guard | **required**, plain type (`Limit int64`) |
| only inside fragments guarded by *itself* | **optional**, pointer (`Status *string`) — `nil` omits the fragment |
| only inside fragments guarded by *other* parameters | required; the value is simply unused when those fragments are off |
| only inside `@choose` cases / `@order-by` keys | required; unused when that case/key is not selected |
| only in a `@when` condition (never in SQL) | required, typed by the literal |

Two consequences worth internalising:

- A parameter that binds *both* inside and outside its own guard is
  required, so guarding on it would be vacuously true → `SQLETCH110`.
- Every `@if-present` guard parameter must bind somewhere inside the
  fragments it guards, otherwise its Go type is uninferable →
  `SQLETCH111`. Pure control parameters are `@when`'s job.

`nil` means *"omit this fragment"*, never *"match SQL NULL"*. To filter
for `IS NULL`, use [`@when`](#5-when--value-conditioned-fragments).

### Parameter types

On PostgreSQL the oracle types every parameter for you. On MySQL and
SQLite the protocol does not type parameter slots, so every bind
parameter needs a `-- @param name: type` directive; expression result
columns on SQLite need `-- @column`. The full rules — including what an
annotation means for an `@in` parameter per dialect — are in
[type annotations](03-annotations.md).

---

## 3. Where templates live

A `.sql` file is the default, and everything else in this chapter
describes it. A template may equally be authored **inside a Go file**,
in a const marked `//sqletch:query`:

```go
package repo

//sqletch:query
const searchUsersSQL = `
-- name: SearchUsers :many
SELECT u.id, u.email FROM users AS u
WHERE TRUE
@if-present(status)
  AND u.status = :status
@endif
;
`
```

List the Go files in `queries:` alongside (or instead of) `.sql`
globs — the input form is chosen by extension:

```yaml
queries: [queries/*.sql, internal/repo/*.go]
```

Nothing else changes. The const's contents are a template file: same
grammar, `-- name:` headers included, several queries per const
allowed. Generated code and cache entries are byte-identical to the
`.sql` form, so a query can move between the two without a cold
regenerate. Diagnostics point at the real `.go` line and column.

Three restrictions, each with a diagnostic:

- the marker goes on a **`const`** declaration, not a `var`
  (`SQLETCH021`) — the SQL that was verified must be the SQL that runs,
  and only a const makes that structural;
- the value is a single **raw** (backquoted) string literal, not an
  interpreted string and not a concatenation (`SQLETCH022`);
- conditionality still lives in the template's constructs. Building
  SQL with Go `if` statements or `fmt.Sprintf` is exactly what sqletch
  exists to replace, and the const requirement makes it impossible.

Extraction is syntactic (`go/parser` only, never `go/types`), so the
package does not have to compile — it may refer to symbols that
`sqletch generate` has not emitted yet. Templates embedded in Go do not
get syntax highlighting yet ([editor support](09-editors.md)); LSP
diagnostics work today.

---

## 4. `@if-present` — optional fragments

Emit a fragment iff **all** listed parameters are non-nil at runtime.
This is the workhorse construct: optional filters, optional scoping
joins, PATCH-style updates, cursor pagination.

```
@if-present(param, …)
  <one complete node>
@endif
```

### 4a. Optional WHERE conjunct

**Template**

```sql
-- name: GetUserProfile :one
SELECT u.id, u.email, u.nickname, u.org_id
FROM users AS u
WHERE u.id = :id

@if-present(status)
  AND u.status = :status
@endif
;
```

**Go**

```go
type GetUserProfileParams struct {
    ID     int64
    Status *string // nil omits the guarded fragment(s)
}

// without the filter
row, err := q.GetUserProfile(ctx, gen.GetUserProfileParams{ID: 42})

// with the filter
row, err := q.GetUserProfile(ctx, gen.GetUserProfileParams{
    ID:     42,
    Status: gen.Ptr("active"),
})
```

**Composed SQL** — `Status: nil` (shape `g=0`)

```sql
SELECT u.id, u.email, u.nickname, u.org_id
FROM users AS u
WHERE u.id = $1


;
```

**Composed SQL** — `Status: gen.Ptr("active")` (shape `g=1`)

```sql
SELECT u.id, u.email, u.nickname, u.org_id
FROM users AS u
WHERE u.id = $1


AND (u.status = $2)
;
```

Note that placeholder numbers are assigned per shape, in
first-occurrence order. The generated code binds the matching values;
you never see the numbers.

If *every* conjunct of a `WHERE` is optional, the clause needs an
unconditional anchor — write `WHERE TRUE` (rule R6, `SQLETCH113`;
`sqletch fmt` inserts it for you).

### 4b. Optional join

An optional join may filter, but may not contribute result columns
(rule R2), and must be `INNER` or `LEFT` (`RIGHT`/`FULL` would
null-extend skeleton relations and change nullability per shape →
`SQLETCH101`).

**Template**

```sql
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

LIMIT :limit;
```

**Go**

```go
type SearchUsersParams struct {
    OrganizationID *int64  // nil omits the guarded fragment(s)
    Status         *string // nil omits the guarded fragment(s)
    Limit          int64
}
```

**Composed SQL** — both guards on

```sql
SELECT
    u.id,
    u.email,
    u.status,
    u.created_at
FROM users AS u


JOIN organization_users AS ou
  ON ou.user_id = u.id
 AND ou.organization_id = $1

WHERE TRUE


AND (u.status = $2)

LIMIT $3;
```

**Anything that resolves into the optional relation must be guarded the
same way** (rule R3). This is checked on *resolved* references, so
unqualified columns are caught too:

```sql
@if-present(organization_id)
JOIN organization_users AS ou ON ou.user_id = u.id
@endif
WHERE TRUE
  AND organization_id = :organization_id   -- SQLETCH115: resolves to ou.organization_id
```

### 4c. Optional `SET` item (PATCH semantics)

**Template**

```sql
-- name: UpdateUserProfile :one
UPDATE users
SET
    updated_at = now()
@if-present(email)
  , email = :email
@endif
@if-present(nickname)
  , nickname = :nickname
@endif
WHERE id = :id
RETURNING id, email, nickname, updated_at;
```

**Go**

```go
row, err := q.UpdateUserProfile(ctx, gen.UpdateUserProfileParams{
    ID:    userID,
    Email: gen.Ptr("new@example.com"), // Nickname stays untouched
})
```

**Composed SQL** — `Email` present, `Nickname` absent

```sql
UPDATE users
SET
    updated_at = now()

, email = $1

WHERE id = $2
RETURNING id, email, nickname, updated_at;
```

Write the separator comma **inside** the guarded fragment, at the
start: sqletch owns the separator and needs to see the item as one
complete node (`SQLETCH008` otherwise). The `SET` clause cannot be
empty, so at least one item must be unconditional — the
`updated_at = now()` anchor above (rule R6, `SQLETCH118`).

`RETURNING` is part of the constant skeleton, so there is exactly one
row struct regardless of which columns were written.

### 4d. Optional `INSERT` column/value pair

A guarded column and its positionally paired value must carry
**identical guards** (rule R7, `SQLETCH119`). Omitting the pair lets
the column take its `DEFAULT`.

**Template**

```sql
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

**Composed SQL** — guard off / guard on

```sql
INSERT INTO users (
    email

) VALUES (
    $1

)
RETURNING id;
```

```sql
INSERT INTO users (
    email

, nickname
) VALUES (
    $1

, $2
)
RETURNING id;
```

Caveat: a `NOT NULL` column without a default that is omitted fails at
*execution* time — prepare-level verification cannot see per-shape
constraint outcomes. The compiler warns about this case
(`SQLETCH212`).

### 4e. Multiple guards, and why they don't nest

Blocks sharing the same guard parameters switch on and off together.
Guards **do not nest** (rule R5, `SQLETCH012`) — a fragment needing two
conditions takes a multi-parameter guard:

```sql
@if-present(min_score)
  @if-present(max_score)                        -- ERROR: guards do not nest
    AND t.score BETWEEN :min_score AND :max_score
  @endif
@endif

-- write instead:
@if-present(min_score, max_score)
  AND t.score BETWEEN :min_score AND :max_score
@endif
```

---

## 5. `@when` — value-conditioned fragments

Like `@if-present`, but the condition compares a **required** parameter
against a compile-time literal, evaluated in Go before any SQL is sent.

```
@when(param = literal)     -- also: != , <> (alias of !=)
  <one complete node>
@end
```

Literals are strings (`'…'`, with `''` escapes), integers, or booleans.
The literal fixes the parameter's Go type; the parameter does not need
to bind in SQL at all — this is the sanctioned *pure control parameter*.

**Template**

```sql
-- name: ListUsersSorted :many
SELECT u.id, u.email, u.created_at
FROM users AS u
WHERE TRUE
@when(include_banned = false)
  AND u.status != 'banned'
@end
LIMIT :limit;
```

**Go**

```go
type ListUsersSortedParams struct {
    Limit         int64
    IncludeBanned bool // @when control parameter
}
```

**Composed SQL** — `IncludeBanned: true` (condition false, fragment off)

```sql
SELECT u.id, u.email, u.created_at
FROM users AS u
WHERE TRUE

LIMIT $1;
```

**Composed SQL** — `IncludeBanned: false` (condition true, fragment on)

```sql
SELECT u.id, u.email, u.created_at
FROM users AS u
WHERE TRUE

AND (u.status != 'banned')
LIMIT $1;
```

This is also the idiomatic way to express *"filter where the column IS
NULL"*, which presence pointers cannot say:

```sql
@when(status_mode = 'null')
  AND u.status IS NULL
@end
@when(status_mode = 'active')
  AND u.status = 'active'
@end
```

Verification detail worth knowing: **all** `@when` fragments are
included in the maximal rendering, even mutually exclusive ones. That
is still sound for every reachable shape, but it means mutually
exclusive fragments must not collide structurally — e.g. two
`@when`-guarded joins need distinct aliases.

For rule R3, a value condition is a guard *atom* compared by exact
equality: `mode = 'a'` and `mode = 'b'` are unrelated atoms, and
neither is related to `mode != 'a'`.

---

## 6. `@choose` — one of a closed set

Select exactly one case from a compile-time-closed set. Each case is
verified independently, so cost is linear in the number of cases.

```
@choose(param)
@case(name)
  <body>
@case(other)
  <body>
@default
  <body>          -- optional; may be empty
@end
```

### 6a. Sort orders

**Template**

```sql
-- name: SearchUsers :many
SELECT u.id, u.email, u.status, u.created_at
FROM users AS u
WHERE TRUE
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

**Go** — a generated enum type

```go
type SearchUsersSort int

const (
    SearchUsersSortDefault SearchUsersSort = iota
    SearchUsersSortCreatedAtDesc
    SearchUsersSortCreatedAtAsc
    SearchUsersSortEmailAsc
)

type SearchUsersParams struct {
    Limit int64
    Sort  SearchUsersSort // zero value selects @default
}

rows, err := q.SearchUsers(ctx, gen.SearchUsersParams{
    Sort:  gen.SearchUsersSortEmailAsc,
    Limit: 50,
})
```

**Composed SQL** — `SearchUsersSortEmailAsc` vs. zero value

```sql
… ORDER BY u.email ASC, u.id ASC
```

```sql
… ORDER BY u.id ASC
```

**Without `@default`**, the parameter is required: the generated enum
reserves the zero value as invalid, and passing it makes the function
return `runtime.ErrChooseRequired` before touching the database.

```go
const (
    _ SignupsByBucketBucket = iota // zero value is invalid: no @default declared
    SignupsByBucketBucketDaily
    …
)
```

### 6b. Projections and `GROUP BY`

`@choose` may also drive a projection expression and `GROUP BY`,
provided every case yields the **same column alias and type** (rule
R2); nullability is unioned across cases, the nullable-most winning.

**Template**

```sql
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

**Go**

```go
rows, err := q.SignupsByBucket(ctx, gen.SignupsByBucketParams{
    Bucket: gen.SignupsByBucketBucketWeekly, // required: no @default
    Since:  monthStart,
})
```

**Composed SQL** — `…Weekly`

```sql
SELECT

date_trunc('week',  u.created_at) AS bucket,
    count(*) AS signups
FROM users AS u
WHERE u.created_at >= $1
GROUP BY bucket
ORDER BY bucket;
```

The alias `bucket` is defined in the constant skeleton, which is why
`GROUP BY bucket` and `ORDER BY bucket` may refer to it (rule R8).

### Constraints

- All cases of one `@choose` must target the same clause — mixing
  `ORDER BY` and `GROUP BY` bodies is an error, and the `@default` body
  must start with the same clause keyword.
- Case bodies have an **empty guard set**: they may not reference
  relations introduced by optional joins (rule R3).
- Names defined inside a case are local to it (rule R8) — this is what
  makes distinct `@choose` parameters non-interacting, and per-case
  verification sufficient.
- The same `@choose` parameter may drive several slots at once; the
  selected case applies to all of them.
- At most one dynamic construct per `ORDER BY` clause.
- `@default` must be last.

---

## 7. `@order-by` — multi-key sorting

Data-grid sorting: any subset of a closed key set, in any order, each
ascending or descending — without `@choose`'s factorial case count.
Each key expression is verified once. One block declares at most **64
keys** (SQLETCH010); `@choose` allows at most **255 cases** counting
`@default`. Both are fixed limits of the shape key's encoding, well
past any hand-written query.

```
@order-by(param)
@key(name)
  <sort expression>       -- no ORDER BY keyword, no ASC/DESC
@key(other)
  <sort expression>
@default
  ORDER BY …              -- optional; used when the list is empty
@end
```

**Template**

```sql
-- name: ListUsersSorted :many
SELECT u.id, u.email, u.created_at
FROM users AS u
WHERE TRUE
@order-by(sort)
@key(created_at)
u.created_at
@key(email)
u.email
@default
ORDER BY u.id ASC
@end
LIMIT :limit;
```

**Go** — one enum constant per key *and direction*

```go
type ListUsersSortedSortKey int

const (
    ListUsersSortedSortCreatedAtAsc  ListUsersSortedSortKey = 0
    ListUsersSortedSortCreatedAtDesc ListUsersSortedSortKey = 1
    ListUsersSortedSortEmailAsc      ListUsersSortedSortKey = 2
    ListUsersSortedSortEmailDesc     ListUsersSortedSortKey = 3
)

type ListUsersSortedParams struct {
    Limit int64
    Sort  []ListUsersSortedSortKey // key sequence; empty = @default
}

rows, err := q.ListUsersSorted(ctx, gen.ListUsersSortedParams{
    Sort: []gen.ListUsersSortedSortKey{
        gen.ListUsersSortedSortEmailAsc,
        gen.ListUsersSortedSortCreatedAtDesc,
    },
    Limit: 20,
})
```

**Composed SQL** — the sequence above

```sql
SELECT u.id, u.email, u.created_at
FROM users AS u
WHERE TRUE

ORDER BY u.email, u.created_at DESC
LIMIT $1;
```

**Composed SQL** — empty `Sort` (the `@default` body)

```sql
SELECT u.id, u.email, u.created_at
FROM users AS u
WHERE TRUE

ORDER BY u.id ASC
LIMIT $1;
```

The caller's key order is honoured — the one sanctioned reordering in
the whole composition model. A key selected twice (in either direction)
returns `runtime.ErrOrderKey` before any SQL is sent.

### Constraints

- Statement-level `ORDER BY` only.
- Key expressions have an empty guard set (rule R3).
- Without `@default`, an empty list omits the clause entirely.
- Forbidden under PostgreSQL's `DISTINCT ON` (`SQLETCH122`): that makes
  `ORDER BY` validity prefix-order-sensitive, breaking the
  subset/permutation argument. Use `@choose` there — every case is
  verified whole.
- `FETCH FIRST … WITH TIES` makes `ORDER BY` mandatory, so a `@default`
  is required (`SQLETCH123`).

---

## 8. `@filter-tree` — caller-composed boolean trees

For advanced-search UIs and for passing filters across layer
boundaries: the caller combines a **closed predicate vocabulary** with
`And`/`Or` to arbitrary depth. What crosses the boundary is a typed
value, never SQL text.

```
WHERE TRUE
  AND @filter-tree(param)        -- or @filter-tree!(param) for required mode
@predicate(name)
  <boolean expression>
@predicate(other)
  <boolean expression>
@end
```

**Template**

```sql
-- name: ListOrders :many
SELECT o.id, o.total
FROM orders AS o
WHERE TRUE
  AND @filter-tree!(scope)
@predicate(tenant)
o.tenant_id = :tenant_id
@predicate(status_eq)
o.status = :status
@predicate(created_in)
o.created_at >= :from AND o.created_at < :to
@end
ORDER BY o.id DESC;
```

**Go** — one constructor per predicate, plus `And` / `Or` / `Unscoped`

```go
func ListOrdersTenant(tenantID int64) runtime.Tree
func ListOrdersStatusEq(status string) runtime.Tree
func ListOrdersCreatedIn(from, to time.Time) runtime.Tree
func ListOrdersUnscoped() runtime.Tree   // renders TRUE — the explicit opt-out

// The tree is an argument of a value type, so both ways of forgetting
// it are refused by the compiler: omitting it is "not enough arguments",
// and `nil` is not a runtime.Tree.
func (q *Queries) ListOrders(
    ctx context.Context,
    scope runtime.Tree,           // @filter-tree!; ListOrdersUnscoped() opts out
    arg ListOrdersParams,
) ([]ListOrdersRow, error)
```

The one zero that still compiles is `runtime.Tree{}`, written out in
full — not something a caller produces by accident. It is refused at
runtime with `ErrFilterRequired`. `Unscoped()` remains how you say "no
scope" deliberately, and the two are distinguishable: `Tree{}` means
the caller did not decide.

```go
// A repository that does not hard-code its filtering:
func (r *OrderRepo) List(ctx context.Context, scope *runtime.Tree) ([]gen.ListOrdersRow, error) {
    return r.q.ListOrders(ctx, scope, gen.ListOrdersParams{})
}

// Callers decide applicability and combination:
repo.List(ctx, gen.ListOrdersTenant(tenantID))
repo.List(ctx, gen.And(
    gen.ListOrdersTenant(tenantID),
    gen.Or(
        gen.ListOrdersStatusEq("active"),
        gen.ListOrdersCreatedIn(from, to),
    ),
))
repo.List(ctx, gen.ListOrdersUnscoped())
```

**Composed SQL** — the nested `And`/`Or` above

```sql
SELECT o.id, o.total
FROM orders AS o
WHERE TRUE
  AND ((o.tenant_id = $1) AND ((o.status = $2) OR (o.created_at >= $3 AND o.created_at < $4)))
ORDER BY o.id DESC;
```

**Composed SQL** — a single predicate / `Unscoped()`

```sql
  AND (o.tenant_id = $1)
```

```sql
  AND TRUE
```

Every predicate **and every subtree** is parenthesised, so a predicate
containing a top-level `OR` can never change its neighbours' meaning.
Because each predicate is a verified boolean fragment and AND/OR
preserve type and safety, *any* runtime tree over the closed set is
sound. Predicate parameters are constructor arguments, never params
struct fields; values ride as binds, and positions come from cached
bind *plans*.

### Required mode: `@filter-tree!`

With the `!`, a `nil` tree returns `runtime.ErrFilterRequired` before
any SQL is sent, and deliberately unfiltered access must be spelled
with the generated `…Unscoped()` constructor. This exists for
multi-tenant robustness: *forgetting* the filter is an error, and
*opting out* is one greppable, reviewable line at the call site.
Without the `!`, an empty or nil tree simply renders `TRUE`.

Every example project carries a runnable `FilterUsers` query in this
mode — see [`examples/sqlite/`](../../examples/sqlite/) for the
no-setup version (`go run .`), and `examples/postgres/`,
`examples/mysql/` for the same query per dialect.

### Constraints

- One `@filter-tree` per query (an implementation restriction, not a
  model limit). It occupies a **WHERE or HAVING conjunct slot** and
  must be one whole conjunct: written directly after an unconditional
  `AND`, and after `@end` the statement continues with `AND`, a clause
  keyword, or the end of the statement (`SQLETCH008` otherwise). The
  compiler additionally verifies the empty-tree rendering (`TRUE`) is
  exactly one top-level conjunct (`SQLETCH102`) — under `OR`, `NOT`,
  or an expression continuation the `TRUE` fallback would silently
  disarm or change the filter.
- Predicates have an empty guard set (rule R3).
- The same predicate may appear several times in one tree with
  independent bindings; placeholders are numbered per occurrence.
- The runtime enforces tree caps — default 32 nodes, depth 8,
  configurable via `filter_tree_caps` in `sqletch.yaml` and baked into
  the generated code. Exceeding them returns `runtime.ErrTreeTooLarge`.
- Queries using `@filter-tree` cannot use strict static expansion (the
  tree space is unbounded); their audit surface is the predicate
  vocabulary, the caps, and `sqletch explain`.

---

## 9. `@in` — variable-arity membership

Membership against a caller-supplied slice.

```sql
WHERE TRUE
  AND u.status @in(:statuses)
```

**Template**

```sql
-- name: UsersInStatuses :many
-- @param statuses: varchar(16)
SELECT u.id, u.email FROM users AS u
WHERE u.tenant_id = :tenant_id
  AND u.status @in(:statuses)
ORDER BY u.id
LIMIT :limit;
```

**Go**

```go
rows, err := q.UsersInStatuses(ctx, gen.UsersInStatusesParams{
    TenantID: 1,
    Statuses: []string{"active", "invited"},
    Limit:    50,
})
```

**Composed SQL — PostgreSQL** (one static shape, no arity dimension)

```sql
-- @param statuses: varchar(16)
SELECT u.id, u.email FROM users AS u
WHERE u.tenant_id = $1
  AND u.status = ANY($2)
ORDER BY u.id
LIMIT $3;
```

**Composed SQL — MySQL** (arity-expanded; arity is part of the shape key)

```sql
-- @param statuses: varchar(16)
SELECT u.id, u.email FROM users AS u
WHERE u.tenant_id = ?
  AND u.status IN (?, ?)
ORDER BY u.id
LIMIT ?;
```

**Composed SQL — MySQL, empty slice**

```sql
  AND u.status IN (SELECT NULL FROM DUAL WHERE FALSE)
```

The empty form is `FALSE` even for a `NULL` operand, matching
PostgreSQL's `= ANY('{}')` exactly. (SQLite spells the same thing
`IN (SELECT NULL WHERE 0)`.)

### Typing the parameter

The `-- @param` annotation means different things per dialect:

| Dialect | Annotation | Meaning |
|---------|-----------|---------|
| PostgreSQL | optional | the **array** type (`text[]`, `bigint[]`, …). Usually omit it and let the oracle infer. |
| MySQL / SQLite | **mandatory** | the **element** type (`varchar(16)`, `bigint`, …); the Go field becomes a slice of it. |

Annotating an `@in` parameter with a *scalar* type on PostgreSQL is a
compile error — the oracle infers an array type from `= ANY($n)`, and
the annotation disagrees:

```
queries/q.sql:2:1: error[SQLETCH213]: `-- @param statuses: varchar(16)` types the
parameter as varchar, but the oracle infers _text from the query; binding at an
unverified type would break the compile-time type guarantee
help: remove the annotation (postgres infers parameter types), or correct it to
`-- @param statuses: text[]`
```

### Three-valued-logic caveat

Uniform across dialects and identical to hand-written SQL: for a `NULL`
operand, a non-empty list yields `NULL` while an empty list yields
`FALSE`. Under negation these differ — the row is kept at arity ≥ 1 and
dropped at arity 0.

### Constraints

- Depth-0 `WHERE`/`HAVING` positions only.
- **Not allowed inside guarded fragment bodies** (`SQLETCH007`). On
  PostgreSQL, write `= ANY(:param)` directly there instead.
- On expanding dialects, queries using `@in` cannot use strict static
  expansion.

---

## 10. Slot reference

A construct is legal only where the compositional argument holds. Every
placement is validated against the dialect's parsed AST, and each
fragment must be **exactly one complete node** in its slot — one
conjunct, one join item, one `SET` item — so that deleting a neighbour
can never regroup its meaning (rule R1).

| Slot | `@if-present` | `@when` | `@choose` | `@order-by` | `@filter-tree` | `@in` |
|------|:---:|:---:|:---:|:---:|:---:|:---:|
| `WHERE` conjunct | ✓ | ✓ | — | — | ✓ (one, after `AND`) | ✓ (expression) |
| `HAVING` conjunct | ✓ | ✓ | — | — | ✓ (one, after `AND`) | ✓ (expression) |
| `FROM` join item (`INNER`/`LEFT`, filter-only) | ✓ | ✓ | — | — | — | — |
| `SET` item (`UPDATE`) | ✓ | ✓ | — | — | — | — |
| `INSERT` column + paired `VALUES` item | ✓ | ✓ | — | — | — | — |
| projection expression | — | — | ✓ | — | — | — |
| `GROUP BY` | — | — | ✓ | — | — | — |
| statement-level `ORDER BY` | — | — | ✓ | ✓ | — | — |

Everywhere else is an error. In particular, constructs may **never**
appear inside subqueries, CTEs, derived tables, `OVER (…)` windows, or
an aggregate's internal `ORDER BY` — although a guarded fragment may
itself *contain* a subquery.

Anchoring (rule R6):

- Clauses that are optional as a whole (`WHERE`, `HAVING`, `ORDER BY`)
  simply vanish when every item is inactive — sqletch owns the clause
  keyword. Convention when every conjunct is optional: write
  `WHERE TRUE`.
- Clauses that must be non-empty (`SET`, the `INSERT` column list) need
  at least one unconditional item.

---

## 11. The rules, informally

- **One complete node per fragment** (R1): a fragment can't regroup its
  neighbours' meaning, and constructs stay at the statement's top level.
- **Constant result shape** (R2): optional parts never add or remove
  result columns (no `SELECT *` over an optional join; optional joins
  are INNER/LEFT only).
- **No dangling references** (R3): referring to an optional join's
  columns requires sharing its guard.
- **Closed choice** (R4): cases, keys, and predicates are fixed at
  compile time — nothing arbitrary can be injected through them.
- **No nesting** (R5), **anchors** (R6), **INSERT pairing** (R7),
  **no name definitions in cases** (R8), **guards must matter** (R9 —
  a guard on a required parameter, or a control parameter that also
  binds, is an error).

The full statement of the rules with their rationale, plus the
soundness argument they support, is in
[`docs/spec.md`](../spec.md); the diagnostics quote them when violated.

---

## 12. What is deliberately not expressible

Free-form SQL splicing, dynamic identifiers (table/column names from
values), guarded fragments inside subqueries, arbitrary reordering,
shape-changing projections. If a query's dynamism doesn't fit the
vocabulary, write two queries — each fully verified — and choose in Go.
The boundary is the product: everything inside it is verified; nothing
outside it can be smuggled in at runtime. The reasoning behind each
exclusion, and the recommended alternative, is the *Design Boundary*
section of [`docs/spec.md`](../spec.md).

---

## See also

- [Type annotations](03-annotations.md) — `-- @param` / `-- @column`.
- [Dialects](04-dialects.md) — what each dialect infers for you, and
  how each renders `@in`.
- [CLI reference](06-cli.md) — `sqletch explain --enumerate` is the
  fastest way to answer *"what does this template actually run?"*, and
  is where every composed-SQL block on this page came from.
- [Diagnostics reference](08-diagnostics.md) — every `SQLETCHnnn` code.
- [`examples/`](../../examples/) — complete working projects, one per
  dialect; their generated code and caches are committed, so they build
  with no database.
